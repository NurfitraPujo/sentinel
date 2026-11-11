package unit

import (
	"context"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/degradation"
	"github.com/stretchr/testify/assert"
)

// This file used to test EventBuffer (Push/Drain/Size/overflow) and
// GracefulDegradation.CheckAndBuffer/BufferSize as if they buffered events in
// process memory during a database outage. That mechanism is deleted, not
// merely untested — see the package doc comment on
// apps/processor-go/degradation/buffer.go and docs/memory/BUGS.md B1 for why
// it could not be repaired: buffering + ACKing lost events on a crash/restart
// (measured: 3 events buffered+ACKed, process killed, restarted -> 0 rows, no
// redelivery, no DLQ entry), and buffering + NAKing double-processes, because
// the flush would replay AND NATS redelivery would replay, and issues.count
// is an ON CONFLICT increment. Do not restore those tests from git history —
// the API they exercised (EventBuffer, Push, Drain, BufferSize,
// SetFlushHandler, triggerAsyncFlush, CheckAndBuffer, MaxBufferSize,
// StatusBuffered, StatusDropped) no longer exists. Durability now belongs
// entirely to NATS redelivery (docs/memory/DECISIONS.md D10); what remains of
// this package is a plain database-health gate, tested below.

func TestGracefulDegradation_IsAvailableByDefault(t *testing.T) {
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return true })

	assert.True(t, g.IsAvailable())
}

func TestEvaluate_DBUp_ReturnsProcessed(t *testing.T) {
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return true })

	status := g.Evaluate(context.Background(), []byte("event-1"))

	assert.Equal(t, degradation.StatusProcessed, status)
	assert.True(t, g.IsAvailable())
}

func TestEvaluate_DBDown_ReturnsUnavailable(t *testing.T) {
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return false })

	status := g.Evaluate(context.Background(), []byte("event-1"))

	assert.Equal(t, degradation.StatusUnavailable, status,
		"a down database must report Unavailable so the caller errors and NATS redelivers")
	assert.False(t, g.IsAvailable())
}

func TestEvaluate_TransitionUnavailableToAvailable(t *testing.T) {
	healthy := false
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return healthy })

	assert.Equal(t, degradation.StatusUnavailable, g.Evaluate(context.Background(), []byte("e1")))
	assert.False(t, g.IsAvailable())

	healthy = true
	assert.Equal(t, degradation.StatusProcessed, g.Evaluate(context.Background(), []byte("e2")))
	assert.True(t, g.IsAvailable(), "recovery must be observed on the next evaluation")
}

// Note: there is deliberately no "healthy -> unhealthy transition observed on
// the very next call" test. isHealthy's short-lived positive-result cache
// (healthCacheTTL, buffer.go) is intentionally NOT bypassed just because the
// underlying dbChecker flips to false a moment later — see that constant's
// doc comment for the asymmetry (cache a healthy reading briefly; never cache
// an unhealthy one). Asserting immediate detection here would pin an
// implementation detail (the cache window) rather than the contract.
