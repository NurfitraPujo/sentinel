package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// --- RunFix happy path: proves the call path this package's blocker finding required ------------
// -- CreatePR -> JournalFixPROpen -- is reachable from RunFix (and, via Dispatch, from
// RealActor.Act, which main.go wires from main()).

func TestRunFix_HappyPath_OpensPRAndJournalsOpenFix(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	journalPath := filepath.Join(t.TempDir(), "jobs.journal")
	journal := state.OpenJournal(journalPath)
	sender := &recordingSender{}
	fp := &fakeProvider{pr: gitprovider.PR{ID: "42", Number: 42, URL: "https://example/pr/42"}}
	sink := LocalDirArtifactSink{Root: t.TempDir()}

	r := &FixRunner{
		WorkspaceRoot: t.TempDir(),
		Journal:       journal,
		Client:        sender,
		Sink:          sink,
		Caps:          NewFixCaps(10, 10, 2, nil),
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{
				Provider:      fp,
				Repo:          gitprovider.RepoRef{Provider: "github", Owner: "o", Repo: "r"},
				CloneURL:      bareRepo,
				DefaultBranch: "main",
			}, true, nil
		},
		ExecutorCmd: `echo "fix applied" >> fixed.txt && echo "applied fix" >> "$PROGRESS_MD"`,
	}

	in := FixJobInput{
		JobID:      "job-1",
		IssueID:    "issue-1",
		ProjectID:  "proj-1",
		ErrorClass: "NilPointerException",
		FixBrief:   "dereference guarded now",
		TriggerSeq: 1,
	}

	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	if len(fp.created) != 1 {
		t.Fatalf("expected exactly one CreatePR call, got %d", len(fp.created))
	}

	payload, found, err := ResolveOpenFixPR(journal, "issue-1")
	if err != nil {
		t.Fatalf("ResolveOpenFixPR: %v", err)
	}
	if !found {
		t.Fatal("expected JournalFixPROpen to have journaled an open fix-PR record reachable via ResolveOpenFixPR")
	}
	if payload.PRURL != "https://example/pr/42" {
		t.Fatalf("unexpected payload: %+v", payload)
	}

	// The artifact bundle (plan §4.4 step 3c) must exist even on a passing attempt.
	if _, err := sink.Get(context.Background(), jobValidationKey("job-1")); err != nil {
		t.Fatalf("expected validation.json artifact: %v", err)
	}
}

// TestRunFix_PerConnectionAgentCmd_OverridesGlobalExecutor is finding 3's RED-FIRST proof (C15):
// settings.RepoConnection.AgentCmd, threaded into FixRepoConfig.AgentCmd, must be the command that
// actually runs -- NOT the global FixRunner.ExecutorCmd -- when set. r.ExecutorCmd here is a no-op
// that produces no commit (would fail as empty-diff), while repo.AgentCmd is the command that
// actually makes+commits the fix; the attempt must PASS, proving AgentCmd ran.
func TestRunFix_PerConnectionAgentCmd_OverridesGlobalExecutor(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}
	fp := &fakeProvider{pr: gitprovider.PR{ID: "1", URL: "https://example/pr/1"}}
	sink := LocalDirArtifactSink{Root: t.TempDir()}

	r := &FixRunner{
		WorkspaceRoot: t.TempDir(),
		Journal:       journal,
		Client:        sender,
		Sink:          sink,
		Caps:          NewFixCaps(10, 10, 2, nil),
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{
				Provider:      fp,
				Repo:          gitprovider.RepoRef{Provider: "github", Owner: "o", Repo: "r"},
				CloneURL:      bareRepo,
				DefaultBranch: "main",
				AgentCmd:      `echo "fix applied" >> fixed.txt && echo "applied fix" >> "$PROGRESS_MD"`,
			}, true, nil
		},
		// The global executor is a no-op: if THIS runs instead of repo.AgentCmd, the attempt fails
		// as empty-diff and no PR is ever opened.
		ExecutorCmd: `true`,
	}

	in := FixJobInput{JobID: "job-agentcmd", IssueID: "issue-agentcmd", ProjectID: "proj-1", FixBrief: "x", TriggerSeq: 1}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if len(fp.created) != 1 {
		t.Fatalf("expected the per-connection AgentCmd to run and produce a passing PR, got %d CreatePR calls", len(fp.created))
	}
}

// TestRunFix_RuntimeCredentialIsGuardedInPRBody proves finding 8 (C16 primary credential path): the
// RUNTIME, server-managed repo credential ResolveRepo/repo.Provider.Auth() returns -- never present
// in r.Secrets, which is populated once in main.go from env -- must still be threaded into the
// per-job redactors AND guard.Check's secret list (via BuildFixPRSpec), so a Fix Brief that leaks
// that credential verbatim is rejected exactly like a leaked env secret would be.
//
// Red-first / mutation proof: this test fails (CreatePR gets called with the leaked token in the PR
// body) if jobSecrets in RunFix/executeValidatePublish is reverted to r.Secrets alone -- verified by
// hand before landing this test; the runtime credential is deliberately absent from r.Secrets here
// (r.Secrets is left nil) so nothing but the fix under test can catch the leak.
func TestRunFix_RuntimeCredentialIsGuardedInPRBody(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}

	runtimeToken := "ghp_runtimeServerManagedToken123456"
	cred := gitprovider.GitHubTokenCredential(runtimeToken)
	fp := &fakeProvider{pr: gitprovider.PR{ID: "1", URL: "https://example/pr/1"}, cred: &cred}

	r := &FixRunner{
		WorkspaceRoot: t.TempDir(),
		Journal:       journal,
		Client:        sender,
		Sink:          LocalDirArtifactSink{Root: t.TempDir()},
		Caps:          NewFixCaps(10, 10, 2, nil),
		// r.Secrets deliberately does NOT contain runtimeToken -- it must never need to, since the
		// token is resolved per-job from repo.Provider.Auth(), not from static config.
		Secrets: nil,
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{
				Provider:      fp,
				Repo:          gitprovider.RepoRef{Provider: "github", Owner: "o", Repo: "r"},
				CloneURL:      bareRepo,
				DefaultBranch: "main",
			}, true, nil
		},
		ExecutorCmd: `echo "fix applied" >> fixed.txt`,
	}

	in := FixJobInput{
		JobID:      "job-runtime-cred",
		IssueID:    "issue-runtime-cred",
		ProjectID:  "proj-1",
		ErrorClass: "SomeError",
		// The Fix Brief leaks the runtime credential verbatim -- guard.Check must reject this.
		FixBrief:   "fixed it using token " + runtimeToken + " for auth",
		TriggerSeq: 1,
	}

	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	if len(fp.created) != 0 {
		t.Fatalf("expected CreatePR to never be called once the runtime credential leak was caught, got %d calls: %+v", len(fp.created), fp.created)
	}

	for _, req := range sender.requests {
		for _, o := range req.Operations {
			params, ok := o.Params.(map[string]interface{})
			if !ok {
				continue
			}
			if body, ok := params["body_md"].(string); ok && strings.Contains(body, runtimeToken) {
				t.Fatalf("SECURITY: runtime credential leaked verbatim into a posted comment: %q", body)
			}
		}
	}
}

