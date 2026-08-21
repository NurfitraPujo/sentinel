package llm

import (
	"sync"
	"time"
)

// Clock abstracts time.Now so DailyBudget/HourlyCounter/DailyCounter can be driven by an injected
// fake in tests instead of real wall-clock time (plan §8: "injected clocks, no real sleeps").
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a plain func() time.Time to Clock.
type ClockFunc func() time.Time

func (f ClockFunc) Now() time.Time { return f() }

// dayKey and hourKey identify the UTC calendar day / hour a given instant falls in, so a reset is
// "the key changed" rather than a duration-since-start computation (which would drift across
// process restarts). Both plan §2.6 windows reset in UTC regardless of the host's local zone.
func dayKey(t time.Time) string  { return t.UTC().Format("2006-01-02") }
func hourKey(t time.Time) string { return t.UTC().Format("2006-01-02T15") }

// DailyBudget tracks adapter-reported token spend against WORKER_DAILY_TOKEN_BUDGET (plan §2.6),
// resetting at 00:00 UTC. It is safe for concurrent use — RunLoop instances across concurrently
// running jobs all report into the same budget.
type DailyBudget struct {
	clock Clock
	limit int // tokens/day; <=0 means unlimited (Exhausted always false)

	mu    sync.Mutex
	day   string
	spent int
}

// NewDailyBudget builds a DailyBudget for the given per-day token limit. clock defaults to the
// real wall clock when nil; pass a fake in tests for deterministic UTC-midnight-reset assertions.
func NewDailyBudget(limit int, clock Clock) *DailyBudget {
	if clock == nil {
		clock = ClockFunc(time.Now)
	}
	return &DailyBudget{clock: clock, limit: limit}
}

// resetIfNewDay rolls spent back to zero the first time it observes a new UTC calendar day. Must
// be called with mu held.
func (b *DailyBudget) resetIfNewDay() {
	k := dayKey(b.clock.Now())
	if k != b.day {
		b.day = k
		b.spent = 0
	}
}

// Add records adapter-reported usage (input+output tokens) against today's spend.
func (b *DailyBudget) Add(u Usage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resetIfNewDay()
	b.spent += u.InputTokens + u.OutputTokens
}

// Spent returns tokens spent so far today.
func (b *DailyBudget) Spent() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resetIfNewDay()
	return b.spent
}

// SeedSpent adds n directly to today's spend, WITHOUT going through Add's normal per-call
// accounting — used by boot-time reconstruction (main.go, reading jobs.SumAdvisedTokenUsage over
// the journal) so a restart does not reset WORKER_DAILY_TOKEN_BUDGET's spend to zero mid-day
// (mirrors DailyCounter.SeedCount's rationale). Must be called before any Add/Exhausted call
// observes today's spend, same convention as FixCaps.SeedToday.
func (b *DailyBudget) SeedSpent(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resetIfNewDay()
	b.spent += n
}

// Remaining returns the tokens left in today's budget (limit - spent), for the plan §7
// "budget_remaining" gauge. A non-positive limit (unlimited) reports -1 so callers/metrics can
// distinguish "unlimited" from "exhausted" (which would otherwise both render as a huge or zero
// number depending on how the caller chose to represent "no limit").
func (b *DailyBudget) Remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		return -1
	}
	b.resetIfNewDay()
	remaining := b.limit - b.spent
	if remaining < 0 {
		remaining = 0
	}
	return remaining
}

// Exhausted reports whether today's spend has reached or exceeded the configured limit — the
// gate jobs/fix.go and the dispatcher (plan §2.6) check before starting a new Advisor loop. A
// non-positive limit means unlimited (never exhausted).
func (b *DailyBudget) Exhausted() bool {
	if b.limit <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resetIfNewDay()
	return b.spent >= b.limit
}

// DailyCounter is a simple per-day count gate (plan §2.6: WORKER_MAX_FIX_JOBS_PER_DAY,
// WORKER_MAX_PRS_PER_DAY, optionally scoped per-repo by the caller using distinct instances or a
// keyed wrapper — this type itself counts one thing per UTC day) resetting at 00:00 UTC.
type DailyCounter struct {
	clock Clock
	limit int // <=0 means unlimited

	mu    sync.Mutex
	day   string
	count int
}

// NewDailyCounter builds a DailyCounter for the given per-day limit.
func NewDailyCounter(limit int, clock Clock) *DailyCounter {
	if clock == nil {
		clock = ClockFunc(time.Now)
	}
	return &DailyCounter{clock: clock, limit: limit}
}

