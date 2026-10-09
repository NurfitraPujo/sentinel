package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

func TestIsJetStreamUnavailable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("failed to create pull subscription: nats: JetStream system temporarily unavailable"), true},
		{fmt.Errorf("failed to create pull subscription: %w", natsgo.ErrTimeout), true},
		{fmt.Errorf("x: %w", context.DeadlineExceeded), true},
		{fmt.Errorf("x: %w", natsgo.ErrNoResponders), true},
		{fmt.Errorf("x: %w", natsgo.ErrStreamNotFound), false},
		{errors.New("permissions violation"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isJetStreamUnavailable(c.err); got != c.want {
			t.Errorf("isJetStreamUnavailable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestSubscribeWithRetryRecovers(t *testing.T) {
	var ready atomic.Bool
	calls := 0
	err := subscribeWithRetry(context.Background(), func() error {
		calls++
		if ready.Load() {
			t.Error("ready must be false while retrying")
		}
		if calls < 3 {
			return errors.New("nats: JetStream system temporarily unavailable")
		}
		return nil
	}, &ready, time.Millisecond, 4*time.Millisecond)
	if err != nil || calls != 3 || !ready.Load() {
		t.Fatalf("err=%v calls=%d ready=%v", err, calls, ready.Load())
	}
}

func TestSubscribeWithRetryFailsFastOnOtherErrors(t *testing.T) {
	var ready atomic.Bool
	calls := 0
	boom := errors.New("stream config mismatch")
	err := subscribeWithRetry(context.Background(), func() error { calls++; return boom }, &ready, time.Millisecond, time.Millisecond)
	if !errors.Is(err, boom) || calls != 1 || ready.Load() {
		t.Fatalf("err=%v calls=%d ready=%v", err, calls, ready.Load())
	}
}

func TestSubscribeWithRetryStopsOnContext(t *testing.T) {
	var ready atomic.Bool
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := subscribeWithRetry(ctx, func() error { return natsgo.ErrTimeout }, &ready, time.Millisecond, 2*time.Millisecond)
	if err == nil || ready.Load() {
		t.Fatalf("err=%v ready=%v", err, ready.Load())
	}
}

func TestLivezAlwaysOK(t *testing.T) {
	rec := httptest.NewRecorder()
	livezHandler(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("livez = %d, want 200", rec.Code)
	}
}
