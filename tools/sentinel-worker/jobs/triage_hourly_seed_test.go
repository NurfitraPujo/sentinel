package jobs

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

func skipPayload(reason string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"reason": reason})
	return b
}

// TestSumTriageStarts_CountsClaimedTriageMinusUnchargedSkips is the real-path proof for main.go's
// boot-time llm.HourlyCounter.SeedCount reconstruction (circuit-config-sec finding 3): a claimed
// TRIAGE jobId in the current UTC hour counts as one consumed slot UNLESS its only journal record
// in this hour that follows is a skip whose reason proves TryIncrement was never actually charged
// (budget-exhausted skips before the gate is even reached; rate-limited skips because TryIncrement
// itself denied it).
func TestSumTriageStarts_CountsClaimedTriageMinusUnchargedSkips(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	prevHour := now.Add(-2 * time.Hour)

	records := []state.Record{
		// Counted: claimed this hour, went on to be advised -- a real TryIncrement consumed a slot.
		{JobID: "j1", IssueID: "i1", Kind: "triage", State: state.StateClaimed, At: now},
		{JobID: "j1", IssueID: "i1", Kind: "triage", State: state.StateAdvised, At: now},

		// Counted: claimed this hour, then failed for an unrelated transient reason -- the slot was
		// still consumed before the failure.
		{JobID: "j2", IssueID: "i2", Kind: "triage", State: state.StateClaimed, At: now},
		{JobID: "j2", IssueID: "i2", Kind: "triage", State: state.StateFailed, At: now},

		// NOT counted: claimed this hour, but skipped for daily-budget-exhausted -- TryIncrement was
		// never even called (the budget gate runs first).
		{JobID: "j3", IssueID: "i3", Kind: "triage", State: state.StateClaimed, At: now},
		{JobID: "j3", IssueID: "i3", Kind: "triage", State: state.StateSkipped, At: now, Payload: skipPayload("daily-budget-exhausted")},

		// NOT counted: claimed this hour, but skipped for triage-rate-limited -- TryIncrement denied
		// it, so nothing was actually charged.
		{JobID: "j4", IssueID: "i4", Kind: "triage", State: state.StateClaimed, At: now},
		{JobID: "j4", IssueID: "i4", Kind: "triage", State: state.StateSkipped, At: now, Payload: skipPayload("triage-rate-limited")},

		// NOT counted: claimed, but in a PRIOR hour -- "only current hour" filter.
		{JobID: "j5", IssueID: "i5", Kind: "triage", State: state.StateClaimed, At: prevHour},

		// NOT counted: claimed this hour, but Kind is followup, not triage.
		{JobID: "j6", IssueID: "i6", Kind: "followup", State: state.StateClaimed, At: now},

		// Counted: claimed this hour, skipped for an unrelated reason (e.g. foreign claim on a
		// later phase) -- that skip does NOT mean TryIncrement was never charged.
		{JobID: "j7", IssueID: "i7", Kind: "triage", State: state.StateClaimed, At: now},
		{JobID: "j7", IssueID: "i7", Kind: "triage", State: state.StateSkipped, At: now, Payload: skipPayload("foreign-claim")},
	}

	want := 3 // j1, j2, j7
	got := SumTriageStarts(records, now)
	if got != want {
		t.Fatalf("SumTriageStarts = %d, want %d", got, want)
	}
}

// TestSumTriageStarts_EmptyIsZero proves a fresh/empty journal seeds to zero, not some panic or
// nonsensical negative/garbage value.
func TestSumTriageStarts_EmptyIsZero(t *testing.T) {
	if got := SumTriageStarts(nil, time.Now()); got != 0 {
		t.Fatalf("SumTriageStarts(nil) = %d, want 0", got)
	}
}
