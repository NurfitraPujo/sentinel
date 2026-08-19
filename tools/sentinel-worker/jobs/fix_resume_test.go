package jobs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- LocalDirArtifactSink ------------------------------------------------------------------

func TestLocalDirArtifactSink_PutGetRoundTrip(t *testing.T) {
	sink := LocalDirArtifactSink{Root: t.TempDir()}
	ctx := context.Background()
	if err := sink.Put(ctx, "fix-artifacts/job-1/TASK.md", []byte("hello")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := sink.Get(ctx, "fix-artifacts/job-1/TASK.md")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestLocalDirArtifactSink_GetMissingReturnsErrArtifactNotFound(t *testing.T) {
	sink := LocalDirArtifactSink{Root: t.TempDir()}
	_, err := sink.Get(context.Background(), "fix-artifacts/job-x/TASK.md")
	if err != ErrArtifactNotFound {
		t.Fatalf("got %v, want ErrArtifactNotFound", err)
	}
}

// --- Index isolation (finding 5): the live-save path must never touch the real .git/index --------

// TestSaveResumeState_DoesNotTouchRealIndex is the RED-FIRST proof for finding 5: the live
// resume-state save runs concurrently with the Fix Executor's own subprocess in the SAME RepoDir
// (fix.go's watchLiveResumeSave). Before the fix, SaveResumeState called ComputeDiffPatch, whose
// `git add -A` staged directly into repoDir/.git/index -- an index.lock race against whatever the
// executor itself is concurrently staging/committing, and a silent mutation of state the executor
// does not expect a bystander to touch. This asserts the real index file's content and mtime are
// byte-for-byte unchanged by a SaveResumeState call.
//
// MUTATION-TEST NOTE (per task brief): change SaveResumeState back to call ComputeDiffPatch instead
// of gitDiffPatchIsolated -- this test must go red, because ComputeDiffPatch's `git add -A` stages
// the new/changed file below into the real index, changing both its content and its mtime.
func TestSaveResumeState_DoesNotTouchRealIndex(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	ctx := context.Background()

	ws, err := PrepareFixWorkspace(ctx, PrepareFixWorkspaceInput{
		WorkspaceRoot: t.TempDir(),
		JobID:         "job-index-race",
		IssueID:       "issue-index-race",
		CloneURL:      bareRepo,
		DefaultBranch: "main",
		Cred:          testCred(),
		Brief:         TaskBrief{IssueID: "issue-index-race", FixBrief: "x"},
	})
	if err != nil {
		t.Fatalf("PrepareFixWorkspace: %v", err)
	}
	// A change SaveResumeState's underlying diff computation would otherwise stage, exactly like a
	// Fix Executor mid-run: a new, never-`git add`ed file.
	if err := os.WriteFile(filepath.Join(ws.RepoDir, "in-progress.txt"), []byte("partial work\n"), 0o644); err != nil {
		t.Fatalf("seed working-tree change: %v", err)
	}

	indexPath := filepath.Join(ws.RepoDir, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("reading real index before save: %v", err)
	}
	beforeInfo, err := os.Stat(indexPath)
	if err != nil {
		t.Fatalf("stat real index before save: %v", err)
	}

	sink := LocalDirArtifactSink{Root: t.TempDir()}
	if err := SaveResumeState(ctx, sink, ws, testCred(), nil); err != nil {
		t.Fatalf("SaveResumeState: %v", err)
	}

	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("reading real index after save: %v", err)
	}
	afterInfo, err := os.Stat(indexPath)
	if err != nil {
		t.Fatalf("stat real index after save: %v", err)
	}

	if !bytes.Equal(before, after) {
		t.Fatalf("SaveResumeState mutated the real .git/index content (%d bytes -> %d bytes) -- it must operate on an isolated index only", len(before), len(after))
	}
	if !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatalf("SaveResumeState touched the real .git/index mtime (%v -> %v) -- it must never open the real index for write", beforeInfo.ModTime(), afterInfo.ModTime())
	}

	// The saved diff must still correctly report the working-tree change -- isolation must not come
	// at the cost of correctness.
	saved, err := sink.Get(ctx, resumeDiffKey("job-index-race"))
	if err != nil {
		t.Fatalf("expected a saved diff: %v", err)
	}
	if !strings.Contains(string(saved), "in-progress.txt") {
		t.Fatalf("saved diff does not mention the new file: %q", saved)
	}
}

// --- SaveResumeState / LoadResumeState round trip ---------------------------------------------

