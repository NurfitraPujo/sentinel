package tagfacets

import (
	"testing"
	"time"
)

// TestPauseAfter kills the tfb-backfill-safety-controls-unproven mutation "PauseAfter returns 0" (no
// duty-cycle pacing, so hot issues would be held ~100% of the time instead of ≤ Duty of it, spec §4.2).
func TestPauseAfter(t *testing.T) {
	cases := []struct {
		name           string
		elapsed, pause time.Duration
		duty           float64
		want           time.Duration
	}{
		{"duty 0.25: pause is 3x elapsed", time.Second, 200 * time.Millisecond, 0.25, 3 * time.Second},
		{"duty 1: no pacing needed, floor to pause", time.Second, 200 * time.Millisecond, 1, 200 * time.Millisecond},
		{"computed pause below the floor: the floor wins", 10 * time.Millisecond, 500 * time.Millisecond, 0.9, 500 * time.Millisecond},
		{"zero elapsed: floor to pause", 0, 200 * time.Millisecond, 0.25, 200 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PauseAfter(c.elapsed, c.pause, c.duty); got != c.want {
				t.Fatalf("PauseAfter(%s, %s, %v) = %s, want %s", c.elapsed, c.pause, c.duty, got, c.want)
			}
		})
	}
}

// TestPauseAfterNeverBelowFloor is the general shape the mutation "PauseAfter returns 0" breaks: whatever
// the inputs, the pause is never less than the floor, so a batch can never run back-to-back with none.
func TestPauseAfterNeverBelowFloor(t *testing.T) {
	for _, elapsed := range []time.Duration{0, time.Millisecond, time.Second, time.Minute} {
		for _, duty := range []float64{0.01, 0.25, 0.5, 1} {
			pause := 200 * time.Millisecond
			if got := PauseAfter(elapsed, pause, duty); got < pause {
				t.Fatalf("PauseAfter(%s, %s, %v) = %s, below the floor %s", elapsed, pause, duty, got, pause)
			}
		}
	}
}
