package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

const (
	subscribeRetryInitial = 1 * time.Second
	subscribeRetryMax     = 30 * time.Second
)

// isJetStreamUnavailable reports whether a Subscribe failure is the transient "JetStream is not
// answering right now" class (meta leader election, rolling restart, request timeout) as opposed to a
// configuration or contract error that retrying cannot fix.
func isJetStreamUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, natsgo.ErrTimeout) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, natsgo.ErrNoResponders) ||
		errors.Is(err, natsgo.ErrJetStreamNotEnabled) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "jetstream system temporarily unavailable") ||
		strings.Contains(msg, "jetstream not enabled") ||
		strings.Contains(msg, "timeout")
}

// subscribeWithRetry calls subscribe until it succeeds, retrying only JetStream-unavailable errors with
// capped exponential backoff (initial..max) until ctx is done. ready is false for the whole retry window
// and set true on success. Any other error is returned immediately (fail fast).
func subscribeWithRetry(ctx context.Context, subscribe func() error, ready *atomic.Bool, initial, max time.Duration) error {
	ready.Store(false)
	delay := initial
	for {
		err := subscribe()
		if err == nil {
			ready.Store(true)
			return nil
		}
		if !isJetStreamUnavailable(err) {
			return err
		}
		slog.WarnContext(ctx, "JetStream unavailable while subscribing; retrying",
			slog.String("error", err.Error()), slog.Duration("retry_in", delay))
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
		if delay *= 2; delay > max {
			delay = max
		}
	}
}
