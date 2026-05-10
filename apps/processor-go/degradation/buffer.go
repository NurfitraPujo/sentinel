package degradation

import (
	"context"
	"log"
	"sync"
	"time"
)

const MaxBufferSize = 10000

type BufferedEvent struct {
	Data      []byte
	Timestamp time.Time
}

type EventBuffer struct {
	mu      sync.Mutex
	buffer  []BufferedEvent
	maxSize int
}

func NewEventBuffer(maxSize int) *EventBuffer {
	if maxSize <= 0 {
		maxSize = MaxBufferSize
	}
	return &EventBuffer{
		buffer:  make([]BufferedEvent, 0, maxSize),
		maxSize: maxSize,
	}
}

func (b *EventBuffer) Push(event []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.buffer) >= b.maxSize {
		log.Printf("WARNING: Event buffer full (%d), dropping event", b.maxSize)
		return false
	}

	b.buffer = append(b.buffer, BufferedEvent{
		Data:      event,
		Timestamp: time.Now(),
	})

	return true
}

func (b *EventBuffer) Drain() []BufferedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()

	events := b.buffer
	b.buffer = make([]BufferedEvent, 0, b.maxSize)
	return events
}

func (b *EventBuffer) Size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buffer)
}

type GracefulDegradation struct {
	buffer      *EventBuffer
	isAvailable bool
	mu          sync.RWMutex
	dbChecker   func(context.Context) bool
}

func NewGracefulDegradation(dbChecker func(context.Context) bool) *GracefulDegradation {
	return &GracefulDegradation{
		buffer:      NewEventBuffer(MaxBufferSize),
		isAvailable: true,
		dbChecker:   dbChecker,
	}
}

func (g *GracefulDegradation) IsAvailable() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.isAvailable
}

func (g *GracefulDegradation) CheckAndBuffer(ctx context.Context, event []byte) bool {
	if g.dbChecker(ctx) {
		if !g.IsAvailable() {
			g.mu.Lock()
			g.isAvailable = true
			g.mu.Unlock()
			log.Printf("Database connection restored, flushing %d buffered events", g.buffer.Size())
		}
		return true
	}

	g.mu.Lock()
	g.isAvailable = false
	g.mu.Unlock()

	log.Printf("WARNING: Database unavailable, buffering event (buffer size: %d)", g.buffer.Size())
	return g.buffer.Push(event)
}

func (g *GracefulDegradation) Flush(ctx context.Context, processor func([]byte) error) int {
	if g.IsAvailable() {
		return 0
	}

	events := g.buffer.Drain()
	if len(events) == 0 {
		return 0
	}

	flushed := 0
	for _, event := range events {
		if err := processor(event.Data); err != nil {
			log.Printf("Failed to flush event: %v", err)
			continue
		}
		flushed++
	}

	log.Printf("Flushed %d/%d buffered events", flushed, len(events))
	return flushed
}

func (g *GracefulDegradation) BufferSize() int {
	return g.buffer.Size()
}
