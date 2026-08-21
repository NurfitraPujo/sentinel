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
		if !caps.AllowPR(repo, repo, nil) {
			t.Fatalf("PR %d should be allowed (total cap not yet reached)", i)
		}
	}
	if caps.AllowPR("github/org/repo-11th", "github/org/repo-11th", nil) {
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
	if !caps.AllowPR(repo, "proj-1", nil) || !caps.AllowPR(repo, "proj-1", nil) {
		t.Fatal("first two PRs for one repo should be allowed (per-repo cap is 2)")
	}
	if caps.AllowPR(repo, "proj-1", nil) {
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

// mutableClock is an llm.Clock whose Now() can be advanced mid-test -- fixedClock's closure is
// read-only, and finding 8's UTC-midnight-reset assertion needs to move the clock forward past a
// day boundary without rebuilding the FixCaps under test.
type mutableClock struct{ now time.Time }

func (c *mutableClock) Now() time.Time { return c.now }

// TestFixCaps_IssueAttempts_ResetsAtUTCMidnight is finding 8's RED-FIRST proof: the per-issue
// attempt cap (issueAttempts, WORKER_MAX_FIX_ATTEMPTS enforced per-issue) must reset at 00:00 UTC
// like every other cap in this file -- before this fix, issueAttempts had no day-rollover logic at
// all, so an issue that exhausted its budget on one UTC day stayed permanently blocked on every
// later day too.
func TestFixCaps_IssueAttempts_ResetsAtUTCMidnight(t *testing.T) {
	clock := &mutableClock{now: time.Date(2026, 8, 18, 23, 0, 0, 0, time.UTC)}
	caps := NewFixCaps(100, 100, 1, clock)

	if !caps.AllowIssueAttempt("issue-x") {
		t.Fatal("first attempt for issue-x should be allowed")
	}
	caps.RecordIssueAttempt("issue-x")
	if caps.AllowIssueAttempt("issue-x") {
		t.Fatal("second attempt for issue-x on the same UTC day must be refused: WORKER_MAX_FIX_ATTEMPTS is 1")
	}

	// Cross into the next UTC calendar day.
	clock.now = time.Date(2026, 8, 19, 0, 30, 0, 0, time.UTC)

	if !caps.AllowIssueAttempt("issue-x") {
		t.Fatal("issue-x's per-issue attempt cap must reset at 00:00 UTC, allowing a fresh attempt the next day")
	}
	if got := caps.IssueAttemptCount("issue-x"); got != 0 {
		t.Fatalf("IssueAttemptCount after the UTC day rolled over = %d, want 0", got)
	}
}

// TestFixCaps_AllowPR_PerProjectCapEnforced is finding 4's red-first proof: a project with
// maxPrsPerDay=1 (settings.ProjectSettings.MaxPRsPerDay, C15) blocks its 2nd PR for the day even
// though the global cap (100) and the per-repo cap (100, different repos below) both have
// headroom, while ANOTHER project is entirely unaffected by the first project's cap.
func TestFixCaps_AllowPR_PerProjectCapEnforced(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	caps := NewFixCaps(100, 100, 2, fixedClock(now))
	var exhausted []string
	caps.OnExhausted = func(reason string) { exhausted = append(exhausted, reason) }

	one := 1
	if !caps.AllowPR("github/org/repo-a", "project-a", &one) {
		t.Fatal("project-a's 1st PR should be allowed (its own cap is 1)")
	}
	if caps.AllowPR("github/org/repo-a2", "project-a", &one) {
		t.Fatal("project-a's 2nd PR must be blocked by its own maxPrsPerDay=1, even in a different repo")
	}
	if len(exhausted) != 1 || exhausted[0] != "prs-per-day-project:project-a" {
		t.Fatalf("expected one prs-per-day-project metric report, got %v", exhausted)
	}
	if !caps.AllowPR("github/org/repo-b", "project-b", &one) {
		t.Fatal("project-b must be unaffected by project-a's exhausted per-project cap")
	}
}

// TestFixCaps_AllowPR_NilProjectLimitFallsBackToGlobal is the mutation-test companion: with the
// production line (nil projectLimit -> use c.prsPerDayLimit) deleted/regressed to an unconditional
// zero limit, a project with NO server-configured cap (nil, "no self-enforced cap" per C15) would
// incorrectly be blocked on its very first PR. This asserts it is NOT blocked.
func TestFixCaps_AllowPR_NilProjectLimitFallsBackToGlobal(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	caps := NewFixCaps(100, 5, 2, fixedClock(now))
	if !caps.AllowPR("github/org/repo", "project-no-cap", nil) {
		t.Fatal("a project with no server-configured maxPrsPerDay must fall back to the global cap, not be blocked outright")
	}
}

// TestFixCaps_AllowPR_ExplicitZeroProjectLimitBlocksUnconditionally is finding 7's RED-FIRST proof:
// an explicit server-supplied maxPrsPerDay of 0 (settings.ProjectSettings.MaxPRsPerDay==0, C15) must
// mean "zero fix PRs allowed for this project", NOT "unlimited" -- before this fix, AllowPR's
// `limit > 0 && pc.count >= limit` check folded an explicit 0 into the same bucket as nil ("no cap"),
// so *0 silently fell back to the global cap and let PRs through.
func TestFixCaps_AllowPR_ExplicitZeroProjectLimitBlocksUnconditionally(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	caps := NewFixCaps(100, 100, 2, fixedClock(now))
	var exhausted []string
	caps.OnExhausted = func(reason string) { exhausted = append(exhausted, reason) }

	zero := 0
	if caps.AllowPR("github/org/repo-zero", "project-zero", &zero) {
		t.Fatal("an explicit maxPrsPerDay=0 must block every PR for this project, not fall back to the global cap")
	}
	if len(exhausted) != 1 || exhausted[0] != "prs-per-day-project:project-zero" {
		t.Fatalf("expected one prs-per-day-project metric report, got %v", exhausted)
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

// TestFixCaps_FixJobsRemainingAndPRsRemaining is finding 10's proof for the FixCaps-level accessors
// main.go's gauge wiring calls -- FixJobsRemaining/PRsRemaining must reflect consumption from
// AllowJobStart/AllowPR exactly like DailyCounter.Remaining itself.
func TestFixCaps_FixJobsRemainingAndPRsRemaining(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	caps := NewFixCaps(2, 2, 2, fixedClock(now))

	if got := caps.FixJobsRemaining(); got != 2 {
		t.Fatalf("FixJobsRemaining() before any job start = %d, want 2", got)
	}
	caps.AllowJobStart()
	if got := caps.FixJobsRemaining(); got != 1 {
		t.Fatalf("FixJobsRemaining() after one job start = %d, want 1", got)
	}

	if got := caps.PRsRemaining(); got != 2 {
		t.Fatalf("PRsRemaining() before any PR = %d, want 2", got)
	}
	caps.AllowPR("github/org/repo", "proj-1", nil)
	if got := caps.PRsRemaining(); got != 1 {
		t.Fatalf("PRsRemaining() after one PR = %d, want 1", got)
	}
}
