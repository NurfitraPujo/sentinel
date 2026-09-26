package sentinel

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestBreadcrumbsConcurrentSameContext is P2.1: goroutines working for the
// same request share one buffer and are race-free.
func TestBreadcrumbsConcurrentSameContext(t *testing.T) {
	ctx := WithBreadcrumbBuffer(context.Background(), 100)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				AddBreadcrumb(ctx, Breadcrumb{Message: "m", Data: map[string]interface{}{"g": g}})
			}
		}(g)
	}
	wg.Wait()

	crumbs := BreadcrumbsFromContext(ctx)
	if len(crumbs) != 100 {
		t.Errorf("len(crumbs) = %d, want 100 (the cap)", len(crumbs))
	}
}

// TestBreadcrumbBufferCap is P2.3: the ring keeps only the last max
// breadcrumbs, oldest dropped.
func TestBreadcrumbBufferCap(t *testing.T) {
	ctx := WithBreadcrumbBuffer(context.Background(), 3)
	for i := 0; i < 5; i++ {
		AddBreadcrumb(ctx, Breadcrumb{Message: itoaTest(i)})
	}
	crumbs := BreadcrumbsFromContext(ctx)
	if len(crumbs) != 3 {
		t.Fatalf("len(crumbs) = %d, want 3", len(crumbs))
	}
	want := []string{"2", "3", "4"}
	for i, c := range crumbs {
		if c.Message != want[i] {
			t.Errorf("crumbs[%d].Message = %q, want %q (oldest dropped, order preserved)", i, c.Message, want[i])
		}
	}
}

func itoaTest(n int) string {
	return string(rune('0' + n))
}

// TestTransportTrimsToConfigMaxBreadcrumbs is P2.3: the transport trims to
// Config.MaxBreadcrumbs at send time, on top of whatever the buffer itself
// held.
func TestTransportTrimsToConfigMaxBreadcrumbs(t *testing.T) {
	ctx := WithBreadcrumbBuffer(context.Background(), 100)
	for i := 0; i < 40; i++ {
		AddBreadcrumb(ctx, Breadcrumb{Message: "m"})
	}
	ev := NewEvent(Config{}, nil, nil)
	ApplyContext(ctx, ev)
	if len(ev.Breadcrumbs) != 40 {
		t.Fatalf("setup: expected 40 breadcrumbs on the event, got %d", len(ev.Breadcrumbs))
	}
	b := boundEvent(ev, 10)
	if len(b.Breadcrumbs) != 10 {
		t.Errorf("len(b.Breadcrumbs) = %d, want 10 (Config.MaxBreadcrumbs)", len(b.Breadcrumbs))
	}
}

// TestAddBreadcrumbWithoutBufferIsNoop is P2.4.
func TestAddBreadcrumbWithoutBufferIsNoop(t *testing.T) {
	AddBreadcrumb(context.Background(), Breadcrumb{Message: "m"}) // must not panic
	AddBreadcrumb(nil, Breadcrumb{Message: "m"})                  // must not panic
	if got := BreadcrumbsFromContext(context.Background()); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

// TestAddBreadcrumbNormalizes is P2.7: timestamp, level and empty Data are
// normalized on the way in.
func TestAddBreadcrumbNormalizes(t *testing.T) {
	ctx := WithBreadcrumbBuffer(context.Background(), 10)
	before := time.Now()
	AddBreadcrumb(ctx, Breadcrumb{Message: "m", Level: "warn", Data: map[string]interface{}{}})
	crumbs := BreadcrumbsFromContext(ctx)
	if len(crumbs) != 1 {
		t.Fatalf("expected 1 crumb, got %d", len(crumbs))
	}
	c := crumbs[0]
	if c.Timestamp.Before(before) {
		t.Errorf("expected a zero Timestamp to become now, got %v before %v", c.Timestamp, before)
	}
	if c.Level != "warning" {
		t.Errorf("Level = %q, want warning (normalized)", c.Level)
	}
	if c.Data != nil {
		t.Errorf("expected an empty Data map to become nil, got %v", c.Data)
	}

	fixed := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	AddBreadcrumb(ctx, Breadcrumb{Timestamp: fixed})
	crumbs = BreadcrumbsFromContext(ctx)
	if !crumbs[len(crumbs)-1].Timestamp.Equal(fixed) {
		t.Error("a non-zero Timestamp must be kept as is")
	}
}

// TestBreadcrumbsSeparateRequestsThroughMiddleware is P2.2: a separate
// WithBreadcrumbBuffer call is a separate object.
func TestBreadcrumbsSeparateRequestsThroughMiddleware(t *testing.T) {
	ctx1 := WithBreadcrumbBuffer(context.Background(), 10)
	ctx2 := WithBreadcrumbBuffer(context.Background(), 10)
	AddBreadcrumb(ctx1, Breadcrumb{Message: "req1"})
	AddBreadcrumb(ctx2, Breadcrumb{Message: "req2"})

	c1 := BreadcrumbsFromContext(ctx1)
	c2 := BreadcrumbsFromContext(ctx2)
	if len(c1) != 1 || c1[0].Message != "req1" {
		t.Errorf("ctx1 crumbs = %v, want [req1]", c1)
	}
	if len(c2) != 1 || c2[0].Message != "req2" {
		t.Errorf("ctx2 crumbs = %v, want [req2]", c2)
	}
}
