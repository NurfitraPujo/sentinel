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
// The automatic, worker-restart-time TRIGGER IS wired as of N8f/N8h: a FIX job left mid-attempt is
// journaled with the FIX-specific in-flight state StateFixRunning (see journalFixRunning), and
// main.go's resumeInFlightJob routes a StateFixRunning record surfaced by state.Journal.RecoveryScan
// straight into FixRunner.ResumeFix on boot -- the same shape as runner.Resume's TRIAGE/FOLLOW-UP
// replay (plan §4.4 step 3b, "resume after any restart"). Since N8h, the FIX job also carries its
// OWN kind="fix" jobID (distinct from the parent triage/followup job), so its in-flight and PR-open
// records are never dropped by the journal's terminal guard.
//
// ResumeFix is an explicit, exported entry point (LoadResumeState -> ResumeFixWorkspace ->
// ContinuationBrief -> re-invoke executor -> the same validate/PR/finish tail RunFix uses). It
// correctly does not call FixCaps.AllowJobStart or RecordAttempt (CLAUDE.md: "a crash-resume of the
// same job does NOT count again").
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/guard"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// DefaultResumeDebounceInterval is how often a growing PROGRESS.md may trigger a new live
// resume-state save (plan §4.4 step 3b: "debounced") when FixRunner.ResumeDebounceInterval is unset.
const DefaultResumeDebounceInterval = 20 * time.Second

// DefaultProgressPostInterval is how often a growing PROGRESS.md may trigger a new throttled
// issues.progress post to Sentinel (plan §4.4 step 3: "tail PROGRESS.md ... post throttled
// issues.progress updates") when FixRunner.ProgressPostInterval is unset. Kept as its own knob
// (finding 2) -- distinct from DefaultResumeDebounceInterval/ResumeDebounceInterval, which throttle
// the unrelated live resume-state save, not what gets posted to the issue.
const DefaultProgressPostInterval = 30 * time.Second

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
	// MaxPRsPerDay is C15's server-supplied per-project PR cap (settings.ProjectSettings.
	// MaxPRsPerDay) — nil means the server reported null ("no self-enforced cap"; the global
	// WORKER_MAX_PRS_PER_DAY still applies via FixCaps.AllowPR's fallback). Finding 4: this field
	// existed on settings.ProjectSettings already but was never threaded into FixRepoConfig, so
	// AllowPR had no way to see it.
	MaxPRsPerDay *int
	// AgentCmd is C15's per-connection executor override (settings.RepoConnection.AgentCmd) --
	// finding 3: empty means "use the worker's built-in agent" (the global FIX_EXECUTOR_CMD/
	// FixRunner.ExecutorCmd), matching the dashboard UI's own documented default. Before this fix
	// the value was fetched into settings but never read anywhere, so every project ran the global
	// executor regardless of what was configured per-connection.
	AgentCmd string
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

	// ProgressPostInterval throttles how often a growing PROGRESS.md triggers a NEW issues.progress
	// post to Sentinel (finding 2, plan §4.4 step 3). Independent of ResumeDebounceInterval -- the
	// two throttle unrelated things (an internal resume-state save vs. a publish to the issue) and
	// may reasonably differ. Defaults to DefaultProgressPostInterval when <= 0.
	ProgressPostInterval time.Duration

	Secrets []string // configured secret values to redact from logs/PR body (guard.Check's gate)

	// MaxVerbatim is WORKER_GATE_MAX_VERBATIM, threaded to BuildFixPRSpec's guard.CheckWithConfig
	// call (plan §4.6/§5 finding 3); <=0 uses guard.DefaultMaxVerbatim.
	MaxVerbatim float64

	// OnEvent, when non-nil, is called for every notable outcome ("dispatched", "skipped:<reason>",
	// "failed:<reason>", "pr-opened", "error") so main.go can log/metric without this package
	// importing log/slog. Never called with a nil in-flight struct.
	OnEvent func(event string, in FixJobInput, detail string)

	// OnAuthFailure, when non-nil, is called at most once per RunFix/ResumeFix invocation when a
	// git-auth failure (gitprovider.ClassAuthFailure — a REST 401/403 from CreatePR/PRStatus, or a
	// git CLI "Authentication failed" from clone/push) is observed (finding 5, C16). main.go wires
	// this to invalidate/refresh the in-memory credential for provider so the NEXT resolve
	// re-fetches instead of waiting for the 5-minute periodic settings refresh. provider is the
	// gitprovider RepoRef's Provider string ("github"/"bitbucket").
	OnAuthFailure func(provider string)

	// wg tracks every FIX goroutine Dispatch has started but not yet finished (finding 3): main.go's
	// graceful-shutdown path calls Wait (bounded by WORKER_SHUTDOWN_TIMEOUT) after draining the
	// dispatcher, so a SIGTERM does not lose an in-flight attempt's live resume-save (watchLiveResumeSave
	// already listens for SIGTERM independently and saves before returning -- what was missing was the
	// PROCESS waiting for that save to actually finish before exiting).
	wg sync.WaitGroup
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

