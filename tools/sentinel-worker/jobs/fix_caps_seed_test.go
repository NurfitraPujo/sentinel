package jobs

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// TestFixCaps_SeedToday_ReconstructsFromRealJournal is finding 2's red-first proof on the REAL
// path: it appends journal records via the SAME production helpers RunFix itself uses
// (journalFixRunning, JournalFixPROpen), through a real state.Journal backed by a real file (not a
// hand-built map), then boots a FRESH FixCaps (as main.go would after a restart) and asserts its
// counters already reflect what the journal recorded -- exactly the scenario finding 2 says was
// broken ("caps reset to 0 on restart").
func TestFixCaps_SeedToday_ReconstructsFromRealJournal(t *testing.T) {
	dir := t.TempDir()
	j := state.OpenJournal(filepath.Join(dir, "jobs.journal"))
	// journalFixRunning/JournalFixPROpen stamp records via journal.Append at the real wall clock, so
	// SeedToday's "today" must be anchored to that same wall day -- a frozen past date would make
	// isToday filter out every just-written record and seed all counts as zero (date-fragile: the
	// test would pass only on the hard-coded calendar day).
	now := time.Now().UTC()

	// Job 1: one FIX attempt started today, then its PR opened today (repo r1, project p1).
	in1 := FixJobInput{JobID: "job-1", IssueID: "issue-1", ProjectID: "p1", TriggerSeq: 1}
	if err := journalFixRunning(j, in1, "base1", false); err != nil {
		t.Fatalf("journalFixRunning(job-1): %v", err)
	}
	if err := JournalFixPROpen(j, "job-1", "issue-1", 1, FixPRPayload{
		Provider: gitprovider.RepoRef{Provider: "github", Owner: "org", Repo: "r1"},
		PRID:     "1", PRURL: "https://example/pr/1",
	}); err != nil {
		t.Fatalf("JournalFixPROpen(job-1): %v", err)
	}

	// Job 2: a second FIX job (different jobID), started today, no PR yet (attempt only).
	in2 := FixJobInput{JobID: "job-2", IssueID: "issue-2", ProjectID: "p1", TriggerSeq: 2}
	if err := journalFixRunning(j, in2, "base2", false); err != nil {
		t.Fatalf("journalFixRunning(job-2): %v", err)
	}

	// Boot a fresh FixCaps -- as main.go would after a restart -- and seed it from the journal.
	caps := NewFixCaps(100, 100, 2, fixedClock(now))
	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	caps.SeedToday(records, now)

	// Fix-jobs-per-day: two distinct jobIDs started today -> 2 slots already consumed. A 100-cap
	// process should still be able to start more, but if we ratchet the cap down to exactly 2 for a
	// SECOND FixCaps instance, a 3rd job-start must now be refused -- proving the count is really
	// seeded, not just present as an unused field.
	strictCaps := NewFixCaps(2, 100, 2, fixedClock(now))
	strictCaps.SeedToday(records, now)
	if strictCaps.AllowJobStart() {
		t.Fatal("jobs-per-day cap of 2 should already be exhausted by the 2 jobs seeded from the journal")
	}

	// Attempts: job-1 and job-2 each have exactly one fix_running record today -> AttemptCount==1.
	if got := caps.AttemptCount("job-1"); got != 1 {
		t.Fatalf("AttemptCount(job-1) after seeding = %d, want 1", got)
	}
	if got := caps.AttemptCount("job-2"); got != 1 {
		t.Fatalf("AttemptCount(job-2) after seeding = %d, want 1", got)
	}

	// PRs: job-1's PR-open should have consumed the repo counter, the project counter, and the
	// total counter. Use a generous global cap (so only the PER-PROJECT override below is the
	// binding constraint) and a strict project limit of 1 for p1: a second PR for p1 must now be
	// refused, while a DIFFERENT project (p2, also limit 1) is entirely unaffected.
	strictPR := NewFixCaps(100, 100, 2, fixedClock(now))
	strictPR.SeedToday(records, now)
	one := 1
	if strictPR.AllowPR("github/org/r1-again", "p1", &one) {
		t.Fatal("project p1's per-project cap of 1 should already be exhausted by the 1 PR seeded from the journal")
	}
	if !strictPR.AllowPR("github/org/r2", "p2", &one) {
		t.Fatal("a different project must be unaffected by another project's seeded PR")
	}

	// The repo counter itself: a strict per-repo/total cap of 1 must refuse a second PR for the
	// SAME repo r1, even under a different project.
	strictRepo := NewFixCaps(100, 1, 2, fixedClock(now))
	strictRepo.SeedToday(records, now)
	if strictRepo.AllowPR("github/org/r1", "p-other", &one) {
		t.Fatal("repo r1's seeded PR should already have consumed its per-repo/total slot (cap 1)")
	}
}

// TestFixCaps_SeedToday_IgnoresYesterdaysRecords is the mutation-test companion for the UTC-day
// boundary: delete the isToday guard and this test goes red (a stale record from yesterday would
// wrongly count against today's caps forever).
func TestFixCaps_SeedToday_IgnoresYesterdaysRecords(t *testing.T) {
	dir := t.TempDir()
	j := state.OpenJournal(filepath.Join(dir, "jobs.journal"))
	yesterday := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	today := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	payload, _ := json.Marshal(FixRunningPayload{Input: FixJobInput{JobID: "job-old", IssueID: "issue-old", ProjectID: "p1"}, BaseCommit: "x"})
	if err := j.Append(state.Record{
		JobID: "job-old", IssueID: "issue-old", Kind: FixKind, State: state.StateFixRunning,
		At: yesterday, Payload: payload,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	strictCaps := NewFixCaps(1, 100, 2, fixedClock(today))
	strictCaps.SeedToday(records, today)
	if !strictCaps.AllowJobStart() {
		t.Fatal("a fix_running record from a PRIOR UTC day must not consume today's jobs-per-day slot")
	}
	if got := strictCaps.AttemptCount("job-old"); got != 0 {
		t.Fatalf("AttemptCount(job-old) after seeding with a yesterday-only record = %d, want 0", got)
	}
}
