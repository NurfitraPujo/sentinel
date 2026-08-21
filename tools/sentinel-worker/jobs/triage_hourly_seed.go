// triage_hourly_seed.go reconstructs the current UTC hour's TRIAGE job count from the job journal,
// so a restart does not reset WORKER_MAX_TRIAGE_PER_HOUR's running total to zero (circuit-config-
// sec finding 3) -- the same "per window, not per process life" problem token_budget_seed.go and
// jobs/fix_caps.go's SeedToday already fix for the daily token budget and the FIX engine's own
// volume caps, applied here to llm.HourlyCounter.
package jobs

import (
	"encoding/json"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// SumTriageStarts counts, among journal records whose At falls in now's current UTC hour, every
// distinct TRIAGE jobId that reached state.StateClaimed -- the point loop.Runner journals
// immediately before consulting Runner.TriageLimiter.TryIncrement (loop/runner.go) -- MINUS any of
// those jobIds whose later-in-the-same-hour terminal record is state.StateSkipped for a reason that
// means TryIncrement was never actually charged for it:
//
//   - "daily-budget-exhausted" (loop.SkipBudgetExhausted): the daily token budget gate runs BEFORE
//     the hourly triage gate, so an exhausted budget skips the job without ever calling
//     TryIncrement.
//   - "triage-rate-limited" (loop.SkipTriageRateLimited): TryIncrement itself denied the increment
//     (already at the cap), so nothing was actually counted for this jobId either.
//
// Every other outcome for a claimed TRIAGE job (advised, questioned, acting, acted, done, a
// transient failure, even a later foreign-claim/deleted skip during a subsequent job phase) implies
// TryIncrement succeeded and consumed one hourly slot, so those jobIds count. main.go calls this
// once at boot (state.Journal.Load's full record sequence, mirroring SumAdvisedTokenUsage/
// FixCaps.SeedToday's own calling convention) and feeds the result into llm.HourlyCounter.SeedCount
// BEFORE the counter is ever consulted or TryIncrement'd.
//
// Corrupt/unparseable skip-reason payloads are treated as "reason unknown" and conservatively left
// counted (the record still reached StateClaimed, which is what actually gates a real TryIncrement
// call) -- matching state.Journal's own "a bad line degrades observability, never crashes recovery"
// posture used throughout this package.
func SumTriageStarts(records []state.Record, now time.Time) int {
	return sumHourlyStarts(records, now, "triage", "daily-budget-exhausted", "triage-rate-limited")
}

// SumFollowupStarts is SumTriageStarts's FOLLOW-UP-kind counterpart (finding 5, core-robustness
// round 3): reconstructs the current UTC hour's FOLLOW-UP job count from the job journal so a
// restart does not reset WORKER_MAX_FOLLOWUP_PER_HOUR's running total to zero, the same "seed
// before any TryIncrement call" fix SumTriageStarts already applies to WORKER_MAX_TRIAGE_PER_HOUR.
// "followup-rate-limited" (loop.SkipFollowupRateLimited) is this cap's own not-charged skip
// reason; "daily-budget-exhausted" still applies since the daily budget gate runs before EITHER
// hourly limiter, TRIAGE or FOLLOW-UP.
func SumFollowupStarts(records []state.Record, now time.Time) int {
	return sumHourlyStarts(records, now, "followup", "daily-budget-exhausted", "followup-rate-limited")
}

// sumHourlyStarts is the shared counting logic behind SumTriageStarts/SumFollowupStarts: among
// journal records of the given kind whose At falls in now's current UTC hour, count every
// distinct jobId that reached state.StateClaimed -- the point loop.Runner journals immediately
// after its Budget/hourly-limiter gates pass and ensure-claimed succeeds -- MINUS any of those
// jobIds whose later-in-the-same-hour terminal record is state.StateSkipped for one of
// notChargedReasons (a skip reason meaning the corresponding TryIncrement call was never actually
// charged, or never made). Corrupt/unparseable skip-reason payloads are treated as "reason
// unknown" and conservatively left counted, matching state.Journal's own "a bad line degrades
// observability, never crashes recovery" posture used throughout this package.
func sumHourlyStarts(records []state.Record, now time.Time, kind string, notChargedReasons ...string) int {
	hour := now.UTC().Format("2006010215")
	inHour := func(t time.Time) bool { return t.UTC().Format("2006010215") == hour }
	notChargedSet := map[string]bool{}
	for _, reason := range notChargedReasons {
		notChargedSet[reason] = true
	}

	claimed := map[string]bool{}
	notCharged := map[string]bool{}

	for _, r := range records {
		if r.Kind != kind || !inHour(r.At) {
			continue
		}
		switch r.State {
		case state.StateClaimed:
			claimed[r.JobID] = true
		case state.StateSkipped:
			if len(r.Payload) == 0 {
				continue
			}
			var fields map[string]string
			if err := json.Unmarshal(r.Payload, &fields); err != nil {
				continue
			}
			if notChargedSet[fields["reason"]] {
				notCharged[r.JobID] = true
			}
		}
	}

	count := 0
	for jobID := range claimed {
		if !notCharged[jobID] {
			count++
		}
	}
	return count
}
