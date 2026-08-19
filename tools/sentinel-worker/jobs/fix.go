// jobs/fix.go is the FIX engine's runner (plan §4.4, N8f "fix-pr-resume-caps"): it dispatches an
// enqueued FIX job through PrepareFixWorkspace -> RunFixExecutor -> ValidateFix -> CreateFixPR ->
// JournalFixPROpen, gated by FixCaps and the project's repo connection (settings.ProjectSettings.
// FixReady). This is the ONLY call path from main() that reaches JournalFixPROpen -- wiring it is
// what makes sweep.go's hasOpenFix actually flip true at runtime, and what makes the plan §4.4
// on-job-end artifact bundle (agent log, final diff, TASK.md, PROGRESS.md, validation result)
// actually get written to fix-artifacts/<jobId>/ (see SaveJobArtifacts).
//
// Live resume-save (plan §4.4 step 3b's "whenever PROGRESS.md grows (debounced) + on SIGTERM") IS
// wired: RunFix drives a ResumeDebouncer off the Fix Executor's OnProgressLine callback and installs
// a SIGTERM listener for the duration of the attempt, both calling SaveResumeState against r.Sink.
// That makes SaveResumeState/ResumeDebouncer real runtime code, not test-only.
//
// What is NOT wired: an automatic, worker-restart-time TRIGGER that finds a FIX job left mid-attempt
// and calls ResumeFix below on its own. That requires a FIX-specific in-flight journal state (no
// such state exists yet -- state.Journal's RecoveryScan today only recognizes StateAdvised/
// StateActing, which are TRIAGE/FOLLOW-UP concepts) and main.go wiring to drive it, same shape as
// runner.Resume's TRIAGE/FOLLOW-UP replay. That is a documented, deferred seam (plan §4.4 step 3b's
// "resume after any restart" -- the *mechanics* ship here, the *automatic trigger* on process
// restart does not).
//
// ResumeFix IS reachable today, though: it is an explicit, exported, fully-tested entry point
// (LoadResumeState -> ResumeFixWorkspace -> ContinuationBrief -> re-invoke executor -> the same
// validate/PR/finish tail RunFix uses) that a future boot-time scan can call once the journal state
// above exists. It correctly does not call FixCaps.AllowJobStart or RecordAttempt (CLAUDE.md: "a
// crash-resume of the same job does NOT count again").
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// DefaultResumeDebounceInterval is how often a growing PROGRESS.md may trigger a new live
// resume-state save (plan §4.4 step 3b: "debounced") when FixRunner.ResumeDebounceInterval is unset.
const DefaultResumeDebounceInterval = 20 * time.Second

// FixJobInput is what RealActor.Act hands to Fixer.Dispatch once compiled.EnqueueFix is true --
// everything the FIX engine needs from the issue Act already fetched (GetIssue), so RunFix never
// needs a second read of it just to learn errorClass/occurrences.
type FixJobInput struct {
	JobID       string
	IssueID     string
	IssueURL    string
	ProjectID   string
	ErrorClass  string
	FixBrief    string
	Occurrences string
	ToolOutputs []string
	TriggerSeq  int64
}

// Fixer is RealActor's seam into the FIX engine. Kept as a narrow interface (rather than RealActor
// importing gitprovider/os/exec directly) matching the ProjectFixSettings/Sender pattern already
// used in this package. Dispatch MUST NOT block its caller for the FIX run's full duration -- a
// real implementation (main.go's wiring) runs RunFix on its own goroutine with a detached context
// and its own timeout, and must recover from panics: a wedged or panicking FIX attempt must never
// take down the poll loop that called Act.
type Fixer interface {
	Dispatch(in FixJobInput)
}

// FixRepoConfig is the narrow per-project repo config RunFix needs, resolved by main.go's wiring
// from settings.Store (C15's repo connection + C16's matching git credential).
type FixRepoConfig struct {
	Provider      gitprovider.Provider
	Repo          gitprovider.RepoRef
	CloneURL      string
	DefaultBranch string
	TestCmd       string
	CloneDepth    int
}

// FixRepoResolver resolves projectID's FIX repo config. found=false means no repo connection
// exists for the project (plan §4.4: "no repo connection => propose-only").
type FixRepoResolver func(projectID string) (FixRepoConfig, bool, error)