// executorCmdFor resolves the coding-agent command one FIX attempt actually runs (finding 3, C15):
// repo.AgentCmd (settings.RepoConnection.AgentCmd, the per-project override) wins when non-empty,
// falling back to the global r.ExecutorCmd (FIX_EXECUTOR_CMD) otherwise -- matching the dashboard
// UI's own documented "defaults to the worker's built-in agent" contract. Before this fix,
// repo.AgentCmd was fetched into settings but never consulted here, so every project ran the
// global command regardless of what was configured per-connection.
func (r *FixRunner) executorCmdFor(repo FixRepoConfig) string {
	if strings.TrimSpace(repo.AgentCmd) != "" {
		return repo.AgentCmd
	}
	return r.ExecutorCmd
}

func (r *FixRunner) progressPostInterval() time.Duration {
	if r.ProgressPostInterval > 0 {
		return r.ProgressPostInterval
	}
	return DefaultProgressPostInterval
}

// FixProgressLinePayload is the journal payload for one throttled PROGRESS.md line posted to
// Sentinel (finding 2) -- an audit record of what was actually published, decoupled from
// FixRunningPayload/FixPRPayload's own record shapes.
type FixProgressLinePayload struct {
	Line string `json:"line"`
}

// journalFixProgressLine appends a terminal (StateDone), audit-only record of one PROGRESS.md
// line actually posted to Sentinel -- keyed under a synthetic jobID (in.JobID + ":progress",
// distinct from in.JobID itself) so it can NEVER become the "latest" record
// state.Journal.RecoveryScan tracks for the real FIX jobID (that must stay whatever
// journalFixRunning/journalFixTerminal/JournalFixPROpen last wrote, or a crash mid-attempt would
// stop looking resumable). Best-effort like every other journal write in this package -- a write
// failure here degrades audit trail only, never the attempt itself.
func journalFixProgressLine(j *state.Journal, in FixJobInput, line string) error {
	payload, err := json.Marshal(FixProgressLinePayload{Line: line})
	if err != nil {
		return fmt.Errorf("jobs: fix: marshaling progress-line payload for job %s: %w", in.JobID, err)
	}
	return j.Append(state.Record{
		JobID:      in.JobID + ":progress",
		IssueID:    in.IssueID,
		Kind:       FixKind,
		TriggerSeq: in.TriggerSeq,
		State:      state.StateDone,
		Payload:    payload,
	})
}

