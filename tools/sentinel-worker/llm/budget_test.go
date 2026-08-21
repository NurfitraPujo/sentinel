package llm

import (
	"sync"
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return tm
}

// fakeClock is an injectable, mutable Clock for deterministic reset-boundary tests (plan §8:
// "injected clocks, no real sleeps").
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func TestDailyBudget_ExhaustedAndReset(t *testing.T) {
	clock := &fakeClock{now: mustParse(t, "2026-08-18T10:00:00Z")}
	b := NewDailyBudget(100, clock)

	if b.Exhausted() {
		t.Fatal("fresh budget must not be exhausted")
	}
	b.Add(Usage{InputTokens: 60, OutputTokens: 30})
	if b.Spent() != 90 {
		t.Fatalf("Spent() = %d, want 90", b.Spent())
	}
	if b.Exhausted() {
		t.Fatal("90/100 must not be exhausted yet")
	}
	b.Add(Usage{InputTokens: 5, OutputTokens: 5})
	if !b.Exhausted() {
		t.Fatal("100/100 must be exhausted")
	}

	// Red-first proof: without the UTC-midnight reset, spend would carry over and stay exhausted.
	clock.Set(mustParse(t, "2026-08-19T00:00:01Z"))
	if b.Exhausted() {
		t.Fatal("budget must reset at UTC midnight")
	}
	if b.Spent() != 0 {
		t.Fatalf("Spent() after reset = %d, want 0", b.Spent())
	}
}

func TestDailyBudget_UnlimitedNeverExhausted(t *testing.T) {
	clock := &fakeClock{now: mustParse(t, "2026-08-18T10:00:00Z")}
	b := NewDailyBudget(0, clock)
	b.Add(Usage{InputTokens: 1_000_000})
	if b.Exhausted() {
		t.Fatal("limit<=0 must mean unlimited")
	}
}

func TestDailyCounter_TryIncrementAndReset(t *testing.T) {
	clock := &fakeClock{now: mustParse(t, "2026-08-18T10:00:00Z")}
	c := NewDailyCounter(2, clock)

	if !c.TryIncrement() {
		t.Fatal("1st increment should be allowed")
	}
	if !c.TryIncrement() {
		t.Fatal("2nd increment should be allowed")
	}
	if c.TryIncrement() {
		t.Fatal("3rd increment must be rejected at limit 2")
	}
	if c.Count() != 2 {
		t.Fatalf("Count() = %d, want 2", c.Count())
	}

	clock.Set(mustParse(t, "2026-08-19T00:00:00Z"))
	if !c.TryIncrement() {
		t.Fatal("increment must be allowed again after UTC-day reset")
	}
}