// FixRunner wires every seam RunFix needs. Built once in main.go, gated on
// WORKER_FIX_ENABLED/WORKER_EXECUTE/FIX_EXECUTOR_CMD, and handed to RealActor as its Fixer.
type FixRunner struct {
	WorkspaceRoot string // $WORKER_WORKSPACE_DIR
	StateDir      string // for agent-logs/<jobId>.log (state.AgentLogPath)
	Journal       *state.Journal
	Client        Sender
	Sink          ArtifactSink
	Caps          *FixCaps
	ResolveRepo   FixRepoResolver

	ExecutorCmd string
	ExecutorEnv map[string]string
	MaxFiles    int
	Timeout     time.Duration
	KeepFailed  bool

	// ResumeDebounceInterval throttles how often a growing PROGRESS.md triggers a live resume-state
	// save (plan §4.4 step 3b). Defaults to DefaultResumeDebounceInterval when <= 0.
	ResumeDebounceInterval time.Duration

	Secrets []string // configured secret values to redact from logs/PR body (guard.Check's gate)

	// OnEvent, when non-nil, is called for every notable outcome ("dispatched", "skipped:<reason>",
	// "failed:<reason>", "pr-opened", "error") so main.go can log/metric without this package
	// importing log/slog. Never called with a nil in-flight struct.
	OnEvent func(event string, in FixJobInput, detail string)
}

func (r *FixRunner) emit(event string, in FixJobInput, detail string) {
	if r.OnEvent != nil {
		r.OnEvent(event, in, detail)
	}
}

func (r *FixRunner) resumeDebounceInterval() time.Duration {
	if r.ResumeDebounceInterval > 0 {
		return r.ResumeDebounceInterval
	}
	return DefaultResumeDebounceInterval
}

// watchLiveResumeSave installs the plan §4.4 step-3b live-save triggers for one attempt: a
// debounced save on every PROGRESS.md-growth line (returned as the FixExecutorInput.OnProgressLine
// callback to wire) and an unconditional final save on SIGTERM. The returned stop func MUST be
// deferred by the caller -- it unregisters the SIGTERM listener and stops its goroutine; it does
// NOT itself save (SIGTERM already covers "about to die", and a normal exit's state is superseded
// by SaveJobArtifacts' end-of-job bundle).
func (r *FixRunner) watchLiveResumeSave(ctx context.Context, ws *FixWorkspace, in FixJobInput, cred gitprovider.GitCredential, redactor *gitprovider.Redactor) (onProgressLine func(string), stop func()) {
	save := func() {
		if r.Sink == nil {
			return
		}
		if err := SaveResumeState(ctx, r.Sink, ws, cred, redactor); err != nil {
			r.emit("error:save-resume-state", in, err.Error())
		}
	}

	debouncer := &ResumeDebouncer{Interval: r.resumeDebounceInterval()}
	onProgressLine = func(string) {
		if debouncer.ShouldSave() {
			save()
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-sigCh:
			save() // unconditional final save -- plan §4.4 step 3b: "... + on SIGTERM"
		case <-done:
		}
	}()
	stop = func() {
		signal.Stop(sigCh)
		close(done)
	}
	return onProgressLine, stop
}

// Dispatch implements Fixer: it runs RunFix on its own goroutine with a context detached from
// whatever ctx Act was called with (Act's ctx is cancelled the moment Act returns, long before a
// 30-minute FIX attempt could ever finish), bounded by r.Timeout, and recovers from a panic inside
// RunFix so a coding-agent-triggered crash never propagates back into the poll loop.
func (r *FixRunner) Dispatch(in FixJobInput) {
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				r.emit("panic", in, fmt.Sprintf("%v", rec))
			}
		}()
		timeout := r.Timeout
		if timeout <= 0 {
			timeout = DefaultFixTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Minute) // headroom over the executor's own timeout for validate+push+PR
		defer cancel()
		if err := r.RunFix(ctx, in); err != nil {
			r.emit("error", in, err.Error())
		}
	}()
}

