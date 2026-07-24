package unit

import (
	"context"
	"errors"
	"sync/atomic"
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

func TestCheckAndBuffer_DBDown_ReturnsTrueOnPushSuccess(t *testing.T) {
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return false })

	ok := g.CheckAndBuffer(context.Background(), []byte("event-1"))
	assert.True(t, ok, "Push succeeded so CheckAndBuffer should return true")
	assert.False(t, g.IsAvailable(), "should be marked unavailable")
	assert.Equal(t, 1, g.BufferSize(), "event should be buffered")
}

func TestCheckAndBuffer_DBDown_PushReturnsFalseOnCapacity(t *testing.T) {
	// Fill the internal buffer to capacity, then verify the next CheckAndBuffer
	// returns the Push result (false) when DB is still down.
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return false })
	for i := 0; i < degradation.MaxBufferSize; i++ {
		require.True(t, g.CheckAndBuffer(context.Background(), []byte{byte(i)}))
	}

	ok := g.CheckAndBuffer(context.Background(), []byte("overflow"))
	assert.False(t, ok, "CheckAndBuffer should return false when push fails")
	assert.False(t, g.IsAvailable())
	assert.Equal(t, degradation.MaxBufferSize, g.BufferSize())
}

func TestCheckAndBuffer_TransitionUnavailableToAvailable(t *testing.T) {
	var dbUp atomic.Bool
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return dbUp.Load() })

	// Phase 1: DB is down; two events get buffered.
	dbUp.Store(false)
	require.True(t, g.CheckAndBuffer(context.Background(), []byte("a")))
	require.True(t, g.CheckAndBuffer(context.Background(), []byte("b")))
	require.False(t, g.IsAvailable())
	require.Equal(t, 2, g.BufferSize())

	// Phase 2: DB comes back. Next CheckAndBuffer should:
	//   - flip IsAvailable back to true
	//   - NOT buffer the new event
	dbUp.Store(true)
	ok := g.CheckAndBuffer(context.Background(), []byte("c"))
	assert.True(t, ok)
	assert.True(t, g.IsAvailable(), "IsAvailable should clear after restore")
	assert.Equal(t, 2, g.BufferSize(), "restored event must not be buffered")
}

// ---------------------------------------------------------------------------
// Flush tests
// ---------------------------------------------------------------------------

func TestFlush_UnavailableDoesNotDrain(t *testing.T) {
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return false })
	require.True(t, g.CheckAndBuffer(context.Background(), []byte("a")))
	require.True(t, g.CheckAndBuffer(context.Background(), []byte("b")))
	require.False(t, g.IsAvailable())
	require.Equal(t, 2, g.BufferSize())

	var called atomic.Bool
	processor := func(_ []byte) error {
		called.Store(true)
		return nil
	}

	flushed := g.Flush(context.Background(), processor)
	assert.Equal(t, 0, flushed, "Flush should return 0 when unavailable")
	assert.False(t, called.Load(), "processor must not be invoked when unavailable")
	assert.Equal(t, 2, g.BufferSize(), "buffer must be untouched when unavailable")
}

func TestFlush_EmptyBufferReturnsZero(t *testing.T) {
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return true })
	require.True(t, g.IsAvailable())

	var called atomic.Bool
	processor := func(_ []byte) error {
		called.Store(true)
		return nil
	}

	flushed := g.Flush(context.Background(), processor)
	assert.Equal(t, 0, flushed)
	assert.False(t, called.Load(), "processor must not be invoked on empty buffer")
}

func TestFlush_AllEventsSucceed(t *testing.T) {
	var dbUp atomic.Bool
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return dbUp.Load() })

	// Buffer events while DB is down so Flush has something to process.
	dbUp.Store(false)
	require.True(t, g.CheckAndBuffer(context.Background(), []byte("a")))
	require.True(t, g.CheckAndBuffer(context.Background(), []byte("b")))
	require.True(t, g.CheckAndBuffer(context.Background(), []byte("c")))
	require.Equal(t, 3, g.BufferSize())
	require.False(t, g.IsAvailable())

	// DB is back up: one CheckAndBuffer transitions IsAvailable back to true
	// without buffering (this is the unavailable→available transition path).
	dbUp.Store(true)
	require.True(t, g.CheckAndBuffer(context.Background(), []byte("ignored")))

	var received []string
	processor := func(data []byte) error {
		received = append(received, string(data))
		return nil
	}

	flushed := g.Flush(context.Background(), processor)
	assert.Equal(t, 3, flushed)
	assert.Equal(t, []string{"a", "b", "c"}, received)
	assert.Equal(t, 0, g.BufferSize(), "buffer should be empty after successful flush")
}

func TestFlush_PartialFailureCountsFailed(t *testing.T) {
	var dbUp atomic.Bool
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return dbUp.Load() })

	dbUp.Store(false)
	require.True(t, g.CheckAndBuffer(context.Background(), []byte("a")))
	require.True(t, g.CheckAndBuffer(context.Background(), []byte("b")))
	require.True(t, g.CheckAndBuffer(context.Background(), []byte("c")))
	require.Equal(t, 3, g.BufferSize())
	require.False(t, g.IsAvailable())

	// Restore availability so Flush will run.
	dbUp.Store(true)
	require.True(t, g.CheckAndBuffer(context.Background(), []byte("ignored")))

	boom := errors.New("processor boom")
	var received []string
	processor := func(data []byte) error {
		received = append(received, string(data))
		if string(data) == "b" {
			return boom
		}
		return nil
	}

	flushed := g.Flush(context.Background(), processor)
	assert.Equal(t, 2, flushed, "only successful events are counted")
	assert.Equal(t, []string{"a", "b", "c"}, received, "all events are still processed in order")
	assert.Equal(t, 0, g.BufferSize(), "buffer is drained even on partial failure")
}

// ---------------------------------------------------------------------------
// BufferSize tests
// ---------------------------------------------------------------------------

func TestBufferSize_ReflectsCurrentOccupancy(t *testing.T) {
	g := degradation.NewGracefulDegradation(func(_ context.Context) bool { return false })
	assert.Equal(t, 0, g.BufferSize())

	require.True(t, g.CheckAndBuffer(context.Background(), []byte("a")))
	assert.Equal(t, 1, g.BufferSize())

	require.True(t, g.CheckAndBuffer(context.Background(), []byte("b")))
	assert.Equal(t, 2, g.BufferSize())

	require.True(t, g.CheckAndBuffer(context.Background(), []byte("c")))
	assert.Equal(t, 3, g.BufferSize())
}
