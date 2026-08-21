package jobs

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// --- finding 3: graceful shutdown waits for in-flight FIX goroutines ----------------------------

// TestFixRunner_Wait_BlocksUntilDispatchedGoroutineFinishes is finding 3's red-first proof on the
// REAL Dispatch path (not a hand-built goroutine): Dispatch is called exactly as RealActor.Act
// calls it, and Wait must not return before the dispatched RunFix has actually completed --
// including its resume-save. main.go's shutdown path calls Wait the same way, bounded by
// WORKER_SHUTDOWN_TIMEOUT, after draining the dispatcher.
func TestFixRunner_Wait_BlocksUntilDispatchedGoroutineFinishes(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}
	fp := &fakeProvider{pr: gitprovider.PR{ID: "1", Number: 1, URL: "https://example/pr/1"}}
	sink := LocalDirArtifactSink{Root: t.TempDir()}

	var finished atomic.Bool
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
		// A slow-ish executor so Wait genuinely has to block on it rather than racing a
		// near-instant goroutine.
		ExecutorCmd: `sleep 0.3 && echo "fix" >> fixed.txt && echo "applied" >> "$PROGRESS_MD"`,
		OnEvent: func(event string, in FixJobInput, detail string) {
			if event == "pr-opened" {
				finished.Store(true)
			}
		},
	}

	r.Dispatch(FixJobInput{JobID: "job-wait", IssueID: "issue-wait", ProjectID: "proj-wait", TriggerSeq: 1})

	// Wait must not return before the dispatched goroutine's pr-opened event fired.
	r.Wait(context.Background())
	if !finished.Load() {
		t.Fatal("Wait returned before the dispatched FIX goroutine finished (pr-opened event never observed)")
	}
}

// TestFixRunner_Wait_BoundedByCallerContext proves Wait respects a bounded context (the
// WORKER_SHUTDOWN_TIMEOUT the caller supplies) rather than blocking forever on a wedged attempt.
func TestFixRunner_Wait_BoundedByCallerContext(t *testing.T) {
	r := &FixRunner{}
	r.wg.Add(1) // never Done() -- simulates a wedged in-flight goroutine

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	r.Wait(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Wait should have returned once ctx timed out, took %v", elapsed)
	}
}

// TestFixRunner_Wait_NilReceiverIsNoOp lets main.go call fixRunner.Wait(ctx) unconditionally even
// when the FIX engine is deployment-disabled (buildFixRunner returns a real nil *jobs.FixRunner).
func TestFixRunner_Wait_NilReceiverIsNoOp(t *testing.T) {
	var r *FixRunner
	done := make(chan struct{})
	go func() {
		r.Wait(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait on a nil *FixRunner should return immediately")
	}
}

// --- finding 5: C16 re-fetch-credentials-on-git-auth-failure ------------------------------------

// TestRunFix_AuthFailureOnCreatePR_TriggersExactlyOneOnAuthFailure is finding 5's red-first proof
// on the real path: CreatePR (reached from RunFix's real executeValidatePublish tail, after a real
// git clone/commit) returns a classified gitprovider.Error with Class==ClassAuthFailure, and
// RunFix must call OnAuthFailure exactly once.
func TestRunFix_AuthFailureOnCreatePR_TriggersExactlyOneOnAuthFailure(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}
	fp := &fakeProvider{createErr: &gitprovider.Error{
		Provider: "github", Op: "CreatePR", Status: 401, Class: sentinel.ClassAuthFailure,
	}}
	sink := LocalDirArtifactSink{Root: t.TempDir()}

	var calls int
	var lastProvider string
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
		OnAuthFailure: func(provider string) {
			calls++
			lastProvider = provider
		},
	}

	in := FixJobInput{JobID: "job-auth", IssueID: "issue-auth", ProjectID: "proj-auth", TriggerSeq: 1}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}

	if calls != 1 {
		t.Fatalf("expected OnAuthFailure called exactly once, got %d", calls)
	}
	if lastProvider != "github" {
		t.Fatalf("expected OnAuthFailure(\"github\"), got %q", lastProvider)
	}
}

// TestRunFix_NonAuthFailureOnCreatePR_DoesNotTriggerOnAuthFailure is the mutation-test companion:
// a non-auth (e.g. 500) provider error must NOT invoke OnAuthFailure.
func TestRunFix_NonAuthFailureOnCreatePR_DoesNotTriggerOnAuthFailure(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &recordingSender{}
	fp := &fakeProvider{createErr: &gitprovider.Error{Provider: "github", Op: "CreatePR", Status: 500}}
	sink := LocalDirArtifactSink{Root: t.TempDir()}

	var calls int
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
		ExecutorCmd:   `echo "fix applied" >> fixed.txt && echo "applied fix" >> "$PROGRESS_MD"`,
		OnAuthFailure: func(provider string) { calls++ },
	}

	in := FixJobInput{JobID: "job-noauth", IssueID: "issue-noauth", ProjectID: "proj-noauth", TriggerSeq: 1}
	if err := r.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if calls != 0 {
		t.Fatalf("a non-auth-failure provider error must not trigger OnAuthFailure, got %d calls", calls)
	}
}
