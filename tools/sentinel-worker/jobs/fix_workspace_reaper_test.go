package jobs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

func mkWorkspaceDir(t *testing.T, root, jobID string) string {
	t.Helper()
	dir := filepath.Join(root, jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return dir
}

// TestSweepOrphanWorkspaces_DeletesUnknownAndTerminal_KeepsInFlight is finding 6's red-first proof
// on the real path: three real directories under a real workspace root, a real state.Journal
// backed by a real file, exercising the SAME journalFixRunning/JournalFixPROpen helpers RunFix
// itself calls.
func TestSweepOrphanWorkspaces_DeletesUnknownAndTerminal_KeepsInFlight(t *testing.T) {
	workspaceRoot := t.TempDir()
	journalDir := t.TempDir()
	j := state.OpenJournal(filepath.Join(journalDir, "jobs.journal"))

	// job-unknown: a directory with no journal record at all (e.g. crash before the first write).
	mkWorkspaceDir(t, workspaceRoot, "job-unknown")

	// job-terminal: its FIX-PR was opened and then closed (state.StateDone -- terminal).
	mkWorkspaceDir(t, workspaceRoot, "job-terminal")
	if err := JournalFixPROpen(j, "job-terminal", "issue-t", 1, FixPRPayload{PRID: "1"}); err != nil {
		t.Fatalf("JournalFixPROpen: %v", err)
	}
	if err := j.Append(state.Record{JobID: "job-terminal", IssueID: "issue-t", Kind: FixKind, State: state.StateDone}); err != nil {
		t.Fatalf("Append terminal: %v", err)
	}

	// job-inflight: a live fix_running marker -- must be kept for boot-time resume.
	mkWorkspaceDir(t, workspaceRoot, "job-inflight")
	if err := journalFixRunning(j, FixJobInput{JobID: "job-inflight", IssueID: "issue-i"}, "base", false); err != nil {
		t.Fatalf("journalFixRunning: %v", err)
	}

	removed, err := SweepOrphanWorkspaces(workspaceRoot, j)
	if err != nil {
		t.Fatalf("SweepOrphanWorkspaces: %v", err)
	}

	removedSet := map[string]bool{}
	for _, r := range removed {
		removedSet[r] = true
	}
	if !removedSet["job-unknown"] {
		t.Error("job-unknown (no journal record) should have been removed")
	}
	if !removedSet["job-terminal"] {
		t.Error("job-terminal (terminal journal record) should have been removed")
	}
	if removedSet["job-inflight"] {
		t.Error("job-inflight (non-terminal fix_running record) must NOT have been removed")
	}
	if _, err := os.Stat(filepath.Join(workspaceRoot, "job-inflight")); err != nil {
		t.Errorf("job-inflight directory should still exist on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspaceRoot, "job-unknown")); !os.IsNotExist(err) {
		t.Errorf("job-unknown directory should be gone from disk, stat err = %v", err)
	}
}

// TestSweepOrphanWorkspaces_MissingRootIsNotAnError covers a fresh install where the FIX engine
// has never run yet.
func TestSweepOrphanWorkspaces_MissingRootIsNotAnError(t *testing.T) {
	journalDir := t.TempDir()
	j := state.OpenJournal(filepath.Join(journalDir, "jobs.journal"))
	removed, err := SweepOrphanWorkspaces(filepath.Join(t.TempDir(), "does-not-exist"), j)
	if err != nil {
		t.Fatalf("expected no error for a missing workspace root, got %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("expected nothing removed, got %v", removed)
	}
}

// TestReapRetainedWorkspaces_HonoursRetentionDays is finding 6's periodic-reaper proof: an old
// kept-failed workspace (mtime older than the retention window) is deleted; a fresh one is kept.
func TestReapRetainedWorkspaces_HonoursRetentionDays(t *testing.T) {
	workspaceRoot := t.TempDir()
	oldDir := mkWorkspaceDir(t, workspaceRoot, "job-old")
	freshDir := mkWorkspaceDir(t, workspaceRoot, "job-fresh")

	now := time.Now()
	oldTime := now.Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(oldDir, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes old: %v", err)
	}
	freshTime := now.Add(-1 * time.Hour)
	if err := os.Chtimes(freshDir, freshTime, freshTime); err != nil {
		t.Fatalf("chtimes fresh: %v", err)
	}

	removed, err := ReapRetainedWorkspaces(workspaceRoot, 3, now)
	if err != nil {
		t.Fatalf("ReapRetainedWorkspaces: %v", err)
	}
	if len(removed) != 1 || removed[0] != "job-old" {
		t.Fatalf("expected only job-old removed, got %v", removed)
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Errorf("job-fresh should still exist: %v", err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("job-old should be gone, stat err = %v", err)
	}
}

// TestReapRetainedWorkspaces_DisabledWhenRetentionDaysNonPositive is the mutation-test companion
// for the dead-knob finding itself: retentionDays<=0 must disable the pass entirely (matching this
// codebase's other <=0-means-unlimited conventions), not delete everything.
func TestReapRetainedWorkspaces_DisabledWhenRetentionDaysNonPositive(t *testing.T) {
	workspaceRoot := t.TempDir()
	oldDir := mkWorkspaceDir(t, workspaceRoot, "job-old")
	old := time.Now().Add(-999 * 24 * time.Hour)
	if err := os.Chtimes(oldDir, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	removed, err := ReapRetainedWorkspaces(workspaceRoot, 0, time.Now())
	if err != nil {
		t.Fatalf("ReapRetainedWorkspaces: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("retentionDays<=0 must disable the reaper entirely, got removed=%v", removed)
	}
}