// RunFix implements plan §4.4 steps 1-5 for one FIX job: caps -> repo resolution (no connection =>
// propose-only) -> workspace -> executor -> validation -> PR (on pass) or comment+release (on
// fail) -> JournalFixPROpen (on pass) -> SaveJobArtifacts (always). It is synchronous and blocking
// -- callers that must not block (RealActor.Act) go through Dispatch instead.
func (r *FixRunner) RunFix(ctx context.Context, in FixJobInput) error {
	if r.Journal == nil || r.Client == nil {
		return fmt.Errorf("jobs: RunFix: Journal and Client are required")
	}
	if r.ResolveRepo == nil {
		return fmt.Errorf("jobs: RunFix: ResolveRepo is required")
	}

	repo, found, err := r.ResolveRepo(in.ProjectID)
	if err != nil {
		return fmt.Errorf("jobs: RunFix: resolving repo for project %s: %w", in.ProjectID, err)
	}
	if !found {
		// plan §4.4: "No repo connection ⇒ propose-only (diagnosis comment from the Fix Brief, no
		// workspace)." Claim is released -- there is no in-flight FIX to keep it open for.
		r.emit("skipped:no-repo-connection", in, "")
		return r.postProposeOnly(ctx, in)
	}

	if r.Caps != nil {
		// AllowAttempt is checked BEFORE AllowJobStart (finding 6): an attempt-exhausted jobID must
		// never consume a WORKER_MAX_FIX_JOBS_PER_DAY slot on re-dispatch just to be turned away by
		// the attempts gate a moment later -- that would let a poisoned/re-delivered job silently
		// eat into the daily budget other, legitimate jobs need.
		if !r.Caps.AllowAttempt(in.JobID) {
			r.emit("skipped:fix-attempts", in, "")
			return r.releaseWithComment(ctx, in, "🤖 This issue has exhausted its FIX attempt budget without a passing attempt; a human should take it from here.")
		}
		if !r.Caps.AllowJobStart() {
			r.emit("skipped:fix-jobs-per-day", in, "")
			return r.releaseWithComment(ctx, in, "🤖 The daily FIX job cap has been reached; a human should look at this issue directly for now.")
		}
		r.Caps.RecordAttempt(in.JobID)
	}

	redactor := gitprovider.NewRedactor(nil)
	redactor.AddSecrets(r.Secrets...)

	brief := TaskBrief{
		IssueID:     in.IssueID,
		IssueURL:    in.IssueURL,
		IssueTitle:  in.ErrorClass,
		Occurrences: in.Occurrences,
		FixBrief:    in.FixBrief,
		TestCmd:     repo.TestCmd,
	}

	ws, err := PrepareFixWorkspace(ctx, PrepareFixWorkspaceInput{
		WorkspaceRoot: r.WorkspaceRoot,
		JobID:         in.JobID,
		IssueID:       in.IssueID,
		CloneURL:      repo.CloneURL,
		DefaultBranch: repo.DefaultBranch,
		CloneDepth:    repo.CloneDepth,
		Cred:          repo.Provider.Auth(),
		Redactor:      redactor,
		Brief:         brief,
	})
	if err != nil {
		r.emit("error:workspace", in, err.Error())
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not prepare a FIX workspace: %s", sanitizeForComment(err.Error())))
	}

	return r.executeValidatePublish(ctx, ws, in, repo, redactor)
}