func TestRunFix_NoRepoConnection_ProposesOnlyAndReleases(t *testing.T) {
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}

	r := &FixRunner{
		Journal: journal,
		Client:  sender,
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{}, false, nil
		},
	}

	in := FixJobInput{JobID: "job-2", IssueID: "issue-2", ProjectID: "proj-2", FixBrief: "diagnosis text"}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	if len(sender.calls) != 1 || sender.calls[0] != "batch" {
		t.Fatalf("expected exactly one batch call (comment+release), got %+v", sender.calls)
	}

	if _, found, err := ResolveOpenFixPR(journal, "issue-2"); err != nil || found {
		t.Fatalf("no-repo-connection path must never journal an open fix-PR record: found=%v err=%v", found, err)
	}
}

// TestRunFix_NoRepoConnection_LeakedSecretInFixBriefIsGated is circuit-config-sec finding 6's
// red-first, wired-from-RunFix proof: postProposeOnly re-emits the model-authored FixBrief as a
// plain comment WITHOUT re-running guard.Check at that publish site -- it relies entirely on the
// upstream CompileTriage/CompileFollowup gate having already run. This drives the REAL
// no-repo-connection path (found=false, exactly like TestRunFix_NoRepoConnection_
// ProposesOnlyAndReleases above) with a FixBrief that leaks a configured secret verbatim, and
// asserts the secret never reaches the posted comment.
func TestRunFix_NoRepoConnection_LeakedSecretInFixBriefIsGated(t *testing.T) {
	const secret = "ghp_postProposeOnlyLeakTestSecret123456"
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}

	r := &FixRunner{
		Journal: journal,
		Client:  sender,
		Secrets: []string{secret},
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{}, false, nil
		},
	}

	in := FixJobInput{
		JobID:     "job-leak",
		IssueID:   "issue-leak",
		ProjectID: "proj-leak",
		FixBrief:  "diagnosed the bug using token " + secret + " for auth",
	}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	if len(sender.calls) != 1 || sender.calls[0] != "batch" {
		t.Fatalf("expected exactly one batch call (comment+release), got %+v", sender.calls)
	}
	for _, req := range sender.requests {
		for _, o := range req.Operations {
			params, ok := o.Params.(map[string]interface{})
			if !ok {
				continue
			}
			if body, ok := params["body_md"].(string); ok && strings.Contains(body, secret) {
				t.Fatalf("SECURITY: secret leaked verbatim into a posted comment via postProposeOnly: %q", body)
			}
		}
	}
}

// MUTATION-TEST NOTE (finding 6): to prove postProposeOnly's guard.CheckWithConfig call is
// load-bearing, temporarily revert it back to unconditionally building `body` from in.FixBrief
// (dropping the guard.CheckWithConfig call and its rejection branch entirely) and re-run
// TestRunFix_NoRepoConnection_LeakedSecretInFixBriefIsGated — it must go red (the secret reaches
// the posted comment) — then restore the gate.
func TestRunFix_CapsExhausted_SkipsAndReleasesWithoutCloning(t *testing.T) {
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}
	fp := &fakeProvider{}
	caps := NewFixCaps(0, 10, 2, nil) // 0 jobs/day -- immediately exhausted

	r := &FixRunner{
		Journal: journal,
		Client:  sender,
		Caps:    caps,
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{Provider: fp, DefaultBranch: "main"}, true, nil
		},
	}

	in := FixJobInput{JobID: "job-3", IssueID: "issue-3", ProjectID: "proj-3", FixBrief: "x"}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if len(fp.created) != 0 {
		t.Fatal("an exhausted fix-jobs-per-day cap must never reach CreatePR")
	}
	if len(sender.calls) != 1 {
		t.Fatalf("expected the cap-exhaustion comment+release batch, got %+v", sender.calls)
	}
}

// --- Commit-before-push (finding 2): a passing validation must not be pushable without a real
// commit landing on the fix branch -- otherwise a push == baseCommit opens an empty PR. ------------

