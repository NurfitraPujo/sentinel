package jobs

import (
	"fmt"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/llm"
)

func fixedClock(t time.Time) llm.Clock { return llm.ClockFunc(func() time.Time { return t }) }

func TestFixCaps_PRsPerDay_TotalCapEnforced_11thSkippedWithMetric(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	caps := NewFixCaps(100, 10, 2, fixedClock(now))
	var exhausted []string
	caps.OnExhausted = func(reason string) { exhausted = append(exhausted, reason) }

	for i := 0; i < 10; i++ {
		repo := fmt.Sprintf("github/org/repo-%d", i) // 10 different repos so only the TOTAL cap binds
		if !caps.AllowPR(repo) {
			t.Fatalf("PR %d should be allowed (total cap not yet reached)", i)
		}
	}
	if caps.AllowPR("github/org/repo-11th") {
		t.Fatal("11th PR of the day must be skipped: total cap is 10")
	}
	if len(exhausted) != 1 || exhausted[0] != "prs-per-day" {
		t.Fatalf("expected one prs-per-day metric report, got %v", exhausted)
	}
}

func TestFixCaps_PRsPerDay_PerRepoCapEnforced(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	caps := NewFixCaps(100, 2, 2, fixedClock(now))
	var exhausted []string
	caps.OnExhausted = func(reason string) { exhausted = append(exhausted, reason) }

	repo := "github/org/repo"
	if !caps.AllowPR(repo) || !caps.AllowPR(repo) {
		t.Fatal("first two PRs for one repo should be allowed (per-repo cap is 2)")
	}
	if caps.AllowPR(repo) {
		t.Fatal("third PR for the SAME repo must be skipped: per-repo cap is 2, even though the total cap (100) has headroom")
	}
	if len(exhausted) != 1 || exhausted[0] != "prs-per-day-repo:"+repo {
		t.Fatalf("expected one prs-per-day-repo metric report, got %v", exhausted)
	}
}

func TestFixCaps_JobsPerDay_Enforced(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	caps := NewFixCaps(2, 100, 2, fixedClock(now))
	if !caps.AllowJobStart() || !caps.AllowJobStart() {
		t.Fatal("first two job starts should be allowed")
	}
	if caps.AllowJobStart() {
		t.Fatal("third job start must be skipped: WORKER_MAX_FIX_JOBS_PER_DAY is 2")
	}
}

func TestFixCaps_AttemptsPerJobID_FreshStartCounts_ResumeDoesNot(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	caps := NewFixCaps(100, 100, 2, fixedClock(now))
	jobID := "job-abc"

	if !caps.AllowAttempt(jobID) {
		t.Fatal("first attempt should be allowed")
	}
	caps.RecordAttempt(jobID) // a fresh start
	if got := caps.AttemptCount(jobID); got != 1 {
		t.Fatalf("AttemptCount after one fresh start = %d, want 1", got)
	}

	// Simulate N resumes of the SAME attempt (a worker crash-restart re-invoking the executor
	// without discarding the attempt): resume must NOT call RecordAttempt.
	for i := 0; i < 5; i++ {
		if !caps.AllowAttempt(jobID) {
			t.Fatalf("resume %d should still be allowed -- resume never counts a new attempt", i)
		}
		// deliberately no caps.RecordAttempt(jobID) here
	}
	if got := caps.AttemptCount(jobID); got != 1 {
		t.Fatalf("AttemptCount after 5 resumes = %d, want still 1 (resumes must not count)", got)
	}

	// A second FRESH start (e.g. patch-apply failure led to a clean restart the caller treats as
	// consuming the 2nd attempt) DOES count.
	caps.RecordAttempt(jobID)
	if got := caps.AttemptCount(jobID); got != 2 {
		t.Fatalf("AttemptCount after second fresh start = %d, want 2", got)
	}
	if caps.AllowAttempt(jobID) {
		t.Fatal("a third attempt must be refused: WORKER_MAX_FIX_ATTEMPTS is 2")
	}
}

func TestFixCaps_AttemptsPerJobID_ScopedPerJob(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	caps := NewFixCaps(100, 100, 1, fixedClock(now))
	caps.RecordAttempt("job-a")
	if !caps.AllowAttempt("job-b") {
		t.Fatal("a different jobID must have its own independent attempt count")
	}
}

func TestFixCaps_DefaultMaxAttemptsAppliedWhenNonPositive(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	caps := NewFixCaps(100, 100, 0, fixedClock(now))
	for i := 0; i < DefaultMaxFixAttempts; i++ {
		if !caps.AllowAttempt("job") {
			t.Fatalf("attempt %d should be allowed under the default cap", i)
		}
		caps.RecordAttempt("job")
	}
	if caps.AllowAttempt("job") {
		t.Fatal("attempt beyond DefaultMaxFixAttempts must be refused when maxAttempts<=0 was passed")
	}
}