// executeValidatePublish is RunFix/ResumeFix's shared tail from plan §4.4 steps 3-5 once a
// workspace exists (fresh OR resumed): run the Fix Executor (with the live resume-save wired to
// its progress callback and a SIGTERM listener for the duration) -> validate -> build+push the PR
// -> journal it open -> SaveJobArtifacts. Every exit path calls r.finish exactly once.
func (r *FixRunner) executeValidatePublish(ctx context.Context, ws *FixWorkspace, in FixJobInput, repo FixRepoConfig, redactor *gitprovider.Redactor) error {
	// Journal the plan §4.4 step-3b in-flight FIX marker (finding 4) BEFORE the executor ever runs:
	// this is the record main.go's boot-time recovery scan looks for to drive a crashed attempt
	// through ResumeFix instead of silently orphaning it (the claim stays held forever with nothing
	// ever resuming the job). Best-effort: a journal write failure here must not abort an otherwise
	// runnable attempt -- it only degrades resumability, matching every other r.emit("error:...")
	// journal-write failure in this function.
	if err := journalFixRunning(r.Journal, in, ws.BaseCommit); err != nil {
		r.emit("error:journal-fix-running", in, err.Error())
	}

	var logBuf bytes.Buffer
	logRedactor := gitprovider.NewRedactor(&logBuf)
	logRedactor.AddSecrets(r.Secrets...)

	saveRedactor := gitprovider.NewRedactor(nil)
	saveRedactor.AddSecrets(r.Secrets...)
	onProgressLine, stopResumeSave := r.watchLiveResumeSave(ctx, ws, in, repo.Provider.Auth(), saveRedactor)
	defer stopResumeSave()

	execResult := RunFixExecutor(ctx, FixExecutorInput{
		Cmd:            r.ExecutorCmd,
		RepoDir:        ws.RepoDir,
		Timeout:        r.Timeout,
		TaskPath:       ws.TaskPath,
		ProgressPath:   ws.ProgressPath,
		JobID:          ws.JobID,
		ExtraEnv:       r.ExecutorEnv,
		LogWriter:      logRedactor,
		OnProgressLine: onProgressLine,
	})
	if execResult.Err != nil {
		r.finish(ctx, ws, in, FixValidationResult{Reason: FixValidReasonNone, Detail: execResult.Err.Error()}, logBuf.Bytes(), false)
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 The Fix Executor failed to run: %s", sanitizeForComment(execResult.Err.Error())))
	}

	valResult, err := ValidateFix(ctx, ValidateFixInput{
		RepoDir:    ws.RepoDir,
		BaseCommit: ws.BaseCommit,
		TestCmd:    repo.TestCmd,
		MaxFiles:   r.MaxFiles,
		Cred:       repo.Provider.Auth(),
		Redactor:   redactor,
	})
	if err != nil {
		r.finish(ctx, ws, in, FixValidationResult{Reason: FixValidReasonNone, Detail: err.Error()}, logBuf.Bytes(), false)
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not validate the FIX attempt: %s", sanitizeForComment(err.Error())))
	}

	if !valResult.Passed {
		r.finish(ctx, ws, in, valResult, logBuf.Bytes(), false)
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 A FIX attempt did not pass validation (%s): %s", valResult.Reason, sanitizeForComment(valResult.Detail)))
	}

	// Commit BEFORE push (finding 2): ValidateFix only ever inspected the working tree/index against
	// ws.BaseCommit -- it never itself commits. Without an explicit commit here, a passing validation
	// gave no guarantee that anything actually lands on the fix branch that gets pushed a few lines
	// below: a caller could push a branch whose tip is STILL ws.BaseCommit, opening an empty PR.
	// changed=false is treated exactly like a failed validation -- fail the attempt as empty-diff
	// rather than push a no-op branch.
	commitSHA, changed, err := CommitFixChanges(ctx, ws.RepoDir, ws.BaseCommit, repo.Provider.Auth(), redactor)
	if err != nil {
		r.finish(ctx, ws, in, valResult, logBuf.Bytes(), false)
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not commit the FIX attempt: %s", sanitizeForComment(err.Error())))
	}
	if !changed || commitSHA == ws.BaseCommit {
		valResult.Passed = false
		valResult.Reason = FixValidReasonEmptyDiff
		valResult.Detail = "no commit was produced (HEAD unchanged from baseCommit after staging)"
		r.finish(ctx, ws, in, valResult, logBuf.Bytes(), false)
		return r.releaseWithComment(ctx, in, "🤖 A FIX attempt did not pass validation (empty-diff): no committed changes were produced.")
	}

	spec, err := BuildFixPRSpec(in.IssueID, in.IssueURL, in.ErrorClass, in.FixBrief, ws.Branch, repo.DefaultBranch, in.ToolOutputs, r.Secrets)
	if err != nil {
		r.finish(ctx, ws, in, valResult, logBuf.Bytes(), false)
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 A passing FIX attempt could not be turned into a pull request: %s", sanitizeForComment(err.Error())))
	}

	if r.Caps != nil {
		repoKey := repo.Repo.Provider + "/" + repo.Repo.Owner + "/" + repo.Repo.Repo
		if !r.Caps.AllowPR(repoKey) {
			r.emit("skipped:prs-per-day", in, repoKey)
			r.finish(ctx, ws, in, valResult, logBuf.Bytes(), false)
			return r.releaseWithComment(ctx, in, "🤖 A fix is ready but the daily pull-request cap has been reached; it will not be opened automatically today.")
		}
	}

	pr, err := CreateFixPR(ctx, repo.Provider, repo.Repo, PushFixBranchInput{
		RepoDir:       ws.RepoDir,
		Branch:        ws.Branch,
		DefaultBranch: repo.DefaultBranch,
		Cred:          repo.Provider.Auth(),
		Redactor:      redactor,
	}, spec)
	if err != nil {
		r.finish(ctx, ws, in, valResult, logBuf.Bytes(), false)
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Opening the fix pull request failed: %s", sanitizeForComment(err.Error())))
	}

	if _, err := PostFixPRBatch(ctx, r.Client, in.JobID, in.IssueID, pr); err != nil {
		r.emit("error:pr-batch", in, err.Error())
	}
	if err := JournalFixPROpen(r.Journal, in.JobID, in.IssueID, in.TriggerSeq, FixPRPayload{
		Provider: repo.Repo,
		PRID:     pr.ID,
		PRURL:    pr.URL,
	}); err != nil {
		r.emit("error:journal-pr-open", in, err.Error())
	}
	r.emit("pr-opened", in, pr.URL)

	r.finish(ctx, ws, in, valResult, logBuf.Bytes(), true)
	return nil
}