// TestRunFix_ExecutorOnlyTouchesWorkingTree_StillProducesRealCommitBeforePush is the RED-FIRST
// proof: the stub executor below only writes a file into the working tree (exactly what a real
// coding-agent CLI does -- it edits files, it does not itself `git commit`, plan §4.4's trust
// boundary keeps `git commit`/`git push` on the worker's side of the line). Before finding 2 was
// fixed, RunFix pushed the fix branch straight off ws.BaseCommit -- the branch tip equalled
// baseCommit and the opened PR would have been empty despite validation having "passed". This test
// asserts the pushed branch's tip (read back from the bare origin) is NOT baseCommit and that the
// remote tip's commit is reachable -- i.e. a real commit landed before CreateFixPR was ever called.
func TestRunFix_ExecutorOnlyTouchesWorkingTree_StillProducesRealCommitBeforePush(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}
	fp := &fakeProvider{pr: gitprovider.PR{ID: "1", URL: "https://example/pr/1"}}

	r := &FixRunner{
		WorkspaceRoot: t.TempDir(),
		Journal:       journal,
		Client:        sender,
		Sink:          LocalDirArtifactSink{Root: t.TempDir()},
		Caps:          NewFixCaps(10, 10, 2, nil),
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{
				Provider:      fp,
				Repo:          gitprovider.RepoRef{Provider: "github", Owner: "o", Repo: "r"},
				CloneURL:      bareRepo,
				DefaultBranch: "main",
			}, true, nil
		},
		// Only touches the working tree -- no `git add`/`git commit` of its own, matching the plan
		// §4.4 trust boundary (the Fix Executor never commits/pushes).
		ExecutorCmd: `echo "fix applied" >> fixed.txt`,
	}

	in := FixJobInput{JobID: "job-commit", IssueID: "issue-commit", ProjectID: "proj-1", FixBrief: "x", TriggerSeq: 1}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	if len(fp.created) != 1 {
		t.Fatalf("expected exactly one CreatePR call, got %d", len(fp.created))
	}

	branch := FixBranchName("issue-commit")
	tip := gitRevParse(t, bareRepo, branch)
	if tip == "" {
		t.Fatalf("pushed branch %s not found on origin", branch)
	}
	main := gitRevParse(t, bareRepo, "main")
	if tip == main {
		t.Fatalf("pushed fix branch tip == baseCommit (%s) -- no commit landed before push, this PR would be empty", tip)
	}
}

// TestRunFix_NoOpExecutor_FailsAsEmptyDiffAndNeverPushes proves the other half: an executor that
// changes NOTHING must fail the attempt as an empty-diff validation failure and must never reach
// CreateFixPR -- CommitFixChanges' changed=false path.
func TestRunFix_NoOpExecutor_FailsAsEmptyDiffAndNeverPushes(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}
	fp := &fakeProvider{pr: gitprovider.PR{ID: "1", URL: "https://example/pr/1"}}

	r := &FixRunner{
		WorkspaceRoot: t.TempDir(),
		Journal:       journal,
		Client:        sender,
		Sink:          LocalDirArtifactSink{Root: t.TempDir()},
		Caps:          NewFixCaps(10, 10, 2, nil),
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{
				Provider:      fp,
				Repo:          gitprovider.RepoRef{Provider: "github", Owner: "o", Repo: "r"},
				CloneURL:      bareRepo,
				DefaultBranch: "main",
			}, true, nil
		},
		ExecutorCmd: `true`, // no-op: no file changed
	}

	in := FixJobInput{JobID: "job-noop", IssueID: "issue-noop", ProjectID: "proj-1", FixBrief: "x", TriggerSeq: 1}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	if len(fp.created) != 0 {
		t.Fatal("a no-op executor must never reach CreatePR")
	}
	if _, found, err := ResolveOpenFixPR(journal, "issue-noop"); err != nil || found {
		t.Fatalf("a no-op executor must never journal an open fix-PR: found=%v err=%v", found, err)
	}
}

// gitRevParse reads branch's tip SHA directly from a bare repo path (no RunGit plumbing needed --
// this is test-fixture inspection, not a credentialed operation), returning "" if the ref does not
// exist.
func gitRevParse(t *testing.T, bareRepoDir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", bareRepoDir, "rev-parse", "--verify", ref)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// --- SaveJobArtifacts: on-job-end bundle (plan §4.4 step 3c) -------------------------------------

func TestSaveJobArtifacts_WritesAllFiveArtifacts(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	ws, err := PrepareFixWorkspace(context.Background(), PrepareFixWorkspaceInput{
		WorkspaceRoot: t.TempDir(),
		JobID:         "job-4",
		IssueID:       "issue-4",
		CloneURL:      bareRepo,
		DefaultBranch: "main",
		Cred:          testCred(),
		Brief:         TaskBrief{IssueID: "issue-4", FixBrief: "brief text"},
	})
	if err != nil {
		t.Fatalf("PrepareFixWorkspace: %v", err)
	}

	sink := LocalDirArtifactSink{Root: t.TempDir()}
	result := FixValidationResult{Passed: false, Reason: FixValidReasonEmptyDiff}
	if err := SaveJobArtifacts(context.Background(), sink, ws, FixJobInput{JobID: "job-4"}, result, []byte("agent log line\n")); err != nil {
		t.Fatalf("SaveJobArtifacts: %v", err)
	}

	for _, key := range []string{
		jobAgentLogKey("job-4"),
		jobFinalDiffKey("job-4"),
		jobValidationKey("job-4"),
		jobFinalTaskKey("job-4"),
		jobFinalProgressKey("job-4"),
	} {
		if _, err := sink.Get(context.Background(), key); err != nil {
			t.Errorf("expected artifact %s to be saved: %v", key, err)
		}
	}
}

// --- Live resume save: proves ResumeDebouncer/SaveResumeState are exercised BY RunFix itself ------
// (the validator finding: these had zero non-test callers), not merely reachable in isolation.

func TestRunFix_LiveResumeSave_PersistsProgressLineToSink(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}
	fp := &fakeProvider{pr: gitprovider.PR{ID: "1", URL: "https://example/pr/1"}}
	sink := LocalDirArtifactSink{Root: t.TempDir()}

	r := &FixRunner{
		WorkspaceRoot: t.TempDir(),
		Journal:       journal,
		Client:        sender,
		Sink:          sink,
		Caps:          NewFixCaps(10, 10, 2, nil),
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{
				Provider:      fp,
				Repo:          gitprovider.RepoRef{Provider: "github", Owner: "o", Repo: "r"},
				CloneURL:      bareRepo,
				DefaultBranch: "main",
			}, true, nil
		},
		// A single PROGRESS.md line is enough: ResumeDebouncer always saves on its first call
		// (last-saved starts zero), so this must produce exactly one live SaveResumeState.
		ExecutorCmd: `echo "fix applied" >> fixed.txt && echo "working on it" >> "$PROGRESS_MD"`,
	}

	in := FixJobInput{JobID: "job-live-save", IssueID: "issue-live-save", ProjectID: "proj-1", FixBrief: "x", TriggerSeq: 1}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	progress, err := sink.Get(context.Background(), resumeProgressKey("job-live-save"))
	if err != nil {
		t.Fatalf("expected a live resume-state PROGRESS.md save reachable from RunFix itself: %v", err)
	}
	if !strings.Contains(string(progress), "working on it") {
		t.Fatalf("unexpected saved PROGRESS.md content: %q", progress)
	}
	if _, err := sink.Get(context.Background(), resumeBaseCommitKey("job-live-save")); err != nil {
		t.Fatalf("expected a live resume-state baseCommit save: %v", err)
	}
}