func TestSaveAndLoadResumeState_RoundTrip(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	ws, err := PrepareFixWorkspace(context.Background(), PrepareFixWorkspaceInput{
		WorkspaceRoot: t.TempDir(),
		JobID:         "job-resume-1",
		IssueID:       "issue-1",
		CloneURL:      bareRepo,
		DefaultBranch: "main",
		Cred:          testCred(),
		Brief:         TaskBrief{IssueID: "issue-1", FixBrief: "do the thing", TestCmd: "true"},
	})
	if err != nil {
		t.Fatalf("PrepareFixWorkspace: %v", err)
	}
	if err := os.WriteFile(ws.ProgressPath, []byte("read handler.go\n"), 0o600); err != nil {
		t.Fatalf("seed PROGRESS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.RepoDir, "fix.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("seed change: %v", err)
	}

	sink := LocalDirArtifactSink{Root: t.TempDir()}
	if err := SaveResumeState(context.Background(), sink, ws, testCred(), nil); err != nil {
		t.Fatalf("SaveResumeState: %v", err)
	}

	loaded, found, err := LoadResumeState(context.Background(), sink, "job-resume-1")
	if err != nil {
		t.Fatalf("LoadResumeState: %v", err)
	}
	if !found {
		t.Fatal("expected resume state to be found")
	}
	if loaded.BaseCommit != ws.BaseCommit {
		t.Fatalf("BaseCommit = %q, want %q", loaded.BaseCommit, ws.BaseCommit)
	}
	if loaded.ProgressMD != "read handler.go\n" {
		t.Fatalf("ProgressMD = %q", loaded.ProgressMD)
	}
	if len(loaded.DiffPatch) == 0 {
		t.Fatal("expected a non-empty diff.patch capturing the seeded fix.go change")
	}
}

func TestLoadResumeState_NotFound(t *testing.T) {
	sink := LocalDirArtifactSink{Root: t.TempDir()}
	_, found, err := LoadResumeState(context.Background(), sink, "job-never-saved")
	if err != nil {
		t.Fatalf("LoadResumeState: %v", err)
	}
	if found {
		t.Fatal("expected not found for a jobID nothing was ever saved under")
	}
}

// --- ResumeDebouncer ----------------------------------------------------------------------

func TestResumeDebouncer_DebouncesWithinInterval(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d := &ResumeDebouncer{Interval: time.Minute, Now: func() time.Time { return now }}
	if !d.ShouldSave() {
		t.Fatal("first call should always save")
	}
	if d.ShouldSave() {
		t.Fatal("a second call within the interval must be debounced")
	}
	now = now.Add(2 * time.Minute)
	if !d.ShouldSave() {
		t.Fatal("a call after the interval elapsed must save again")
	}
}

// --- ResumeFixWorkspace: resume applies diff.patch at baseCommit, and patch-apply failure -----

func TestResumeFixWorkspace_AppliesDiffPatchAtBaseCommit(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	ctx := context.Background()

	// A prior attempt's workspace: clone, make a change, capture the diff -- simulating what
	// SaveResumeState would have captured before the worker crashed mid-attempt.
	origRoot := t.TempDir()
	ws, err := PrepareFixWorkspace(ctx, PrepareFixWorkspaceInput{
		WorkspaceRoot: origRoot,
		JobID:         "job-resume-2",
		IssueID:       "issue-2",
		CloneURL:      bareRepo,
		DefaultBranch: "main",
		Cred:          testCred(),
		Brief:         TaskBrief{IssueID: "issue-2", FixBrief: "fix it", TestCmd: "true"},
	})
	if err != nil {
		t.Fatalf("PrepareFixWorkspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.RepoDir, "partial.go"), []byte("package main\n\n// partial work\n"), 0o644); err != nil {
		t.Fatalf("seed partial work: %v", err)
	}
	diff, err := ComputeDiffPatch(ctx, ws.RepoDir, ws.BaseCommit, testCred(), nil)
	if err != nil {
		t.Fatalf("ComputeDiffPatch: %v", err)
	}
	if len(diff) == 0 {
		t.Fatal("expected a non-empty diff capturing the partial work")
	}

	resumeState := &ResumeState{
		JobID:      "job-resume-2",
		TaskMD:     "# resumed task",
		ProgressMD: "read handler.go\nwrote failing test\n",
		DiffPatch:  diff,
		BaseCommit: ws.BaseCommit,
	}

	// Resume into a FRESH workspace root, as a real restart would.
	result, err := ResumeFixWorkspace(ctx, ResumeFixWorkspaceInput{
		WorkspaceRoot: t.TempDir(),
		JobID:         "job-resume-2",
		IssueID:       "issue-2",
		CloneURL:      bareRepo,
		Cred:          testCred(),
		State:         resumeState,
	})
	if err != nil {
		t.Fatalf("ResumeFixWorkspace: %v", err)
	}
	if !result.PatchApplied {
		t.Fatal("expected the saved diff.patch to apply cleanly at baseCommit")
	}
	if _, err := os.Stat(filepath.Join(result.Workspace.RepoDir, "partial.go")); err != nil {
		t.Fatalf("resumed workspace must contain the prior attempt's partial work: %v", err)
	}
	if got, _ := os.ReadFile(result.Workspace.TaskPath); string(got) != "# resumed task" {
		t.Fatalf("TASK.md not restored: %q", got)
	}
	if got, _ := os.ReadFile(result.Workspace.ProgressPath); string(got) != resumeState.ProgressMD {
		t.Fatalf("PROGRESS.md not restored: %q", got)
	}
}

func TestResumeFixWorkspace_EmptyDiffAppliesTrivially(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	ctx := context.Background()
	origWS, err := PrepareFixWorkspace(ctx, PrepareFixWorkspaceInput{
		WorkspaceRoot: t.TempDir(),
		JobID:         "job-resume-3",
		IssueID:       "issue-3",
		CloneURL:      bareRepo,
		DefaultBranch: "main",
		Cred:          testCred(),
		Brief:         TaskBrief{IssueID: "issue-3", FixBrief: "x", TestCmd: "true"},
	})
	if err != nil {
		t.Fatalf("PrepareFixWorkspace: %v", err)
	}
	result, err := ResumeFixWorkspace(ctx, ResumeFixWorkspaceInput{
		WorkspaceRoot: t.TempDir(),
		JobID:         "job-resume-3",
		IssueID:       "issue-3",
		CloneURL:      bareRepo,
		Cred:          testCred(),
		State:         &ResumeState{JobID: "job-resume-3", BaseCommit: origWS.BaseCommit},
	})
	if err != nil {
		t.Fatalf("ResumeFixWorkspace: %v", err)
	}
	if !result.PatchApplied {
		t.Fatal("an empty diff must be treated as trivially applied")
	}
}

// TestResumeFixWorkspace_PatchApplyFailure_CleanRestartNotError proves plan §4.4 step 3b's
// "Patch-apply failure => clean restart of that attempt": a corrupted/unrelated diff.patch must
// come back as PatchApplied:false with a NIL error (an expected outcome, not a crash), and the
// resulting workspace must still be usable for a caller to CleanupFixWorkspace + start fresh.
func TestResumeFixWorkspace_PatchApplyFailure_CleanRestartNotError(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	ctx := context.Background()
	origWS, err := PrepareFixWorkspace(ctx, PrepareFixWorkspaceInput{
		WorkspaceRoot: t.TempDir(),
		JobID:         "job-resume-4",
		IssueID:       "issue-4",
		CloneURL:      bareRepo,
		DefaultBranch: "main",
		Cred:          testCred(),
		Brief:         TaskBrief{IssueID: "issue-4", FixBrief: "x", TestCmd: "true"},
	})
	if err != nil {
		t.Fatalf("PrepareFixWorkspace: %v", err)
	}

	garbage := []byte("this is not a valid unified diff at all\n---garbage---\n")
	result, err := ResumeFixWorkspace(ctx, ResumeFixWorkspaceInput{
		WorkspaceRoot: t.TempDir(),
		JobID:         "job-resume-4",
		IssueID:       "issue-4",
		CloneURL:      bareRepo,
		Cred:          testCred(),
		State:         &ResumeState{JobID: "job-resume-4", BaseCommit: origWS.BaseCommit, DiffPatch: garbage},
	})
	if err != nil {
		t.Fatalf("ResumeFixWorkspace must not return an error for a patch-apply failure, got: %v", err)
	}
	if result.PatchApplied {
		t.Fatal("a garbage diff.patch must not report PatchApplied:true")
	}
	if result.Workspace == nil {
		t.Fatal("even on patch-apply failure, a workspace must be returned so the caller can clean it up")
	}
	// Clean restart: caller discards this workspace and prepares a fresh one for the SAME
	// attempt (no new FixCaps.RecordAttempt call -- proved by fix_caps_test.go's attempt-counting
	// tests, this test only proves the resume/cleanup mechanics).
	if err := CleanupFixWorkspace(result.Workspace); err != nil {
		t.Fatalf("CleanupFixWorkspace: %v", err)
	}
}

// TestContinuationBrief_WrapsPriorProgressAsUntrusted proves ContinuationBrief actually threads
// prior progress through TaskBrief.ContinuationPrompt (guard.WrapUntrusted) rather than
// interpolating it raw.
func TestContinuationBrief_WrapsPriorProgressAsUntrusted(t *testing.T) {
	b := TaskBrief{IssueID: "issue-1", FixBrief: "brief", TestCmd: "true"}
	prior := &ResumeState{ProgressMD: "wrote failing test\napplied fix\n"}
	got := ContinuationBrief(b, prior)
	for _, want := range []string{"RESUMED attempt", "wrote failing test", "<<<untrusted:prior_progress:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("continuation brief missing %q: %q", want, got)
		}
	}
}