// ResumeFix implements the restart-time half of plan §4.4 step 3b for a FIX job whose live resume
// state was already saved by a prior attempt (watchLiveResumeSave): LoadResumeState -> fresh clone
// + checkout baseCommit + git apply diff.patch (ResumeFixWorkspace) -> re-invoke the Fix Executor
// with a continuation prompt (ContinuationBrief) -> the same validate/PR/finish tail RunFix uses.
//
// It deliberately does NOT call FixCaps.AllowJobStart (a resume isn't "one more job") or
// RecordAttempt (CLAUDE.md: "a crash-resume of the same job does NOT count again") -- only
// AllowAttempt is checked, since a resume must still respect WORKER_MAX_FIX_ATTEMPTS. If no resume
// state was ever saved for in.JobID (e.g. the crash happened before the first debounced save), this
// falls back to RunFix, which DOES count as a fresh attempt.
//
// No automatic caller invokes this on process restart yet -- see the package doc.
func (r *FixRunner) ResumeFix(ctx context.Context, in FixJobInput) error {
	if r.Journal == nil || r.Client == nil {
		return fmt.Errorf("jobs: ResumeFix: Journal and Client are required")
	}
	if r.ResolveRepo == nil {
		return fmt.Errorf("jobs: ResumeFix: ResolveRepo is required")
	}
	if r.Sink == nil {
		return fmt.Errorf("jobs: ResumeFix: an ArtifactSink is required to resume")
	}

	repo, found, err := r.ResolveRepo(in.ProjectID)
	if err != nil {
		return fmt.Errorf("jobs: ResumeFix: resolving repo for project %s: %w", in.ProjectID, err)
	}
	if !found {
		r.emit("skipped:no-repo-connection", in, "")
		return r.postProposeOnly(ctx, in)
	}

	if r.Caps != nil && !r.Caps.AllowAttempt(in.JobID) {
		r.emit("skipped:fix-attempts", in, "")
		return r.releaseWithComment(ctx, in, "🤖 This issue has exhausted its FIX attempt budget without a passing attempt; a human should take it from here.")
	}

	prior, found, err := LoadResumeState(ctx, r.Sink, in.JobID)
	if err != nil {
		r.emit("error:load-resume-state", in, err.Error())
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not load saved FIX progress to resume: %s", sanitizeForComment(err.Error())))
	}
	if !found {
		// Nothing was ever saved for this job (crash before the first debounced save) -- a resume
		// with nothing to resume from is just a fresh start, which DOES count an attempt.
		r.emit("resume:no-saved-state,falling-back-to-fresh", in, "")
		return r.RunFix(ctx, in)
	}

	redactor := gitprovider.NewRedactor(nil)
	redactor.AddSecrets(r.Secrets...)

	result, err := ResumeFixWorkspace(ctx, ResumeFixWorkspaceInput{
		WorkspaceRoot: r.WorkspaceRoot,
		JobID:         in.JobID,
		IssueID:       in.IssueID,
		CloneURL:      repo.CloneURL,
		Cred:          repo.Provider.Auth(),
		Redactor:      redactor,
		State:         prior,
	})
	if err != nil {
		r.emit("error:resume-workspace", in, err.Error())
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not resume the FIX workspace: %s", sanitizeForComment(err.Error())))
	}
	ws := result.Workspace

	if !result.PatchApplied {
		// plan §4.4 step 3b: "Patch-apply failure => clean restart of that attempt." Not a new
		// attempt in FixCaps' accounting -- discard this workspace and retry from a fresh clone at
		// the ORIGINAL baseCommit's brief, same jobID, same attempt count.
		r.emit("resume:patch-apply-failed,clean-restart", in, "")
		if err := CleanupFixWorkspace(ws); err != nil {
			r.emit("error:cleanup", in, err.Error())
		}
		ws, err = PrepareFixWorkspace(ctx, PrepareFixWorkspaceInput{
			WorkspaceRoot: r.WorkspaceRoot,
			JobID:         in.JobID,
			IssueID:       in.IssueID,
			CloneURL:      repo.CloneURL,
			DefaultBranch: repo.DefaultBranch,
			CloneDepth:    repo.CloneDepth,
			Cred:          repo.Provider.Auth(),
			Redactor:      redactor,
			Brief: TaskBrief{
				IssueID:     in.IssueID,
				IssueURL:    in.IssueURL,
				IssueTitle:  in.ErrorClass,
				Occurrences: in.Occurrences,
				FixBrief:    in.FixBrief,
				TestCmd:     repo.TestCmd,
			},
		})
		if err != nil {
			r.emit("error:workspace", in, err.Error())
			return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not prepare a FIX workspace after a resume patch-apply failure: %s", sanitizeForComment(err.Error())))
		}
		return r.executeValidatePublish(ctx, ws, in, repo, redactor)
	}

	// Patch applied cleanly: overwrite TASK.md with the continuation prompt (guard.WrapUntrusted via
	// TaskBrief.ContinuationPrompt gates prior.ProgressMD the same way a fresh brief's FixBrief is
	// gated) so the re-invoked executor knows this workspace already contains prior work.
	continuation := ContinuationBrief(TaskBrief{
		IssueID:     in.IssueID,
		IssueURL:    in.IssueURL,
		IssueTitle:  in.ErrorClass,
		Occurrences: in.Occurrences,
		FixBrief:    in.FixBrief,
		TestCmd:     repo.TestCmd,
	}, prior)
	if err := os.WriteFile(ws.TaskPath, []byte(continuation), 0o600); err != nil {
		r.emit("error:continuation-brief", in, err.Error())
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not write the FIX continuation brief: %s", sanitizeForComment(err.Error())))
	}

	return r.executeValidatePublish(ctx, ws, in, repo, redactor)
}