// TestRunFix_ProgressLine_PostedAsIssuesProgressAndJournaled is finding 2's RED-FIRST proof: a
// PROGRESS.md line emitted during a FIX run must be forwarded to Sentinel as a (guard-gated,
// throttled) issues.progress update, AND a journal record of it appended -- before this fix,
// onProgressLine discarded the line text entirely and only drove the live resume-save.
func TestRunFix_ProgressLine_PostedAsIssuesProgressAndJournaled(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	var captured []sentinel.BatchRequest
	sender := &capturingSender{onBatch: func(req sentinel.BatchRequest) { captured = append(captured, req) }}
	fp := &fakeProvider{pr: gitprovider.PR{ID: "1", URL: "https://example/pr/1"}}
	sink := LocalDirArtifactSink{Root: t.TempDir()}

	r := &FixRunner{
		WorkspaceRoot: t.TempDir(),
		Journal:       journal,
		Client:        sender,
		Sink:          sink,
		Caps:          NewFixCaps(10, 10, 2, nil),
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{
				Provider:      fp,
				Repo:          gitprovider.RepoRef{Provider: "github", Owner: "o", Repo: "r"},
				CloneURL:      bareRepo,
				DefaultBranch: "main",
			}, true, nil
		},
		ExecutorCmd: `echo "fix applied" >> fixed.txt && echo "working on it" >> "$PROGRESS_MD"`,
	}

	in := FixJobInput{JobID: "job-progress-post", IssueID: "issue-progress-post", ProjectID: "proj-1", FixBrief: "x", TriggerSeq: 1}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	var found bool
	for _, req := range captured {
		for _, op := range req.Operations {
			if op.Op != "issues.progress" {
				continue
			}
			params, ok := op.Params.(map[string]interface{})
			if !ok {
				continue
			}
			if msg, _ := params["message_md"].(string); strings.Contains(msg, "working on it") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("expected the PROGRESS.md line to be forwarded as an issues.progress message_md update")
	}

	records, _, err := journal.Load()
	if err != nil {
		t.Fatalf("journal.Load: %v", err)
	}
	var journaled bool
	for _, rec := range records {
		if rec.JobID == "job-progress-post:progress" {
			var p FixProgressLinePayload
			if err := json.Unmarshal(rec.Payload, &p); err == nil && strings.Contains(p.Line, "working on it") {
				journaled = true
			}
		}
	}
	if !journaled {
		t.Fatal("expected a journal record of the posted PROGRESS.md line")
	}
}

// --- ResumeFix: restart-time resume re-invokes the executor without counting a fresh attempt ------

func TestResumeFix_UsesSavedStateAndDoesNotCountAFreshAttempt(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	workspaceRoot := t.TempDir()
	ctx := context.Background()

	// Simulate a prior, crashed attempt: prepare a workspace, make a partial change, and save its
	// live resume state -- exactly what watchLiveResumeSave would have done before the crash.
	ws1, err := PrepareFixWorkspace(ctx, PrepareFixWorkspaceInput{
		WorkspaceRoot: workspaceRoot,
		JobID:         "job-resume",
		IssueID:       "issue-resume",
		CloneURL:      bareRepo,
		DefaultBranch: "main",
		Cred:          testCred(),
		Brief:         TaskBrief{IssueID: "issue-resume", FixBrief: "dereference guarded now"},
	})
	if err != nil {
		t.Fatalf("PrepareFixWorkspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws1.RepoDir, "fixed.txt"), []byte("partial fix\n"), 0o644); err != nil {
		t.Fatalf("seed partial fix: %v", err)
	}
	sink := LocalDirArtifactSink{Root: t.TempDir()}
	if err := SaveResumeState(ctx, sink, ws1, testCred(), nil); err != nil {
		t.Fatalf("SaveResumeState: %v", err)
	}
	if err := CleanupFixWorkspace(ws1); err != nil {
		t.Fatalf("CleanupFixWorkspace: %v", err)
	}

	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}
	fp := &fakeProvider{pr: gitprovider.PR{ID: "9", URL: "https://example/pr/9"}}
	caps := NewFixCaps(10, 10, 2, nil)
	caps.RecordAttempt("job-resume") // the crashed first attempt already consumed one slot

	r := &FixRunner{
		WorkspaceRoot: workspaceRoot,
		Journal:       journal,
		Client:        sender,
		Sink:          sink,
		Caps:          caps,
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{
				Provider:      fp,
				Repo:          gitprovider.RepoRef{Provider: "github", Owner: "o", Repo: "r"},
				CloneURL:      bareRepo,
				DefaultBranch: "main",
			}, true, nil
		},
		ExecutorCmd: `true`, // the resumed workspace already carries the change via diff.patch
	}

	in := FixJobInput{JobID: "job-resume", IssueID: "issue-resume", ProjectID: "proj-1", FixBrief: "dereference guarded now", TriggerSeq: 1}
	if err := r.ResumeFix(ctx, in); err != nil {
		t.Fatalf("ResumeFix: %v", err)
	}

	if len(fp.created) != 1 {
		t.Fatalf("expected exactly one CreatePR call from the resumed attempt, got %d", len(fp.created))
	}
	if _, found, err := ResolveOpenFixPR(journal, "issue-resume"); err != nil || !found {
		t.Fatalf("expected ResumeFix to journal an open fix-PR: found=%v err=%v", found, err)
	}
	if got := caps.AttemptCount("job-resume"); got != 1 {
		t.Fatalf("ResumeFix must not record a fresh attempt: attempt count = %d, want 1", got)
	}
}