func (c *DailyCounter) resetIfNewDay() {
	k := dayKey(c.clock.Now())
	if k != c.day {
		c.day = k
		c.count = 0
	}
}

// Allow reports whether one more increment is permitted right now, without consuming it — callers
// that need atomic check-and-increment should use TryIncrement instead.
func (c *DailyCounter) Allow() bool {
	if c.limit <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetIfNewDay()
	return c.count < c.limit
}

// TryIncrement atomically checks the limit and increments if under it, returning whether the
// increment was allowed. This is the race-safe primitive — Allow-then-Increment as two calls
// would double-count under concurrency (plan §8: "race-test the budget ... concurrency").
func (c *DailyCounter) TryIncrement() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetIfNewDay()
	if c.limit > 0 && c.count >= c.limit {
		return false
	}
	c.count++
	return true
}

// Count returns today's count so far.
func (c *DailyCounter) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetIfNewDay()
	return c.count
}

// Remaining returns the slots left today (limit - count), mirroring DailyBudget.Remaining's
// contract: a non-positive limit (unlimited) reports -1 so a metrics gauge can distinguish
// "unlimited" from "exhausted" rather than rendering both as the same number.
func (c *DailyCounter) Remaining() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limit <= 0 {
		return -1
	}
	c.resetIfNewDay()
	remaining := c.limit - c.count
	if remaining < 0 {
		remaining = 0
	}
	return remaining
}

// SeedCount sets today's count directly, WITHOUT going through TryIncrement's limit check —
// used by boot-time reconstruction (jobs.FixCaps.SeedToday, N8f finding 2) to restore a count
// observed in a durable log, which may legitimately already be >= limit (e.g. the process crashed
// exactly at the cap). resetIfNewDay runs first so a seed call after a stale day's leftover count
// does not silently add onto it. n is only ever raised, never lowered, so seeding twice (or
// seeding after a real TryIncrement already happened) can never regress the count downward.
func (c *DailyCounter) SeedCount(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetIfNewDay()
	if n > c.count {
		c.count = n
	}
}

// HourlyCounter is the same shape as DailyCounter but keyed by UTC hour, for
// WORKER_MAX_TRIAGE_PER_HOUR (plan §2.6's one hourly-window cap).
type HourlyCounter struct {
	clock Clock
	limit int // <=0 means unlimited

	mu    sync.Mutex
	hour  string
	count int
}

// NewHourlyCounter builds an HourlyCounter for the given per-hour limit.
func NewHourlyCounter(limit int, clock Clock) *HourlyCounter {
	if clock == nil {
		clock = ClockFunc(time.Now)
	}
	return &HourlyCounter{clock: clock, limit: limit}
}

func (c *HourlyCounter) resetIfNewHour() {
	k := hourKey(c.clock.Now())
	if k != c.hour {
		c.hour = k
		c.count = 0
	}
}

// Allow reports whether one more increment is permitted right now, without consuming it.
func (c *HourlyCounter) Allow() bool {
	if c.limit <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetIfNewHour()
	return c.count < c.limit
}

// TryIncrement atomically checks the limit and increments if under it (race-safe, see
// DailyCounter.TryIncrement).
func (c *HourlyCounter) TryIncrement() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetIfNewHour()
	if c.limit > 0 && c.count >= c.limit {
		return false
	}
	c.count++
	return true
}

// Count returns this hour's count so far.
func (c *HourlyCounter) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetIfNewHour()
	return c.count
}

// Remaining returns the slots left this UTC hour (limit - count), mirroring
// DailyCounter.Remaining/DailyBudget.Remaining's contract (-1 for an unlimited/non-positive limit).
func (c *HourlyCounter) Remaining() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limit <= 0 {
		return -1
	}
	c.resetIfNewHour()
	remaining := c.limit - c.count
	if remaining < 0 {
		remaining = 0
	}
	return remaining
}

// SeedCount sets the current UTC hour's count directly, WITHOUT going through TryIncrement's
// limit check — same rationale and same "only ever raised, never lowered" contract as
// DailyCounter.SeedCount (circuit-config-sec finding 3): boot-time reconstruction from the
// journal's triage-start records for the current hour, so a crash-loop restart cannot reset the
// WORKER_MAX_TRIAGE_PER_HOUR cap and let a caller triage past it across a restart within the same
// hour. resetIfNewHour runs first so seeding after a stale hour's leftover count does not silently
// add onto it.
func (c *HourlyCounter) SeedCount(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetIfNewHour()
	if n > c.count {
		c.count = n
	}
}
