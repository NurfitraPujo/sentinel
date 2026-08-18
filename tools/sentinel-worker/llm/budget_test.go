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