// TestRunFix_MaxVerbatim_ThreadsFromFixRunnerToGuard is the fix_pr real-path counterpart to
// jobs/act_test.go's TestCompileTriage_MaxVerbatim_ThreadsFromActContext, driven through the ACTUAL
// RunFix call path (fix.go:417's `BuildFixPRSpec(..., r.MaxVerbatim)`), not a direct call to
// BuildFixPRSpec — a prior version of this coverage called BuildFixPRSpec directly and a mutation
// hardcoding fix.go:417's argument to 0 survived undetected, because nothing exercised the actual
// threading site.
//
// The FixBrief is engineered to ~40% verbatim coverage of the ToolOutputs corpus: rejected by
// guard.CheckWithConfig at the default 0.25 threshold (r.MaxVerbatim left zero-valued), allowed
// once r.MaxVerbatim is raised to 0.5 — same FixRunner shape, same candidate, only the configured
// cap differs. Mutation-proof: hardcode fix.go:417's `r.MaxVerbatim` argument to `0` and the
// second (MaxVerbatim=0.5) sub-test must go red because BuildFixPRSpec then falls back to
// guard.DefaultMaxVerbatim regardless of r.MaxVerbatim.
func TestRunFix_MaxVerbatim_ThreadsFromFixRunnerToGuard(t *testing.T) {
	verbatimBlock := strings.Repeat("Z", 40) // one 40-byte run, above guard's 8-byte k-gram floor
	toolOutput := "some other unrelated tool output content around it " + verbatimBlock + " and more unrelated content"
	filler := "the quick brown fox jumps over the lazy dog 0123456789 zyx" // 60 bytes, no overlap
	fixBrief := verbatimBlock + filler                                     // ~40/100 bytes verbatim

	run := func(t *testing.T, maxVerbatim float64) (prCreated bool) {
		t.Helper()
		bareRepo := newBareFixtureRepo(t)
		journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
		sender := &recordingSender{}
		fp := &fakeProvider{pr: gitprovider.PR{ID: "1", Number: 1, URL: "https://example/pr/1"}}
		sink := LocalDirArtifactSink{Root: t.TempDir()}

		r := &FixRunner{
			WorkspaceRoot: t.TempDir(),
			Journal:       journal,
			Client:        sender,
			Sink:          sink,
			Caps:          NewFixCaps(10, 10, 2, nil),
			ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
				return FixRepoConfig{
					Provider:      fp,
					Repo:          gitprovider.RepoRef{Provider: "github", Owner: "o", Repo: "r"},
					CloneURL:      bareRepo,
					DefaultBranch: "main",
				}, true, nil
			},
			ExecutorCmd: `echo "fix applied" >> fixed.txt && echo "applied fix" >> "$PROGRESS_MD"`,
			MaxVerbatim: maxVerbatim,
		}

		in := FixJobInput{
			JobID:       "job-verbatim",
			IssueID:     "issue-verbatim",
			ProjectID:   "proj-1",
			ErrorClass:  "NilPointerException",
			FixBrief:    fixBrief,
			ToolOutputs: []string{toolOutput},
			TriggerSeq:  1,
		}

		if err := r.RunFix(context.Background(), in); err != nil {
			t.Logf("RunFix returned error (expected when the gate rejects): %v", err)
		}
		return len(fp.created) == 1
	}

	t.Run("default_0.25_rejects_~40pct_verbatim", func(t *testing.T) {
		if run(t, 0) {
			t.Fatal("expected the default 0.25 verbatim cap to reject a ~40 percent verbatim FixBrief, but a PR was created")
		}
	})

	t.Run("raised_0.5_allows_~40pct_verbatim", func(t *testing.T) {
		if !run(t, 0.5) {
			t.Fatal("expected r.MaxVerbatim=0.5 to allow a ~40 percent verbatim FixBrief through to CreatePR, but none was created")
		}
	})
}

// --- Fix-lifecycle remediation round 2 -----------------------------------------------------------

// TestRunFix_ValidationFailure_JournalsTerminalAndIsNotResumed is the RED-FIRST proof for finding 1
// (BLOCKER): before this fix, a FIX attempt that failed validation returned via releaseWithComment
// having journaled ONLY the non-terminal journalFixRunning record for in.JobID -- nothing ever
// closed it out terminal. state.Journal.RecoveryScan would therefore keep surfacing this exact
// jobID as in-flight (state.StateFixRunning) forever, and main.go's resumeInFlightJob would drive
// it straight back into ResumeFix on every subsequent restart, re-running (and re-failing) the
// identical dead job forever. This asserts BOTH halves directly against the real journal: (1) the
// job's LATEST record is a terminal state (state.StateFailed), and (2) a RecoveryScan taken AFTER
// the failed attempt no longer includes this jobID.
//
// MUTATION-TEST NOTE: revert releaseWithComment to not journal a terminal record (its pre-fix
// shape) and this test goes red on both assertions -- LatestByJobID reports state.StateFixRunning
// (non-terminal) as the latest record, and RecoveryScan's second call still contains the job.
func TestRunFix_ValidationFailure_JournalsTerminalAndIsNotResumed(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}
	fp := &fakeProvider{pr: gitprovider.PR{ID: "1", URL: "https://example/pr/1"}}

	r := &FixRunner{
		WorkspaceRoot: t.TempDir(),
		Journal:       journal,
		Client:        sender,
		Sink:          LocalDirArtifactSink{Root: t.TempDir()},
		Caps:          NewFixCaps(10, 10, 2, nil),
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{
				Provider:      fp,
				Repo:          gitprovider.RepoRef{Provider: "github", Owner: "o", Repo: "r"},
				CloneURL:      bareRepo,
				DefaultBranch: "main",
				TestCmd:       "false", // always fails -- guarantees ValidateFix reports FixValidReasonTestsFailed
			}, true, nil
		},
		// A real change, so this exercises the test-command failure path specifically, not the
		// (already covered) empty-diff path.
		ExecutorCmd: `echo "fix applied" >> fixed.txt`,
	}

	in := FixJobInput{JobID: "job-valfail", IssueID: "issue-valfail", ProjectID: "proj-1", FixBrief: "x", TriggerSeq: 1}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	if len(fp.created) != 0 {
		t.Fatal("a validation failure must never reach CreatePR")
	}

	latest, err := journal.LatestByJobID()
	if err != nil {
		t.Fatalf("LatestByJobID: %v", err)
	}
	rec, ok := latest["job-valfail"]
	if !ok {
		t.Fatal("expected a journal record for job-valfail")
	}
	if !rec.State.IsTerminal() {
		t.Fatalf("expected job-valfail's latest record to be terminal, got state=%q (RecoveryScan will re-surface this forever)", rec.State)
	}
	if rec.State != state.StateFailed {
		t.Fatalf("expected job-valfail to terminate as state.StateFailed, got %q", rec.State)
	}

	inFlight, _, err := journal.RecoveryScan()
	if err != nil {
		t.Fatalf("RecoveryScan: %v", err)
	}
	for _, job := range inFlight {
		if job.JobID == "job-valfail" {
			t.Fatal("a validation-failed FIX job must NOT be re-surfaced by RecoveryScan as in-flight")
		}
	}
}

