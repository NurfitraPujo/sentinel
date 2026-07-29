package unit

import (
	"context"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/degradation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// EventBuffer tests
// ---------------------------------------------------------------------------

func TestEventBuffer_PushUnderCapacity(t *testing.T) {
	b := degradation.NewEventBuffer(10)

	ok := b.Push([]byte("a"))
	require.True(t, ok)
	assert.Equal(t, 1, b.Size())

	ok = b.Push([]byte("b"))
	require.True(t, ok)
	assert.Equal(t, 2, b.Size())
}

func TestEventBuffer_PushAtCapacityDrops(t *testing.T) {
	b := degradation.NewEventBuffer(2)

	require.True(t, b.Push([]byte("a")))
	require.True(t, b.Push([]byte("b")))
	require.Equal(t, 2, b.Size())

	ok := b.Push([]byte("c"))
	assert.False(t, ok, "Push should return false when buffer is full")
	assert.Equal(t, 2, b.Size(), "buffer size should remain at capacity")
}

func TestEventBuffer_DrainReturnsAllAndEmpties(t *testing.T) {
	b := degradation.NewEventBuffer(5)
	require.True(t, b.Push([]byte("a")))
	require.True(t, b.Push([]byte("b")))
	require.True(t, b.Push([]byte("c")))

	events := b.Drain()
	require.Len(t, events, 3)
	assert.Equal(t, "a", string(events[0].Data))
	assert.Equal(t, "b", string(events[1].Data))
	assert.Equal(t, "c", string(events[2].Data))
	assert.Equal(t, 0, b.Size(), "Drain must empty the buffer")
}

func TestEventBuffer_DrainOnEmptyReturnsEmpty(t *testing.T) {
	b := degradation.NewEventBuffer(5)

	events := b.Drain()
	assert.Empty(t, events)
	assert.Equal(t, 0, b.Size())
}

func TestEventBuffer_DrainAllowsReuse(t *testing.T) {
	b := degradation.NewEventBuffer(3)
	require.True(t, b.Push([]byte("x")))
	b.Drain()

	// After drain, buffer should accept new pushes again.
	require.True(t, b.Push([]byte("y")))
	require.True(t, b.Push([]byte("z")))
	assert.Equal(t, 2, b.Size())
}

func TestNewEventBuffer_ZeroFallsBackToMaxBufferSize(t *testing.T) {
	b := degradation.NewEventBuffer(0)

	// The buffer should accept up to MaxBufferSize events.
	for i := 0; i < degradation.MaxBufferSize; i++ {
		ok := b.Push([]byte{byte(i)})
		require.True(t, ok, "Push %d should succeed", i)
	}
	assert.Equal(t, degradation.MaxBufferSize, b.Size())

	// One more push should be dropped.
	ok := b.Push([]byte("overflow"))
	assert.False(t, ok)
	assert.Equal(t, degradation.MaxBufferSize, b.Size())
}

// ---------------------------------------------------------------------------
// GracefulDegradation tests
// ---------------------------------------------------------------------------

func TestGracefulDegradation_IsAvailableByDefault(t *testing.T) {
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return true })

	assert.True(t, g.IsAvailable())
	assert.Equal(t, 0, g.BufferSize())
}

func TestCheckAndBuffer_DBUp_ReturnsTrueWithoutBuffering(t *testing.T) {
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return true })

	ok := g.CheckAndBuffer(context.Background(), []byte("event-1"))
	assert.True(t, ok)
	assert.True(t, g.IsAvailable())
	assert.Equal(t, 0, g.BufferSize(), "event must not be buffered when DB is up")
}

// The tests below replaced seven that asserted GracefulDegradation BUFFERS events while the database
// is down. That behavior was removed deliberately, not accidentally.
//
// Buffering only looked safe: the caller ACKed the NATS message on the strength of an in-PROCESS
// buffer, so a crash or redeploy during an outage destroyed the event outright (measured: 3 events
// buffered+ACKed, processor killed, restarted -> 0 rows, no redelivery, no DLQ entry). Buffering AND
// NAKing is not an alternative either: the event would be replayed once by the flush and once by
// redelivery, and issues.count is an ON CONFLICT increment, so the duplicate is visible in the
// product. With no idempotency key on the event (see VERIFIED_STATE.md S16 on event_id having no
// server-side destination), there is no third option.
//
// So durability now belongs entirely to NATS: D10's bounded retry with backoff, then a DLQ entry for
// anything that outlasts the budget. See docs/memory/DECISIONS.md D10 and BUGS.md B1.

func TestEvaluate_DBDown_DoesNotTakeCustodyOfEvent(t *testing.T) {
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return false })

	status := g.Evaluate(context.Background(), []byte("event-1"))

	assert.Equal(t, degradation.StatusUnavailable, status,
		"a down database must report Unavailable so the caller errors and NATS redelivers")
	assert.False(t, g.IsAvailable())
	assert.Equal(t, 0, g.BufferSize(),
		"the event must NOT be held in process memory — ACKing on the strength of an in-memory buffer is how events were silently lost")
}

func TestEvaluate_DBUp_ProcessesWithoutBuffering(t *testing.T) {
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return true })

	status := g.Evaluate(context.Background(), []byte("event-1"))

	assert.Equal(t, degradation.StatusProcessed, status)
	assert.True(t, g.IsAvailable())
	assert.Equal(t, 0, g.BufferSize())
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