// postProgressLine gates line through guard.Check (the same publish-safety gate every other
// model/executor-influenced text this package publishes goes through -- FieldFixBrief is reused
// here since PROGRESS.md content, like a Fix Brief, is untrusted coding-agent output headed for a
// human-facing issue note) and, if it passes, posts it as a SINGLE-op issues.progress batch
// (message_md -- finding 1's same param-name fix applies here) then journals the post
// (journalFixProgressLine). A gate rejection or a post failure is emitted as an event but never
// fails the attempt -- a missed progress update is a degraded-observability problem, not a reason
// to abandon an otherwise-succeeding FIX attempt.
func (r *FixRunner) postProgressLine(ctx context.Context, in FixJobInput, line string) {
	if r.Client == nil || line == "" {
		return
	}
	cfg := guard.Config{SecretValues: r.Secrets, MaxVerbatim: r.MaxVerbatim}
	if cfg.MaxVerbatim <= 0 {
		cfg.MaxVerbatim = guard.DefaultMaxVerbatim
	}
	if err := guard.CheckWithConfig(guard.FieldFixBrief, line, nil, cfg); err != nil {
		r.emit("error:progress-gate-reject", in, err.Error())
		return
	}
	b := newOpBuilder(in.JobID)
	b.add("issues.progress", in.IssueID, map[string]interface{}{"message_md": line})
	res, err := r.Client.PostBatch(ctx, sentinel.BatchRequest{Operations: b.ops, StopOnError: false})
	if err != nil {
		r.emit("error:progress-post", in, err.Error())
		return
	}
	if res.Status < 200 || res.Status >= 300 {
		r.emit("error:progress-post", in, fmt.Sprintf("status %d", res.Status))
		return
	}
	if err := checkBatchResults(Compiled{Ops: b.ops}, res); err != nil {
		r.emit("error:progress-post", in, err.Error())
		return
	}
	if r.Journal != nil {
		if err := journalFixProgressLine(r.Journal, in, line); err != nil {
			r.emit("error:journal-progress-line", in, err.Error())
		}
	}
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
	progressPostDebouncer := &ResumeDebouncer{Interval: r.progressPostInterval()}
	onProgressLine = func(line string) {
		if debouncer.ShouldSave() {
			save()
		}
		// finding 2 (plan §4.4 step 3): PROGRESS.md tailing was previously wired ONLY to the live
		// resume-save above -- the line text itself was discarded, so a coding agent's progress
		// narration never reached the issue as an issues.progress update. Throttled independently
		// (progressPostDebouncer, not the resume-save debouncer above) via its own
		// ProgressPostInterval.
		if progressPostDebouncer.ShouldSave() {
			r.postProgressLine(ctx, in, line)
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

// runBounded is Dispatch/DispatchResume's shared shape: run fn (RunFix or ResumeFix) on its own
// goroutine, tracked by r.wg, with a context detached from the caller's own ctx (which may be
// cancelled or unbounded long before a 30-minute FIX attempt could ever finish) and bounded by
// r.Timeout+2min headroom, recovering from any panic inside fn so a coding-agent-triggered crash
// never propagates back into whatever loop called Dispatch/DispatchResume. errEvent labels the
// emitted "error" event distinctly per caller ("error" for Dispatch, "error:resume" for
// DispatchResume) so main.go's FIX-event logging can tell a fresh-attempt failure apart from a
// boot-recovery failure without inspecting FixJobInput.
func (r *FixRunner) runBounded(in FixJobInput, errEvent string, fn func(ctx context.Context, in FixJobInput) error) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
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
		if err := fn(ctx, in); err != nil {
			r.emit(errEvent, in, err.Error())
		}
	}()
}

// Dispatch implements Fixer: it runs RunFix on its own goroutine with a context detached from
// whatever ctx Act was called with (Act's ctx is cancelled the moment Act returns, long before a
// 30-minute FIX attempt could ever finish), bounded by r.Timeout, and recovers from a panic inside
// RunFix so a coding-agent-triggered crash never propagates back into the poll loop.
func (r *FixRunner) Dispatch(in FixJobInput) {
	r.runBounded(in, "error", r.RunFix)
}

// DispatchResume runs ResumeFix on its own goroutine, bounded by r.Timeout and tracked by the same
// WaitGroup Dispatch uses -- the finding-2 MAJOR fix (fix-lifecycle remediation round 2): before
// this, main.go's boot-time recovery pass called ResumeFix SYNCHRONOUSLY, on the caller's own
// unbounded ctx, BEFORE the poll loop ever started, so a single slow (or, via finding 1, forever-
// stale pre-fix) in-flight FIX attempt blocked the worker from ever polling or serving /readyz.
// Called from main.go's resumeInFlightJob for exactly the state.StateFixRunning in-flight case,
// mirroring Dispatch's own goroutine/timeout/panic-recovery/WaitGroup shape so runWorker's graceful
// shutdown (Wait, bounded by WORKER_SHUTDOWN_TIMEOUT) drains a resumed boot recovery exactly like
// any other in-flight FIX attempt.
func (r *FixRunner) DispatchResume(in FixJobInput) {
	r.runBounded(in, "error:resume", r.ResumeFix)
}

// Wait blocks until every FIX goroutine Dispatch has started so far has returned, or until ctx is
// done, whichever comes first (finding 3). main.go's graceful-shutdown path calls this, bounded by
// a WORKER_SHUTDOWN_TIMEOUT context, AFTER draining the dispatcher's per-issue queues, so a SIGTERM
// waits for an in-flight FIX attempt's resume-save (watchLiveResumeSave's SIGTERM listener) to
// actually finish writing before the process exits. Safe to call on a nil *FixRunner (no-op) so
// main.go does not need a separate nil check when the FIX engine is deployment-disabled.
func (r *FixRunner) Wait(ctx context.Context) {
	if r == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// reportAuthFailureOnce invokes r.OnAuthFailure(provider) at most once per *fired guard, and only
// when err is a git-auth failure (gitprovider.IsAuthFailure, finding 5/C16). Callers pass a
// fresh, call-scoped bool (not a FixRunner field) so concurrent Dispatch goroutines for different
// jobs never share the guard.
func (r *FixRunner) reportAuthFailureOnce(fired *bool, provider string, err error) {
	if r.OnAuthFailure == nil || *fired || err == nil || !gitprovider.IsAuthFailure(err) {
		return
	}
	*fired = true
	r.OnAuthFailure(provider)
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
		// Finding 5 (MINOR, fix-lifecycle remediation round 2): a bare error return here, before this
		// fix, stranded the claim for WORKER_NAG_DAYS -- the repo CONNECTION exists (found would have
		// been true) but the credential behind it is unusable (e.g. a revoked/expired token), and
		// every OTHER RunFix failure tail (workspace prep, executor, validate, commit, PR-spec,
		// CreatePR) already releases via releaseWithComment; this was the one path that didn't. Same
		// tail as every other early-gate failure: comment + release + terminal journal record.
		r.emit("error:resolve-repo", in, err.Error())
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not resolve the repository connection for this project: %s", sanitizeForComment(err.Error())), state.StateFailed)
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
		// Round-2 finding 6: AllowIssueAttempt is the real PER-ISSUE cap (WORKER_MAX_FIX_ATTEMPTS as
		// the plan documents it), checked alongside AllowAttempt's per-jobID cap -- without it, an
		// issue that re-triggers a FIX from several different events (a fresh jobID each time, since
		// jobID = JobID(FixKind, issue, triggerSeq)) gets a brand-new, full attempt budget every
		// single re-trigger, so the cap never actually bounds attempts against ONE issue.
		if !r.Caps.AllowAttempt(in.JobID) || !r.Caps.AllowIssueAttempt(in.IssueID) {
			r.emit("skipped:fix-attempts", in, "")
			return r.releaseWithComment(ctx, in, "🤖 This issue has exhausted its FIX attempt budget without a passing attempt; a human should take it from here.", state.StateSkipped)
		}
		if !r.Caps.AllowJobStart() {
			r.emit("skipped:fix-jobs-per-day", in, "")
			return r.releaseWithComment(ctx, in, "🤖 The daily FIX job cap has been reached; a human should look at this issue directly for now.", state.StateSkipped)
		}
		r.Caps.RecordAttempt(in.JobID)
		r.Caps.RecordIssueAttempt(in.IssueID)
	}
	// Finding 6 (durability-startup remediation, plan §7 "fix_attempts"): emit once per genuine
	// attempt -- past both the no-repo-connection and attempt/day-cap gates above, i.e. an attempt
	// that is actually going to run the executor, not merely a re-dispatch that was turned away.
	r.emit("attempt-started", in, "")

	// Finding 4: journal the plan §4.4 step-3b in-flight FIX marker here, at RunFix's real entry
	// point for a genuine attempt -- BEFORE PrepareFixWorkspace runs, not after it returns (which is
	// where this write used to live, inside executeValidatePublish). PrepareFixWorkspace does a full
	// git clone and can itself run for a long time; a crash during that clone, before this fix, left
	// r.Caps.RecordAttempt/RecordIssueAttempt's in-memory bookkeeping (just above) with no journal
	// trace at all, so FixCaps.SeedToday's boot-time reconstruction would not know this attempt ever
	// started, and main.go's RecoveryScan would not know to resume it either -- the claim stayed held
	// with the attempt silently un-journaled. baseCommit is not yet known this early (no workspace
	// exists) and is carried only for observability (see FixRunningPayload's doc) -- ResumeFix
	// re-derives the real base commit it needs from the saved resume-state artifacts, not from this
	// field. Best-effort, like every other journal write in this file.
	if err := journalFixRunning(r.Journal, in, "", false); err != nil {
		r.emit("error:journal-fix-running", in, err.Error())
	}

	// jobSecrets is r.Secrets (the static env-derived list) PLUS this job's runtime,
	// server-managed repo credential (finding 8, C16 primary credential path) -- the token most
	// likely to be present and most likely to be leaked verbatim into a PR body/Fix Brief by the
	// coding agent, and the one r.Secrets alone (populated once in main.go from env) can never
	// contain. Every redactor AND guard.Check's secret list below must use jobSecrets, not
	// r.Secrets directly, or the runtime credential goes unguarded on this path.
	jobSecrets := append(append([]string{}, r.Secrets...), repo.Provider.Auth().Secrets()...)

	redactor := gitprovider.NewRedactor(nil)
	redactor.AddSecrets(jobSecrets...)

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
		var authFailure bool
		r.reportAuthFailureOnce(&authFailure, repo.Repo.Provider, err)
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not prepare a FIX workspace: %s", sanitizeForComment(err.Error())), state.StateFailed)
	}

	return r.executeValidatePublish(ctx, ws, in, repo, redactor, jobSecrets)
}

