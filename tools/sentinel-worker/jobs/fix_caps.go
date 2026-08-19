// jobs/fix_caps.go implements the FIX engine's plan §2.6 volume caps (N8f "fix-pr-resume-caps"):
// WORKER_MAX_FIX_JOBS_PER_DAY, WORKER_MAX_PRS_PER_DAY (per-repo AND total), and
// WORKER_MAX_FIX_ATTEMPTS counted per jobID (a crash-resume of the SAME job does NOT count again;
// a fresh FIX start does — CLAUDE.md). All windows reset at 00:00 UTC, reusing llm/budget.go's
// DailyCounter rather than reimplementing day-rollover bookkeeping a third time.
//
// Exhausted caps mean the caller queues/skips the job with a metric, never a crash (CLAUDE.md).
// FixCaps itself never panics or errors on exhaustion — every check returns a plain bool (or, for
// AllowPR, false) and, when OnExhausted is set, reports a stable reason string so a caller can
// wire it straight to a metrics sink without string-matching an error.
package jobs

import (
	"sync"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/llm"
)

// Plan §2.6/§5 defaults for the FIX engine's volume caps and per-attempt timeout.
const (
	DefaultMaxFixJobsPerDay = 10
	DefaultMaxPRsPerDay     = 10
	DefaultMaxFixAttempts   = 2
	DefaultFixTimeout       = 30 * time.Minute
)

// FixCaps enforces the plan §2.6 FIX volume caps for one worker process. It is safe for
// concurrent use — every method locks internally, and AllowPR's total+per-repo pair is checked
// and committed atomically under one critical section so two concurrently racing PRs for
// different repos can never both observe headroom in the shared total counter and then both
// commit, pushing it over the configured limit (llm.DailyCounter.Allow-then-TryIncrement as two
// separate calls would have exactly that race — see AllowPR's doc).
type FixCaps struct {
	jobsPerDay     *llm.DailyCounter // WORKER_MAX_FIX_JOBS_PER_DAY
	prsTotal       *llm.DailyCounter // WORKER_MAX_PRS_PER_DAY, total across all repos
	prsPerDayLimit int               // same numeric limit, applied per-repo too (plan §2.6: "per-repo AND total")
	maxAttempts    int               // WORKER_MAX_FIX_ATTEMPTS
	clock          llm.Clock

	mu         sync.Mutex
	prsPerRepo map[string]*llm.DailyCounter // lazily created, one per repoKey
	attempts   map[string]int               // jobID -> attempts started (RecordAttempt calls)

	// OnExhausted, when non-nil, is called once per rejected check with a stable reason:
	// "fix-jobs-per-day" | "prs-per-day" | "prs-per-day-repo:<repoKey>" | "fix-attempts". Never
	// called on a successful check. nil is a valid no-op (metrics wiring is optional).
	OnExhausted func(reason string)
}

// NewFixCaps builds a FixCaps. clock defaults to the real wall clock when nil (pass a fake in
// tests, plan §8: "injected clocks, no real sleeps"). maxAttempts <= 0 falls back to
// DefaultMaxFixAttempts — a zero-value config must not silently mean "unlimited attempts", which
// llm.DailyCounter's own <=0-means-unlimited convention would otherwise imply if reused verbatim
// here; attempts-per-jobID has no such "unlimited" reading in the plan.
func NewFixCaps(maxJobsPerDay, maxPRsPerDay, maxAttempts int, clock llm.Clock) *FixCaps {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxFixAttempts
	}
	return &FixCaps{
		jobsPerDay:     llm.NewDailyCounter(maxJobsPerDay, clock),
		prsTotal:       llm.NewDailyCounter(maxPRsPerDay, clock),
		prsPerDayLimit: maxPRsPerDay,
		maxAttempts:    maxAttempts,
		clock:          clock,
		prsPerRepo:     map[string]*llm.DailyCounter{},
		attempts:       map[string]int{},
	}
}

