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