// executeValidatePublish is RunFix/ResumeFix's shared tail from plan §4.4 steps 3-5 once a
// workspace exists (fresh OR resumed): run the Fix Executor (with the live resume-save wired to
// its progress callback and a SIGTERM listener for the duration) -> validate -> build+push the PR
// -> journal it open -> SaveJobArtifacts. Every exit path calls r.finish exactly once.
func (r *FixRunner) executeValidatePublish(ctx context.Context, ws *FixWorkspace, in FixJobInput, repo FixRepoConfig, redactor *gitprovider.Redactor, jobSecrets []string) error {
	// authFailure guards r.OnAuthFailure to at-most-once for this attempt (finding 5, C16) --
	// scoped to this call, never shared across concurrently-dispatched jobs.
	var authFailure bool

	// Finding 4: the plan §4.4 step-3b in-flight FIX marker is now journaled EARLY -- by RunFix
	// before PrepareFixWorkspace runs, and by ResumeFix before ResumeFixWorkspace/the clean-restart
	// PrepareFixWorkspace call, both below -- rather than here, once ws already exists. Writing it
	// here too (a second time, now that ws.BaseCommit is known) would double-count this attempt in
	// FixCaps.SeedToday's boot-time reconstruction, which counts one WORKER_MAX_FIX_JOBS_PER_DAY/
	// WORKER_MAX_FIX_ATTEMPTS slot per State==state.StateFixRunning record for a jobID -- so this
	// function deliberately does NOT journal it again.

	var logBuf bytes.Buffer
	logRedactor := gitprovider.NewRedactor(&logBuf)
	logRedactor.AddSecrets(jobSecrets...)

	saveRedactor := gitprovider.NewRedactor(nil)
	saveRedactor.AddSecrets(jobSecrets...)
	onProgressLine, stopResumeSave := r.watchLiveResumeSave(ctx, ws, in, repo.Provider.Auth(), saveRedactor)
	defer stopResumeSave()

	execResult := RunFixExecutor(ctx, FixExecutorInput{
		Cmd:            r.executorCmdFor(repo),
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
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 The Fix Executor failed to run: %s", sanitizeForComment(execResult.Err.Error())), state.StateFailed)
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
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not validate the FIX attempt: %s", sanitizeForComment(err.Error())), state.StateFailed)
	}

	if !valResult.Passed {
		r.finish(ctx, ws, in, valResult, logBuf.Bytes(), false)
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 A FIX attempt did not pass validation (%s): %s", valResult.Reason, sanitizeForComment(valResult.Detail)), state.StateFailed)
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
		r.reportAuthFailureOnce(&authFailure, repo.Repo.Provider, err)
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not commit the FIX attempt: %s", sanitizeForComment(err.Error())), state.StateFailed)
	}
	if !changed || commitSHA == ws.BaseCommit {
		valResult.Passed = false
		valResult.Reason = FixValidReasonEmptyDiff
		valResult.Detail = "no commit was produced (HEAD unchanged from baseCommit after staging)"
		r.finish(ctx, ws, in, valResult, logBuf.Bytes(), false)
		return r.releaseWithComment(ctx, in, "🤖 A FIX attempt did not pass validation (empty-diff): no committed changes were produced.", state.StateFailed)
	}

	spec, err := BuildFixPRSpec(in.IssueID, in.IssueURL, in.ErrorClass, in.FixBrief, ws.Branch, repo.DefaultBranch, in.ToolOutputs, jobSecrets, r.MaxVerbatim)
	if err != nil {
		r.finish(ctx, ws, in, valResult, logBuf.Bytes(), false)
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 A passing FIX attempt could not be turned into a pull request: %s", sanitizeForComment(err.Error())), state.StateFailed)
	}

	if r.Caps != nil {
		repoKey := repo.Repo.Provider + "/" + repo.Repo.Owner + "/" + repo.Repo.Repo
		if !r.Caps.AllowPR(repoKey, in.ProjectID, repo.MaxPRsPerDay) {
			r.emit("skipped:prs-per-day", in, repoKey)
			r.finish(ctx, ws, in, valResult, logBuf.Bytes(), false)
			return r.releaseWithComment(ctx, in, "🤖 A fix is ready but the daily pull-request cap has been reached; it will not be opened automatically today.", state.StateSkipped)
		}
	}

	pr, err := CreateFixPR(ctx, repo.Provider, repo.Repo, PushFixBranchInput{
		RepoDir:       ws.RepoDir,
		Branch:        ws.Branch,
		DefaultBranch: repo.DefaultBranch,
		Cred:          repo.Provider.Auth(),
		Redactor:      redactor,
		CloneURL:      repo.CloneURL,
	}, spec)
	if err != nil {
		r.finish(ctx, ws, in, valResult, logBuf.Bytes(), false)
		r.reportAuthFailureOnce(&authFailure, repo.Repo.Provider, err)
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Opening the fix pull request failed: %s", sanitizeForComment(err.Error())), state.StateFailed)
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
		// Finding 5 (MINOR, fix-lifecycle remediation round 2): same claim-leak this fixes in RunFix
		// -- the repo connection exists but the credential is unusable, so release rather than
		// stranding the claim for WORKER_NAG_DAYS.
		r.emit("error:resolve-repo", in, err.Error())
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not resolve the repository connection for this project: %s", sanitizeForComment(err.Error())), state.StateFailed)
	}
	if !found {
		r.emit("skipped:no-repo-connection", in, "")
		return r.postProposeOnly(ctx, in)
	}

	if r.Caps != nil && (!r.Caps.AllowAttempt(in.JobID) || !r.Caps.AllowIssueAttempt(in.IssueID)) {
		r.emit("skipped:fix-attempts", in, "")
		return r.releaseWithComment(ctx, in, "🤖 This issue has exhausted its FIX attempt budget without a passing attempt; a human should take it from here.", state.StateSkipped)
	}

	prior, found, err := LoadResumeState(ctx, r.Sink, in.JobID)
	if err != nil {
		r.emit("error:load-resume-state", in, err.Error())
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not load saved FIX progress to resume: %s", sanitizeForComment(err.Error())), state.StateFailed)
	}
	if !found {
		// Nothing was ever saved for this job (crash before the first debounced save) -- a resume
		// with nothing to resume from is just a fresh start, which DOES count an attempt.
		r.emit("resume:no-saved-state,falling-back-to-fresh", in, "")
		return r.RunFix(ctx, in)
	}

	// Finding 4/5: journal the in-flight marker for this RESUME here, once we actually know there is
	// saved state to resume from (prior found=true above) -- symmetric to RunFix's own early write,
	// and BEFORE ResumeFixWorkspace/the clean-restart PrepareFixWorkspace call below runs, so a crash
	// during either is journaled and resumable. resumed=true (finding 5): a resume of an
	// already-counted attempt must NOT increment FixCaps.SeedToday's per-jobID/per-issue attempt
	// count on the next boot the way a fresh RunFix attempt does -- this is the SAME attempt
	// continuing, not one more.
	if err := journalFixRunning(r.Journal, in, "", true); err != nil {
		r.emit("error:journal-fix-running", in, err.Error())
	}

	// jobSecrets: see the identical comment in RunFix -- r.Secrets PLUS this job's runtime,
	// server-managed repo credential (finding 8, C16).
	jobSecrets := append(append([]string{}, r.Secrets...), repo.Provider.Auth().Secrets()...)

	redactor := gitprovider.NewRedactor(nil)
	redactor.AddSecrets(jobSecrets...)

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
		var authFailure bool
		r.reportAuthFailureOnce(&authFailure, repo.Repo.Provider, err)
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not resume the FIX workspace: %s", sanitizeForComment(err.Error())), state.StateFailed)
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
			return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not prepare a FIX workspace after a resume patch-apply failure: %s", sanitizeForComment(err.Error())), state.StateFailed)
		}
		return r.executeValidatePublish(ctx, ws, in, repo, redactor, jobSecrets)
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
		return r.releaseWithComment(ctx, in, fmt.Sprintf("🤖 Could not write the FIX continuation brief: %s", sanitizeForComment(err.Error())), state.StateFailed)
	}

	return r.executeValidatePublish(ctx, ws, in, repo, redactor, jobSecrets)
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
//
// circuit-config-sec finding 6: in.FixBrief is model-authored (an Advisor decision), and this is a
// publish site -- exactly the shape guard.Check exists to gate (plan §4.6). It IS gated once
// upstream (jobs.CompileTriage/CompileFollowup's guard.CheckWithConfig(guard.FieldFixBrief, ...)
// runs before a FixBrief is ever accepted into a Decision at all), but a publish site should
// self-defend rather than trust that every caller upstream of it gated correctly forever -- the
// same posture BuildFixPRSpec already takes for the PR-body path (fix_pr.go). Re-running the gate
// here costs nothing when the brief is already clean, and catches a future caller that reaches
// postProposeOnly with an ungated FixBrief (e.g. a resumed job whose journaled brief predates a
// gate change, or a new caller added later that forgets the upstream gate).
func (r *FixRunner) postProposeOnly(ctx context.Context, in FixJobInput) error {
	cfg := guard.Config{SecretValues: r.Secrets, MaxVerbatim: r.MaxVerbatim}
	if cfg.MaxVerbatim <= 0 {
		cfg.MaxVerbatim = guard.DefaultMaxVerbatim
	}
	if err := guard.CheckWithConfig(guard.FieldFixBrief, in.FixBrief, in.ToolOutputs, cfg); err != nil {
		r.emit("error:guard-reject", in, err.Error())
		return r.releaseWithComment(ctx, in, "🤖 This issue looks fixable, but the diagnosis could not be safely posted (failed the publish-safety gate). No pull request was opened.", state.StateSkipped)
	}
	body := "🤖 This issue looks fixable, but no repository connection is configured for this project, so no pull request can be opened automatically.\n\n## Diagnosis (Fix Brief)\n\n" + in.FixBrief
	return r.releaseWithComment(ctx, in, body, state.StateSkipped)
}

// releaseWithComment posts body as a comment then releases the claim -- the shared tail of every
// RunFix/ResumeFix failure/skip path, matching PostFixPRClosedBatch's own comment-then-release op
// order. terminal (finding 1, fix-lifecycle remediation round 2) is journaled for in.JobID via
// journalFixTerminal BEFORE the comment+release batch is posted: every caller of this function is
// a FIX exit path that will never open a PR, so this jobID's journal record must be closed out
// terminal here or state.Journal.RecoveryScan re-surfaces it as in-flight forever (see
// journalFixTerminal's doc comment). Best-effort like every other journal write in this file -- a
// journal failure here degrades resumability/dedup but must not stop the claim from being released
// (leaving a claim stranded is worse than a resumable-looking record that will just be resumed and
// immediately fail/skip the same way again).
func (r *FixRunner) releaseWithComment(ctx context.Context, in FixJobInput, body string, terminal state.JobState) error {
	if r.Journal != nil {
		if err := journalFixTerminal(r.Journal, in, terminal); err != nil {
			r.emit("error:journal-fix-terminal", in, err.Error())
		}
	}
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