// TestRunFix_CapsExhausted_JournalsTerminal is finding 1's coverage for the pre-workspace exit
// paths (attempt-cap/job-cap exhaustion): these never even journal journalFixRunning's non-terminal
// marker (no workspace exists yet), but must still leave a terminal record so a duplicate-delivered
// event for the same jobID is correctly deduped as terminal (state.Journal.IsDuplicate) rather than
// silently having no record at all.
func TestRunFix_CapsExhausted_JournalsTerminal(t *testing.T) {
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}
	fp := &fakeProvider{}
	caps := NewFixCaps(0, 10, 2, nil) // 0 jobs/day -- immediately exhausted

	r := &FixRunner{
		Journal: journal,
		Client:  sender,
		Caps:    caps,
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{Provider: fp, DefaultBranch: "main"}, true, nil
		},
	}

	in := FixJobInput{JobID: "job-capfail", IssueID: "issue-capfail", ProjectID: "proj-3", FixBrief: "x"}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	latest, err := journal.LatestByJobID()
	if err != nil {
		t.Fatalf("LatestByJobID: %v", err)
	}
	rec, ok := latest["job-capfail"]
	if !ok || !rec.State.IsTerminal() {
		t.Fatalf("expected a terminal journal record for job-capfail, got %+v (found=%v)", rec, ok)
	}
}

// TestRunFix_ResolveRepoError_ReleasesClaim is the RED-FIRST proof for finding 5 (MINOR): a
// ResolveRepo error (repo connection exists, credential unusable) must release the claim like
// every other RunFix failure tail, not strand it for WORKER_NAG_DAYS.
//
// MUTATION-TEST NOTE: revert the ResolveRepo-error branch to `return fmt.Errorf(...)` (its pre-fix
// shape) and this test goes red -- sender.calls stays empty (no comment+release batch posted) and
// the returned error becomes non-nil.
func TestRunFix_ResolveRepoError_ReleasesClaim(t *testing.T) {
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}

	r := &FixRunner{
		Journal: journal,
		Client:  sender,
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{}, false, fmt.Errorf("credential expired")
		},
	}

	in := FixJobInput{JobID: "job-resolveerr", IssueID: "issue-resolveerr", ProjectID: "proj-1", FixBrief: "x"}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: expected a nil error (the failure is reported via the release-with-comment batch, not a returned error), got %v", err)
	}

	if len(sender.calls) != 1 || sender.calls[0] != "batch" {
		t.Fatalf("expected exactly one comment+release batch call, got %+v", sender.calls)
	}

	latest, err := journal.LatestByJobID()
	if err != nil {
		t.Fatalf("LatestByJobID: %v", err)
	}
	rec, ok := latest["job-resolveerr"]
	if !ok || rec.State != state.StateFailed {
		t.Fatalf("expected a terminal StateFailed record for job-resolveerr, got %+v (found=%v)", rec, ok)
	}
}

