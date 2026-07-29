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

// healthCacheTTL bounds how long a positive health result is trusted before
// CheckAndBuffer/Evaluate re-checks the database directly. Previously every
// single event paid the cost of its own db.Ping (VERIFIED_STATE.md S9); at
// pipeline throughput this meaningfully adds to per-event latency and load on
// the connection pool for no benefit in the (overwhelmingly common) steady
// "database is healthy" case.
//
// The cache is intentionally NOT consulted while the DB is currently marked
// unavailable — every event evaluated during an outage re-checks for real,
// so recovery is detected on the very next event after the DB actually comes
// back rather than being delayed by a stale cache entry. This asymmetry
// (cache when healthy, always verify when not) is deliberate: the cost of a
// stale "down" reading is a few extra seconds of buffering/dropping, which is
// safe; the cost of a stale "up" reading is a live write attempted against a
// database that is not actually there, which is already handled safely by
// classifyStoreError + the NATS bounded-retry/backoff path (D10) — see
// docs/memory/DECISIONS.md.
const healthCacheTTL = 2 * time.Second

// BufferStatus is the outcome of evaluating one event against the current
// database health. It replaces the single ambiguous bool CheckAndBuffer used
// to return, which conflated "processed live" and "safely buffered" into the
// same `true` — the exact confusion VERIFIED_STATE.md S9 describes: the call
// site could not tell "healthy" from "down, but buffered" and ran
// processEventInternal in both cases.
type BufferStatus int

const (
	// StatusProcessed means the database was healthy: the caller must run
	// its normal live-processing path.
	StatusProcessed BufferStatus = iota
	// StatusBuffered means the database was down but the event was
	// accepted into the in-memory buffer. The caller must NOT attempt to
	// process it live; it will be replayed by a recovery-triggered flush.
	StatusBuffered
	// StatusDropped means the database was down AND the buffer was full:
	// the event was neither stored nor buffered. The caller must not treat
	// this as success.
	// StatusUnavailable means the database is down and the event was deliberately NOT taken into
	// process memory. The caller MUST return an error so NATS redelivers it (D10).
	StatusUnavailable
	StatusDropped
)

func (s BufferStatus) String() string {
	switch s {
	case StatusProcessed:
		return "processed"
	case StatusBuffered:
		return "buffered"
	case StatusDropped:
		return "dropped"
	default:
		return "unknown"
	}
}

type GracefulDegradation struct {
	buffer      *EventBuffer
	isAvailable bool
	mu          sync.RWMutex
	dbChecker   func(context.Context) bool

	lastHealthCheck time.Time

	// flushFn, when set (via SetFlushHandler), is invoked to replay each
	// buffered event once a recovery transition (unavailable -> available)
	// is observed. It is nil in every test that constructs
	// GracefulDegradation directly without calling SetFlushHandler, which is
	// exactly what keeps this package's own unit tests (which call Flush
	// manually) behaving identically to before this change.
	flushMu sync.RWMutex
	flushFn func([]byte) error
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

// SetFlushHandler installs the function used to replay buffered events after
// a recovery is detected. It is intended to be called once, at construction
// time, by service.NewProcessorService (wiring processEventInternal back in
// without processEventInternal itself ever calling Flush — see
// triggerAsyncFlush's doc comment for why that matters).
func (g *GracefulDegradation) SetFlushHandler(fn func([]byte) error) {
	g.flushMu.Lock()
	defer g.flushMu.Unlock()
	g.flushFn = fn
}

// isHealthy reports current database health, consulting the short-lived
// cache described on healthCacheTTL. It also updates isAvailable/
// lastHealthCheck whenever it performs a real check.
func (g *GracefulDegradation) isHealthy(ctx context.Context) bool {
	g.mu.RLock()
	available := g.isAvailable
	fresh := time.Since(g.lastHealthCheck) < healthCacheTTL
	g.mu.RUnlock()

	if available && fresh {
		return true
	}

	healthy := g.dbChecker(ctx)
	g.mu.Lock()
	g.lastHealthCheck = time.Now()
	g.mu.Unlock()
	return healthy
}

// Evaluate is the tri-state replacement for CheckAndBuffer's ambiguous bool
// (VERIFIED_STATE.md S9). Callers must branch on all three BufferStatus
// values; in particular processEventInternal must never run for an event
// that did not return StatusProcessed.
func (g *GracefulDegradation) Evaluate(ctx context.Context, event []byte) BufferStatus {
	if g.isHealthy(ctx) {
		g.mu.Lock()
		recovering := !g.isAvailable
		g.isAvailable = true
		g.mu.Unlock()

		if recovering {
			log.Printf("Database connection restored, flushing %d buffered events", g.buffer.Size())
			g.triggerAsyncFlush()
		}
		return StatusProcessed
	}

	g.mu.Lock()
	g.isAvailable = false
	g.mu.Unlock()

	// The database is down. Do NOT take custody of the event.
	//
	// Buffering here and letting the caller ACK looked safe — the event *was* held somewhere — but
	// the buffer is process memory, so ACKing removes the only DURABLE copy (JetStream) and a crash
	// or redeploy during the outage destroys the event outright. Measured: 3 events buffered+ACKed,
	// processor SIGKILLed, everything restarted -> 0 rows, no redelivery, no DLQ entry. The code
	// this replaced argued NATS was "the actual durability backstop" while ACKing the message out of
	// NATS, which cannot both be true.
	//
	// Buffering AND returning an error is not an option either: the event would be replayed once by
	// the flush and once by redelivery, and issues.count is an ON CONFLICT increment, so duplicates
	// are visible in the product.
	//
	// So the buffer is no longer a durability mechanism. D10's bounded retry with backoff owns
	// recovery, and a DLQ entry preserves anything that outlasts the retry budget. That is a real
	// durability boundary rather than a memory-only one. See docs/memory/BUGS.md B1, which records
	// the original buffer as a mitigation that never worked.
	log.Printf("WARNING: Database unavailable, returning event to NATS for bounded retry (D10)")
	return StatusUnavailable
}

// triggerAsyncFlush replays the buffer on its own goroutine, decoupled from
// whatever event's call stack detected the recovery. This is deliberate:
// Evaluate can be called from inside ProcessEvent/processEventInternal, and
// the old code called degradation.Flush directly from the end of
// processEventInternal — a re-entrant call back into itself
// (VERIFIED_STATE.md S9). Running the flush on a separate goroutine, with its
// own context, breaks that recursion structurally rather than relying on
// callers to avoid it.
func (g *GracefulDegradation) triggerAsyncFlush() {
	g.flushMu.RLock()
	fn := g.flushFn
	g.flushMu.RUnlock()
	if fn == nil {
		return
	}
	go g.Flush(context.Background(), fn)
}

// CheckAndBuffer is retained for callers/tests that only need a bool. It is
// equivalent to Evaluate(ctx, event) != StatusDropped: true when the event
// was either processed live or safely buffered, false only when it was
// dropped. Production code should prefer Evaluate, which cannot be misread
// the way this bool historically was (VERIFIED_STATE.md S9) — new call sites
// should not be added against this method.
func (g *GracefulDegradation) CheckAndBuffer(ctx context.Context, event []byte) bool {
	return g.Evaluate(ctx, event) != StatusDropped
}

func (g *GracefulDegradation) Flush(ctx context.Context, processor func([]byte) error) int {
	if !g.IsAvailable() {
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