// finish persists the plan §4.4 step-3c on-job-end artifact bundle and cleans up the workspace
// (unless KeepFailed is set and the attempt did not pass) -- called on every RunFix exit path once
// a workspace exists, success or failure alike.
func (r *FixRunner) finish(ctx context.Context, ws *FixWorkspace, in FixJobInput, valResult FixValidationResult, agentLog []byte, passed bool) {
	if r.Sink != nil && ws != nil {
		if err := SaveJobArtifacts(ctx, r.Sink, ws, in, valResult, agentLog); err != nil {
			r.emit("error:save-artifacts", in, err.Error())
		}
	}
	if ws != nil && (passed || !r.KeepFailed) {
		if err := CleanupFixWorkspace(ws); err != nil {
			r.emit("error:cleanup", in, err.Error())
		}
	}
}

// postProposeOnly posts the FixBrief as a plain diagnosis comment (no PR, no workspace) and
// releases the claim -- plan §4.4's "no repo connection ⇒ propose-only" path.
func (r *FixRunner) postProposeOnly(ctx context.Context, in FixJobInput) error {
	body := "🤖 This issue looks fixable, but no repository connection is configured for this project, so no pull request can be opened automatically.\n\n## Diagnosis (Fix Brief)\n\n" + in.FixBrief
	return r.releaseWithComment(ctx, in, body)
}