// TestFixCaps_PerIssueAttemptCap_BoundsAcrossDistinctJobIDs is the RED-FIRST proof for finding 6:
// WORKER_MAX_FIX_ATTEMPTS is documented (plan §2.6) as a PER-ISSUE cap, but AllowAttempt/
// RecordAttempt alone are keyed by jobID = JobID(FixKind, issue, triggerSeq) -- a fresh trigger
// mints a fresh jobID with its OWN full attempt budget. This proves the same issueID re-triggering
// a FIX under THREE DISTINCT jobIDs (simulating occurrence_burst, then regressed, then another
// occurrence_burst) is bounded by the real per-issue cap once AllowIssueAttempt is consulted
// alongside AllowAttempt, even though each individual jobID never exhausts its OWN per-jobID cap.
//
// This test exercises FixCaps directly (not RunFix) and so only proves the cap semantics in
// isolation; it does NOT exercise RunFix's gate at jobs/fix.go:319 and is not, by itself,
// sensitive to that gate being removed. See TestRunFix_PerIssueCap_RealPath_BoundsAcrossDistinctJobIDs
// below for the real-path (RunFix-level) mutation proof that jobs/fix.go:319's
// `|| !r.Caps.AllowIssueAttempt(in.IssueID)` clause is load-bearing.
func TestFixCaps_PerIssueAttemptCap_BoundsAcrossDistinctJobIDs(t *testing.T) {
	caps := NewFixCaps(10, 10, 2, nil) // maxAttempts=2, per issue AND per jobID
	issueID := "issue-repeat-trigger"

	if !caps.AllowAttempt("job-1") || !caps.AllowIssueAttempt(issueID) {
		t.Fatal("first attempt (job-1) should be allowed")
	}
	caps.RecordAttempt("job-1")
	caps.RecordIssueAttempt(issueID)

	// A SECOND distinct jobID for the SAME issue (a re-trigger) -- job-1's own per-jobID counter is
	// irrelevant here (job-2 has never attempted), but the per-issue counter is now at 1.
	if !caps.AllowAttempt("job-2") || !caps.AllowIssueAttempt(issueID) {
		t.Fatal("second attempt (job-2, same issue) should be allowed -- issue attempt count is 1, cap is 2")
	}
	caps.RecordAttempt("job-2")
	caps.RecordIssueAttempt(issueID)

	// A THIRD distinct jobID for the SAME issue: job-3's own per-jobID counter is still zero (it has
	// never attempted before), so AllowAttempt("job-3") alone would say yes -- only the per-issue
	// cap (now at 2, the configured max) can catch this.
	if caps.AllowAttempt("job-3") && caps.AllowIssueAttempt(issueID) {
		t.Fatal("third attempt for the SAME issue under a brand-new jobID must be rejected by the per-issue cap, even though job-3 itself has zero prior attempts")
	}
	if got := caps.IssueAttemptCount(issueID); got != 2 {
		t.Fatalf("IssueAttemptCount(%s) = %d, want 2", issueID, got)
	}
}

// TestRunFix_PerIssueCap_RealPath_BoundsAcrossDistinctJobIDs is the validator-required real-path
// (RunFix-level, not FixCaps-unit) proof for finding 6: it drives RunFix itself, not FixCaps
// methods called directly, so it is sensitive to the actual gate wired at jobs/fix.go:319.
//
// Setup: a FixCaps with maxAttempts=2, with the issue's per-issue budget already fully consumed by
// two RecordIssueAttempt calls (simulating two prior attempts under two earlier, distinct jobIDs --
// exactly what would happen across two occurrence_burst/regressed re-triggers). RunFix is then
// called with a THIRD, brand-new jobID for the SAME issueID; that jobID's own per-jobID counter is
// at zero (AllowAttempt alone would allow it), so only the per-issue gate can reject it.
//
// MUTATION-TEST NOTE: remove the `|| !r.Caps.AllowIssueAttempt(in.IssueID)` clause from RunFix
// (jobs/fix.go:319, restoring its pre-fix shape) and this test goes red -- fp.created would gain an
// entry (RunFix would sail through to CreatePR) instead of staying empty.
func TestRunFix_PerIssueCap_RealPath_BoundsAcrossDistinctJobIDs(t *testing.T) {
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}
	fp := &fakeProvider{pr: gitprovider.PR{ID: "1", URL: "https://example/pr/1"}}
	caps := NewFixCaps(10, 10, 2, nil) // maxAttempts=2, per issue AND per jobID

	const issueID = "issue-real-path-repeat-trigger"
	// Simulate two prior attempts under two earlier, distinct jobIDs -- the issue's per-issue
	// budget (cap=2) is now fully consumed, even though neither "job-1" nor "job-2" is the jobID
	// RunFix will be called with below.
	caps.RecordAttempt("job-1")
	caps.RecordIssueAttempt(issueID)
	caps.RecordAttempt("job-2")
	caps.RecordIssueAttempt(issueID)

	r := &FixRunner{
		Journal: journal,
		Client:  sender,
		Caps:    caps,
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{Provider: fp, DefaultBranch: "main"}, true, nil
		},
	}

	// job-3: a brand-new jobID (its own per-jobID counter is zero) for the SAME issue.
	in := FixJobInput{JobID: "job-3", IssueID: issueID, ProjectID: "proj-1", FixBrief: "x", TriggerSeq: 3}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	if len(fp.created) != 0 {
		t.Fatalf("a per-issue-cap-exhausted issue must never reach CreatePR even under a fresh jobID, got %d CreatePR calls", len(fp.created))
	}
	if len(sender.calls) != 1 || sender.calls[0] != "batch" {
		t.Fatalf("expected exactly one comment+release batch call, got %+v", sender.calls)
	}

	latest, err := journal.LatestByJobID()
	if err != nil {
		t.Fatalf("LatestByJobID: %v", err)
	}
	rec, ok := latest["job-3"]
	if !ok || rec.State != state.StateSkipped {
		t.Fatalf("expected a terminal StateSkipped record for job-3, got %+v (found=%v)", rec, ok)
	}
}

// --- FIX subsystem remediation round (10 findings) -----------------------------------------------

// TestRunFix_JournalsFixRunningBeforeWorkspacePrep is finding 4's RED-FIRST proof: a crash inside
// PrepareFixWorkspace (simulated here by pointing ResolveRepo's CloneURL at a location that makes
// the clone fail) must still leave a State==state.StateFixRunning marker in the journal for this
// jobID -- proving the write happens BEFORE PrepareFixWorkspace runs/fails, not only afterward (the
// pre-fix shape journaled it inside executeValidatePublish, which a failed PrepareFixWorkspace never
// reaches at all, so NO non-terminal marker existed anywhere in the journal for this jobID).
//
// MUTATION-TEST NOTE: revert the journalFixRunning call back into executeValidatePublish only (its
// pre-fix position) and this test goes red: journal.Load() contains no StateFixRunning record for
// job-earlywrite at all, since PrepareFixWorkspace below always fails before reaching that call.
func TestRunFix_JournalsFixRunningBeforeWorkspacePrep(t *testing.T) {
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}
	fp := &fakeProvider{}

	r := &FixRunner{
		WorkspaceRoot: t.TempDir(),
		Journal:       journal,
		Client:        sender,
		Caps:          NewFixCaps(10, 10, 2, nil),
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			// A clone URL that cannot possibly succeed -- PrepareFixWorkspace must fail here.
			return FixRepoConfig{
				Provider:      fp,
				Repo:          gitprovider.RepoRef{Provider: "github", Owner: "o", Repo: "r"},
				CloneURL:      filepath.Join(t.TempDir(), "does-not-exist"),
				DefaultBranch: "main",
			}, true, nil
		},
	}

	in := FixJobInput{JobID: "job-earlywrite", IssueID: "issue-earlywrite", ProjectID: "proj-1", FixBrief: "x", TriggerSeq: 1}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	records, _, err := journal.Load()
	if err != nil {
		t.Fatalf("journal.Load: %v", err)
	}
	var sawRunning bool
	for _, rec := range records {
		if rec.JobID == "job-earlywrite" && rec.State == state.StateFixRunning {
			sawRunning = true
		}
	}
	if !sawRunning {
		t.Fatal("expected a state.StateFixRunning journal record for job-earlywrite written before the (failing) workspace prep, but found none")
	}
}

