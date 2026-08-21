package jobs

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/llm"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// TestSumAdvisedTokenUsage_FiltersToTodaysAdvisedTriageAndFollowup is the real-path proof for
// main.go's boot-time llm.DailyBudget.SeedSpent reconstruction (called from main.go around line
// 1718): only state.StateAdvised records, on "now"'s UTC calendar day, of Kind "triage" or
// "followup", with a decodable Decision payload, may contribute to the sum. Every other record in
// the journal is noise that must NOT be counted, or a restart would seed the budget with the wrong
// running total.
func TestSumAdvisedTokenUsage_FiltersToTodaysAdvisedTriageAndFollowup(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	yesterday := now.AddDate(0, 0, -1)

	advisedPayload := func(in, out int) json.RawMessage {
		b, err := json.Marshal(Decision{Kind: "triage", Usage: llm.Usage{InputTokens: in, OutputTokens: out}})
		if err != nil {
			t.Fatalf("marshal decision: %v", err)
		}
		return b
	}

	records := []state.Record{
		// Counted: today, advised, triage.
		{JobID: "j1", IssueID: "i1", Kind: "triage", State: state.StateAdvised, At: now, Payload: advisedPayload(100, 50)},
		// Counted: today, advised, followup.
		{JobID: "j2", IssueID: "i2", Kind: "followup", State: state.StateAdvised, At: now, Payload: advisedPayload(30, 20)},
		// NOT counted: yesterday, otherwise identical -- "only today" filter.
		{JobID: "j3", IssueID: "i3", Kind: "triage", State: state.StateAdvised, At: yesterday, Payload: advisedPayload(1000, 1000)},
		// NOT counted: today but not StateAdvised (a later transition of the SAME decision) --
		// "only advised" filter, guards against double-counting.
		{JobID: "j1", IssueID: "i1", Kind: "triage", State: state.StateActed, At: now, Payload: advisedPayload(100, 50)},
		{JobID: "j1", IssueID: "i1", Kind: "triage", State: state.StateDone, At: now},
		// NOT counted: today, advised, but Kind is neither triage nor followup -- "only
		// triage/followup" filter (FIX engine or any future kind must not leak in here).
		{JobID: "j4", IssueID: "i4", Kind: "fix", State: state.StateAdvised, At: now, Payload: advisedPayload(500, 500)},
		// NOT counted: today, advised, triage, but the payload is corrupt -- "corrupt skipped"
		// filter, matching state.Journal's degrade-don't-crash posture.
		{JobID: "j5", IssueID: "i5", Kind: "triage", State: state.StateAdvised, At: now, Payload: json.RawMessage(`{not valid json`)},
		// NOT counted: today, advised, triage, but empty payload.
		{JobID: "j6", IssueID: "i6", Kind: "triage", State: state.StateAdvised, At: now},
	}

	want := 100 + 50 + 30 + 20 // only j1's StateAdvised record + j2
	got := SumAdvisedTokenUsage(records, now)
	if got != want {
		t.Fatalf("SumAdvisedTokenUsage = %d, want %d", got, want)
	}
}