func TestDailyCounter_ConcurrentTryIncrementNeverExceedsLimit(t *testing.T) {
	clock := &fakeClock{now: mustParse(t, "2026-08-18T10:00:00Z")}
	c := NewDailyCounter(50, clock)

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.TryIncrement() {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 50 {
		t.Fatalf("allowed = %d, want exactly 50 under concurrent TryIncrement", allowed)
	}
	if c.Count() != 50 {
		t.Fatalf("Count() = %d, want 50", c.Count())
	}
}

// TestDailyCounter_Allow covers DailyCounter.Allow (0% before this test): it must report whether
// one more increment is currently permitted WITHOUT consuming it (repeated calls at the limit stay
// stable, and it must observe the UTC-day reset the same way TryIncrement does).
func TestDailyCounter_Allow(t *testing.T) {
	clock := &fakeClock{now: mustParse(t, "2026-08-18T10:00:00Z")}
	c := NewDailyCounter(1, clock)

	if !c.Allow() {
		t.Fatal("fresh counter under limit should Allow")
	}
	if !c.Allow() {
		t.Fatal("calling Allow again must not have consumed anything")
	}
	if !c.TryIncrement() {
		t.Fatal("increment at limit 1 should succeed")
	}
	if c.Allow() {
		t.Fatal("Allow must report false once the limit is reached")
	}

	clock.Set(mustParse(t, "2026-08-19T00:00:00Z"))
	if !c.Allow() {
		t.Fatal("Allow must report true again after the UTC-day reset")
	}
}

// TestDailyCounter_Allow_UnlimitedWhenLimitNonPositive covers the limit<=0 unlimited fast path
// (Allow returns true without ever touching the mutex/clock).
func TestDailyCounter_Allow_UnlimitedWhenLimitNonPositive(t *testing.T) {
	c := NewDailyCounter(0, nil)
	for i := 0; i < 5; i++ {
		if !c.Allow() {
			t.Fatal("limit<=0 must mean Allow always true")
		}
	}
}

func TestHourlyCounter_Allow(t *testing.T) {
	clock := &fakeClock{now: mustParse(t, "2026-08-18T10:59:59Z")}
	c := NewHourlyCounter(1, clock)

	if !c.Allow() {
		t.Fatal("fresh counter under limit should Allow")
	}
	if !c.TryIncrement() {
		t.Fatal("increment at limit 1 should succeed")
	}
	if c.Allow() {
		t.Fatal("Allow must report false once the limit is reached within the same hour")
	}

	clock.Set(mustParse(t, "2026-08-18T11:00:00Z"))
	if !c.Allow() {
		t.Fatal("Allow must report true again in the new UTC hour")
	}
}

// TestHourlyCounter_Allow_UnlimitedWhenLimitNonPositive covers the limit<=0 unlimited fast path.
func TestHourlyCounter_Allow_UnlimitedWhenLimitNonPositive(t *testing.T) {
	c := NewHourlyCounter(0, nil)
	for i := 0; i < 5; i++ {
		if !c.Allow() {
			t.Fatal("limit<=0 must mean Allow always true")
		}
	}
}

// TestHourlyCounter_Count covers HourlyCounter.Count (0% before this test): it must reflect
// TryIncrement's running total within the hour and observe the UTC-hour reset.
func TestHourlyCounter_Count(t *testing.T) {
	clock := &fakeClock{now: mustParse(t, "2026-08-18T10:00:00Z")}
	c := NewHourlyCounter(5, clock)

	if c.Count() != 0 {
		t.Fatalf("Count() on a fresh counter = %d, want 0", c.Count())
	}
	c.TryIncrement()
	c.TryIncrement()
	if c.Count() != 2 {
		t.Fatalf("Count() = %d, want 2", c.Count())
	}

	clock.Set(mustParse(t, "2026-08-18T11:00:00Z"))
	if c.Count() != 0 {
		t.Fatalf("Count() after UTC-hour reset = %d, want 0", c.Count())
	}
}

func TestHourlyCounter_ResetOnHourBoundary(t *testing.T) {
	clock := &fakeClock{now: mustParse(t, "2026-08-18T10:59:59Z")}
	c := NewHourlyCounter(1, clock)

	if !c.TryIncrement() {
		t.Fatal("1st increment should be allowed")
	}
	if c.TryIncrement() {
		t.Fatal("2nd increment must be rejected at limit 1 within the same hour")
	}

	clock.Set(mustParse(t, "2026-08-18T11:00:00Z"))
	if !c.TryIncrement() {
		t.Fatal("increment must be allowed again in the new UTC hour")
	}
}

// TestHourlyCounter_SeedCount is circuit-config-sec finding 3's proof for HourlyCounter.SeedCount
// (0% before this fix -- the method did not exist, and WORKER_MAX_TRIAGE_PER_HOUR reset to zero on
// every restart within the same UTC hour). Mirrors DailyCounter.SeedCount's contract: only ever
// raises the count, never lowers it, and a stale hour's leftover count is reset first.
func TestHourlyCounter_SeedCount(t *testing.T) {
	clock := &fakeClock{now: mustParse(t, "2026-08-18T10:00:00Z")}
	c := NewHourlyCounter(5, clock)

	c.SeedCount(3)
	if got := c.Count(); got != 3 {
		t.Fatalf("Count() after SeedCount(3) = %d, want 3", got)
	}

	// Real-path proof: seeding to at/above the limit must actually block a subsequent TryIncrement
	// in the same hour, exactly as if those slots had been consumed by real TryIncrement calls
	// before a crash.
	c.SeedCount(5)
	if c.TryIncrement() {
		t.Fatal("TryIncrement after SeedCount(5) at limit 5 must be denied")
	}

	// SeedCount must never lower an existing count.
	c2 := NewHourlyCounter(10, clock)
	c2.SeedCount(4)
	c2.SeedCount(2)
	if got := c2.Count(); got != 4 {
		t.Fatalf("SeedCount(2) after SeedCount(4) lowered the count to %d, want 4 (must only ever raise)", got)
	}

	// A stale hour's leftover count must not leak into the new hour's seed.
	c3 := NewHourlyCounter(10, clock)
	c3.TryIncrement()
	c3.TryIncrement()
	clock.Set(mustParse(t, "2026-08-18T11:00:00Z"))
	c3.SeedCount(1)
	if got := c3.Count(); got != 1 {
		t.Fatalf("SeedCount(1) in a new hour = %d, want 1 (must not add onto the prior hour's stale count)", got)
	}
}

func TestHourlyCounter_ConcurrentRace(t *testing.T) {
	clock := &fakeClock{now: mustParse(t, "2026-08-18T10:00:00Z")}
	c := NewHourlyCounter(20, clock)

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.TryIncrement() {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 20 {
		t.Fatalf("allowed = %d, want exactly 20", allowed)
	}
}

// TestDailyCounter_Remaining is finding 10's RED-FIRST proof: DailyCounter needs a Remaining()
// method (mirroring DailyBudget.Remaining's own contract) before main.go can expose a
// fix_jobs_remaining / prs_remaining gauge for it -- before this fix, DailyCounter had no such
// method at all (a compile error, not just a wrong value).
func TestDailyCounter_Remaining(t *testing.T) {
	clock := &fakeClock{now: mustParse(t, "2026-08-18T10:00:00Z")}
	c := NewDailyCounter(3, clock)
	if got := c.Remaining(); got != 3 {
		t.Fatalf("Remaining() before any increment = %d, want 3", got)
	}
	c.TryIncrement()
	if got := c.Remaining(); got != 2 {
		t.Fatalf("Remaining() after one increment = %d, want 2", got)
	}
	c.TryIncrement()
	c.TryIncrement()
	if got := c.Remaining(); got != 0 {
		t.Fatalf("Remaining() at the limit = %d, want 0", got)
	}

	clock.Set(mustParse(t, "2026-08-19T00:00:00Z"))
	if got := c.Remaining(); got != 3 {
		t.Fatalf("Remaining() after UTC-day reset = %d, want 3", got)
	}

	unlimited := NewDailyCounter(0, clock)
	if got := unlimited.Remaining(); got != -1 {
		t.Fatalf("Remaining() for a non-positive limit = %d, want -1 (unlimited)", got)
	}
}

// TestHourlyCounter_Remaining is finding 10's RED-FIRST proof for the hourly caps
// (WORKER_MAX_TRIAGE_PER_HOUR / WORKER_MAX_FOLLOWUP_PER_HOUR) needing the same gauge-friendly
// Remaining() method, resetting on the UTC hour boundary rather than the day boundary.
func TestHourlyCounter_Remaining(t *testing.T) {
	clock := &fakeClock{now: mustParse(t, "2026-08-18T10:00:00Z")}
	c := NewHourlyCounter(2, clock)
	if got := c.Remaining(); got != 2 {
		t.Fatalf("Remaining() before any increment = %d, want 2", got)
	}
	c.TryIncrement()
	c.TryIncrement()
	if got := c.Remaining(); got != 0 {
		t.Fatalf("Remaining() at the limit = %d, want 0", got)
	}

	clock.Set(mustParse(t, "2026-08-18T11:00:00Z"))
	if got := c.Remaining(); got != 2 {
		t.Fatalf("Remaining() after UTC-hour reset = %d, want 2", got)
	}

	unlimited := NewHourlyCounter(0, clock)
	if got := unlimited.Remaining(); got != -1 {
		t.Fatalf("Remaining() for a non-positive limit = %d, want -1 (unlimited)", got)
	}
}
