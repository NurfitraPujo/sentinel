package jobs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
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