func (c *FixCaps) report(reason string) {
	if c.OnExhausted != nil {
		c.OnExhausted(reason)
	}
}

// AllowJobStart atomically checks and consumes one WORKER_MAX_FIX_JOBS_PER_DAY slot. Callers gate
// starting a FIX job (fresh workspace prep) on this — NOT on resuming an in-flight one (a resume
// isn't "one more job", it's the same job continuing).
func (c *FixCaps) AllowJobStart() bool {
	if c.jobsPerDay.TryIncrement() {
		return true
	}
	c.report("fix-jobs-per-day")
	return false
}

// repoCounter returns repoKey's per-repo PR counter, creating it (at the same prsPerDayLimit) on
// first use. Caller must hold c.mu.
func (c *FixCaps) repoCounter(repoKey string) *llm.DailyCounter {
	if rc, ok := c.prsPerRepo[repoKey]; ok {
		return rc
	}
	rc := llm.NewDailyCounter(c.prsPerDayLimit, c.clock)
	c.prsPerRepo[repoKey] = rc
	return rc
}

// AllowPR atomically checks BOTH repoKey's per-repo and the total WORKER_MAX_PRS_PER_DAY counters
// and consumes one slot from each ONLY if both currently have headroom (plan §2.6: "10, per-repo
// AND total" — the same configured number bounds both dimensions). repoKey should be a stable
// per-repo identifier (e.g. "provider/owner/repo") — a caller-chosen string, not interpreted here.
// Checking and committing both counters under one held c.mu (rather than two independent
// llm.DailyCounter.TryIncrement calls) is what prevents two different repos' concurrent AllowPR
// calls from both reading the total counter as under-limit and both committing, pushing total
// spend over the configured cap.
//
// The per-repo counter is checked FIRST, deliberately: since the total counter's spend is always
// >= any single repo's own spend (every PR increments both), a repo that has independently reached
// the shared numeric limit will always have driven the total counter to at least that same limit
// too — so if AllowPR checked the total counter first, the more specific and actionable
// "prs-per-day-repo:<repoKey>" report would never be reachable (the total check would always fire
// first once any one repo saturates it). Checking the repo counter first surfaces the more precise
// reason whenever the exhaustion traces back to one repo, and still correctly reports the plain
// "prs-per-day" reason when the repo itself has headroom but the shared total does not (load
// spread across several different repos).
func (c *FixCaps) AllowPR(repoKey string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	rc := c.repoCounter(repoKey)
	if !rc.Allow() {
		c.report("prs-per-day-repo:" + repoKey)
		return false
	}
	if !c.prsTotal.Allow() {
		c.report("prs-per-day")
		return false
	}
	rc.TryIncrement()
	c.prsTotal.TryIncrement()
	return true
}

// AllowAttempt reports whether jobID may start (a fresh attempt) or continue (a resume) without
// exceeding WORKER_MAX_FIX_ATTEMPTS. It does not itself consume anything — see RecordAttempt.
func (c *FixCaps) AllowAttempt(jobID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.attempts[jobID] >= c.maxAttempts {
		c.report("fix-attempts")
		return false
	}
	return true
}

// RecordAttempt increments jobID's attempt count. Callers MUST call this exactly once per FRESH
// FIX attempt start (a brand-new workspace/clone) and MUST NOT call it for a resume of the same
// attempt (CLAUDE.md: "a crash-resume of the same job does NOT count again; a fresh FIX start
// does") — a patch-apply-failure clean restart of a resume is likewise not a fresh start in this
// sense; whether it re-consumes an attempt is the caller's policy choice, this type only tracks
// what it's told.
func (c *FixCaps) RecordAttempt(jobID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts[jobID]++
}

// AttemptCount returns jobID's current attempt count (test/observability convenience — not itself
// load-bearing for any gate).
func (c *FixCaps) AttemptCount(jobID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts[jobID]
}
