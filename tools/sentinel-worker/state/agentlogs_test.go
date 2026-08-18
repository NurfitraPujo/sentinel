package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentLogPath(t *testing.T) {
	got := AgentLogPath("/state", "job-123")
	want := filepath.Join("/state", "agent-logs", "job-123.log")
	if got != want {
		t.Fatalf("AgentLogPath = %s, want %s", got, want)
	}
}

func TestReapAgentLogs_MissingDirIsNotError(t *testing.T) {
	deleted, truncated, err := ReapAgentLogs(t.TempDir(), nil, time.Now(), 1024)
	if err != nil {
		t.Fatalf("expected no error for missing agent-logs dir, got: %v", err)
	}
	if deleted != 0 || truncated != 0 {
		t.Fatalf("expected nothing done, got deleted=%d truncated=%d", deleted, truncated)
	}
}

// TestReapAgentLogs_DropsOnlyOldTerminal proves the plan §2.2 rule that agent-logs reaping mirrors
// journal Compact: only logs for jobs the caller names as terminal-and-old are removed.
func TestReapAgentLogs_DropsOnlyOldTerminal(t *testing.T) {
	stateDir := t.TempDir()
	logsDir := filepath.Join(stateDir, "agent-logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, id := range []string{"old-done", "recent-done", "in-flight"} {
		if err := os.WriteFile(filepath.Join(logsDir, id+".log"), []byte("some log output\n"), 0o600); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
	}

	old := time.Now().Add(-10 * 24 * time.Hour)
	recent := time.Now().Add(-1 * time.Hour)
	terminal := map[string]time.Time{
		"old-done":    old,
		"recent-done": recent,
		// "in-flight" intentionally absent — not terminal, must survive regardless of age.
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)

	deleted, _, err := ReapAgentLogs(stateDir, terminal, cutoff, 0)
	if err != nil {
		t.Fatalf("ReapAgentLogs: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected exactly 1 deletion, got %d", deleted)
	}
	if _, err := os.Stat(filepath.Join(logsDir, "old-done.log")); !os.IsNotExist(err) {
		t.Errorf("expected old-done.log to be deleted")
	}
	for _, id := range []string{"recent-done", "in-flight"} {
		if _, err := os.Stat(filepath.Join(logsDir, id+".log")); err != nil {
			t.Errorf("expected %s.log to survive, stat error: %v", id, err)
		}
	}
}

// TestReapAgentLogs_TruncatesOversizedFileKeepingTail proves the size cap keeps the most recent
// bytes of a live (non-terminal) Fix Executor log rather than deleting it outright.
func TestReapAgentLogs_TruncatesOversizedFileKeepingTail(t *testing.T) {
	stateDir := t.TempDir()
	logsDir := filepath.Join(stateDir, "agent-logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := []byte("0123456789")
	path := filepath.Join(logsDir, "big-job.log")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, truncated, err := ReapAgentLogs(stateDir, nil, time.Now(), 4)
	if err != nil {
		t.Fatalf("ReapAgentLogs: %v", err)
	}
	if truncated != 1 {
		t.Fatalf("expected 1 truncation, got %d", truncated)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading truncated log: %v", err)
	}
	if string(got) != "6789" {
		t.Fatalf("expected tail bytes '6789', got %q", got)
	}
}