// TestFixCaps_SeedToday_ResumedRecordDoesNotDoubleCountAttempt is finding 5's RED-FIRST proof: a
// FIX job that journals a fresh (Resumed=false) attempt-start marker AND a later resumed
// (Resumed=true) marker for the SAME jobID/issueID (a crash-resume of that same attempt) must seed
// exactly ONE attempt against WORKER_MAX_FIX_ATTEMPTS -- not two -- on the next boot's SeedToday
// reconstruction.
//
// MUTATION-TEST NOTE: revert SeedToday's `if !resumed` guard (increment unconditionally, as before
// finding 5) and this test goes red: AttemptCount/IssueAttemptCount report 2, not 1.
func TestFixCaps_SeedToday_ResumedRecordDoesNotDoubleCountAttempt(t *testing.T) {
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	// journalFixRunning stamps records via journal.Append at the real wall clock, so SeedToday's
	// "today" must be anchored to that same wall day -- a frozen past date would make isToday filter
	// out every just-written record and seed all counts as zero (date-fragile: passes only on the
	// hard-coded day).
	now := time.Now().UTC()

	in := FixJobInput{JobID: "job-resume-seed", IssueID: "issue-resume-seed", ProjectID: "proj-1", TriggerSeq: 1}
	if err := journalFixRunning(journal, in, "", false); err != nil { // fresh attempt start
		t.Fatalf("journalFixRunning(fresh): %v", err)
	}
	if err := journalFixRunning(journal, in, "", true); err != nil { // crash-resume of the SAME attempt
		t.Fatalf("journalFixRunning(resumed): %v", err)
	}

	records, _, err := journal.Load()
	if err != nil {
		t.Fatalf("journal.Load: %v", err)
	}

	caps := NewFixCaps(100, 100, 2, fixedClock(now))
	caps.SeedToday(records, now)

	if got := caps.AttemptCount("job-resume-seed"); got != 1 {
		t.Fatalf("AttemptCount after one fresh start + one resume = %d, want 1 (the resume must not count again)", got)
	}
	if got := caps.IssueAttemptCount("issue-resume-seed"); got != 1 {
		t.Fatalf("IssueAttemptCount after one fresh start + one resume = %d, want 1", got)
	}
}

// TestFixRunner_ExecutorEnvSecret_RedactedInAgentLog is finding 6's RED-FIRST proof at the seam
// this package actually owns: r.Secrets (populated in production from main.go's configuredSecrets,
// which finding 6 fixes to include Config.WorkerFixExecutorEnv's values) must reach the redactor
// that wraps the Fix Executor's own log writer, so a coding-agent credential handed to the executor
// via ExecutorEnv never appears in cleartext in the agent log RunFix captures.
//
// This exercises the package-local half of finding 6 directly (r.Secrets -> jobSecrets ->
// logRedactor in RunFix): with the executor's secret value present in r.Secrets, an executor script
// that echoes it into its own stdout must come back redacted in the saved agent-log artifact.
// main.go's configuredSecrets is covered by TestConfiguredSecrets_IncludesFixExecutorEnvValues in
// main_test.go.
func TestFixRunner_ExecutorEnvSecret_RedactedInAgentLog(t *testing.T) {
	const executorSecret = "sk-fix-executor-super-secret-token"
	bareRepo := newBareFixtureRepo(t)
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}
	fp := &fakeProvider{pr: gitprovider.PR{ID: "1", URL: "https://example/pr/1"}}
	sink := LocalDirArtifactSink{Root: t.TempDir()}

	r := &FixRunner{
		WorkspaceRoot: t.TempDir(),
		Journal:       journal,
		Client:        sender,
		Sink:          sink,
		Caps:          NewFixCaps(10, 10, 2, nil),
		ResolveRepo: func(projectID string) (FixRepoConfig, bool, error) {
			return FixRepoConfig{
				Provider:      fp,
				Repo:          gitprovider.RepoRef{Provider: "github", Owner: "o", Repo: "r"},
				CloneURL:      bareRepo,
				DefaultBranch: "main",
			}, true, nil
		},
		// Echoes the "executor's own credential" into stdout/agent log, then makes a real commit so
		// the attempt reaches PR creation and SaveJobArtifacts writes the agent log.
		ExecutorCmd: `echo "using token ` + executorSecret + `" && echo "fix" >> fixed.txt`,
		Secrets:     []string{executorSecret}, // finding 6: this is what configuredSecrets must supply
	}

	in := FixJobInput{JobID: "job-secret-redact", IssueID: "issue-secret-redact", ProjectID: "proj-1", FixBrief: "x", TriggerSeq: 1}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if len(fp.created) != 1 {
		t.Fatalf("expected exactly one CreatePR call, got %d", len(fp.created))
	}

	agentLog, err := sink.Get(context.Background(), jobAgentLogKey("job-secret-redact"))
	if err != nil {
		t.Fatalf("reading saved agent log: %v", err)
	}
	if strings.Contains(string(agentLog), executorSecret) {
		t.Fatalf("expected the executor's own secret to be redacted from the saved agent log, got: %s", agentLog)
	}
}