// releaseWithComment posts body as a comment then releases the claim -- the shared tail of every
// RunFix failure/skip path, matching PostFixPRClosedBatch's own comment-then-release op order.
func (r *FixRunner) releaseWithComment(ctx context.Context, in FixJobInput, body string) error {
	b := newOpBuilder(in.JobID)
	b.add("issues.comment", in.IssueID, map[string]interface{}{"body_md": body})
	if err := b.addRelease(in.IssueID); err != nil {
		return err
	}
	_, err := r.Client.PostBatch(ctx, sentinel.BatchRequest{Operations: b.ops, StopOnError: false})
	if err != nil {
		return fmt.Errorf("jobs: RunFix: posting release batch for job %s: %w", in.JobID, err)
	}
	return nil
}

// sanitizeForComment collapses whitespace and caps length, mirroring errorClassForTitle's
// reasoning (fix_pr.go) -- an error/detail string embedded in a human-facing comment must not
// blow up or carry embedded control characters from a misbehaving executor/test command.
func sanitizeForComment(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const maxLen = 500
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}

// --- on-job-end artifact bundle (plan §4.4 step 3c) ---------------------------------------------

// jobArtifactBundleKeys are the plan §4.4 step-3c on-job-end artifacts, distinct from
// fix_resume.go's four LIVE resume keys (same fix-artifacts/<jobId>/ prefix, different names, so a
// bundle write can never collide with or clobber an in-flight resume-state save).
func jobAgentLogKey(jobID string) string      { return resumeArtifactPrefix(jobID) + "agent.log" }
func jobFinalDiffKey(jobID string) string     { return resumeArtifactPrefix(jobID) + "final-diff.patch" }
func jobValidationKey(jobID string) string    { return resumeArtifactPrefix(jobID) + "validation.json" }
func jobFinalTaskKey(jobID string) string     { return resumeArtifactPrefix(jobID) + "TASK.md" }
func jobFinalProgressKey(jobID string) string { return resumeArtifactPrefix(jobID) + "PROGRESS.md" }

// SaveJobArtifacts persists the plan §4.4 step-3c on-job-end bundle -- agent log, final diff,
// TASK.md, PROGRESS.md, and the validation result -- to fix-artifacts/<jobId>/ under sink, on ANY
// outcome (success or failure). These are separate, per-job objects with their own ~30-day
// lifecycle (an operational bucket-lifecycle-rule concern, not enforced by this code); they are
// NEVER part of the §2.8 state snapshot.
func SaveJobArtifacts(ctx context.Context, sink ArtifactSink, ws *FixWorkspace, in FixJobInput, result FixValidationResult, agentLog []byte) error {
	if sink == nil || ws == nil {
		return fmt.Errorf("jobs: SaveJobArtifacts: sink and workspace are required")
	}
	taskMD, err := os.ReadFile(ws.TaskPath)
	if err != nil {
		return fmt.Errorf("jobs: SaveJobArtifacts: reading TASK.md: %w", err)
	}
	progressMD, err := os.ReadFile(ws.ProgressPath)
	if err != nil {
		return fmt.Errorf("jobs: SaveJobArtifacts: reading PROGRESS.md: %w", err)
	}
	finalDiff, err := ComputeDiffPatch(ctx, ws.RepoDir, ws.BaseCommit, gitprovider.GitCredential{}, gitprovider.NewRedactor(nil))
	if err != nil {
		// Diff computation failing must not lose the rest of the bundle (the agent log alone is
		// often the most useful artifact for a failed attempt) -- record it as the diff content
		// rather than aborting the whole bundle.
		finalDiff = []byte("(final diff unavailable: " + err.Error() + ")")
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("jobs: SaveJobArtifacts: marshaling validation result: %w", err)
	}
	items := map[string][]byte{
		jobAgentLogKey(ws.JobID):      agentLog,
		jobFinalDiffKey(ws.JobID):     finalDiff,
		jobValidationKey(ws.JobID):    resultJSON,
		jobFinalTaskKey(ws.JobID):     taskMD,
		jobFinalProgressKey(ws.JobID): progressMD,
	}
	for key, data := range items {
		if err := sink.Put(ctx, key, data); err != nil {
			return fmt.Errorf("jobs: SaveJobArtifacts: saving %s: %w", key, err)
		}
	}
	return nil
}
