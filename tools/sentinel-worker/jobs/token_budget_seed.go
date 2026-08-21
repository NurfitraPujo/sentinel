// token_budget_seed.go reconstructs today's (UTC) LLM token spend from the job journal, so a
// restart does not reset WORKER_DAILY_TOKEN_BUDGET's running total to zero (plan §2.6 finding 1) —
// the same "per day, not per process life" problem jobs/fix_caps.go's SeedToday already fixed for
// the FIX engine's own volume caps, applied here to llm.DailyBudget.
package jobs

import (
	"encoding/json"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// SumAdvisedTokenUsage sums Decision.Usage (InputTokens+OutputTokens) across every "advised"
// journal record — one per TRIAGE/FOLLOW-UP job that actually invoked the Advisor, per Runner.Run
// — whose At falls on now's UTC calendar day. main.go calls this once at boot (state.Journal.Load's
// full record sequence, mirroring FixCaps.SeedToday's own calling convention) and feeds the result
// into llm.DailyBudget.SeedSpent BEFORE the budget is ever consulted or Add'ed into.
//
// Only state.StateAdvised records carry a Decision payload (state.StateActing/StateActed/
// StateDone that follow the SAME jobId are later transitions of the identical decision, not a
// second Advisor call — summing those too would double-count). A corrupt/unparseable payload is
// skipped rather than treated as fatal, matching state.Journal's own "a bad line degrades
// observability, never crashes recovery" posture (same convention as FixCaps.SeedToday).
func SumAdvisedTokenUsage(records []state.Record, now time.Time) int {
	year, month, day := now.UTC().Date()
	isToday := func(t time.Time) bool {
		y, m, d := t.UTC().Date()
		return y == year && m == month && d == day
	}

	total := 0
	for _, r := range records {
		if r.State != state.StateAdvised || !isToday(r.At) {
			continue
		}
		if r.Kind != "triage" && r.Kind != "followup" {
			continue
		}
		if len(r.Payload) == 0 {
			continue
		}
		var d Decision
		if err := json.Unmarshal(r.Payload, &d); err != nil {
			continue
		}
		total += d.Usage.InputTokens + d.Usage.OutputTokens
	}
	return total
}
