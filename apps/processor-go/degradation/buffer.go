// Package degradation gates event processing on database health.
//
// This package used to also buffer events in process memory while the
// database was down (see docs/memory/DECISIONS.md D1's original text and
// docs/memory/BUGS.md B1). That mechanism is gone, deliberately, not
// accidentally:
//
//   - Buffering + ACKing the NATS message lost events on a crash/restart
//     during an outage, because the buffer was only ever in process memory.
//     Measured: 3 events buffered+ACKed, process killed, restarted -> 0 rows,
//     no redelivery, no DLQ entry.
//   - Buffering + NAKing is not a fix either: the event would be replayed
//     once by the buffer's own flush and once by NATS redelivery, and
//     issues.count is an ON CONFLICT increment, so the replay is a visible
//     duplicate in the product. Deduplicating needs a server-side
//     idempotency key — event_id now has one: store.StoreEvent writes it to
//     error_occurrences.event_id and deduplicates on (issue_id, event_id)
//     inside a single transaction with the issue upsert (docs/plans/
//     IDEMPOTENCY_PLAN.md D-b/D-c, closing the S18 count-inflation defect).
//     That fix lives entirely in the store/service write path, not here —
//     this package still holds no event data of its own; see below.
//
// With no third option, durability now belongs entirely to NATS: D10's
// bounded retry with backoff (1s/5s/15s/30s/60s, MaxDeliver 5), then a DLQ
// entry for anything that outlasts the budget. This package's only remaining
// job is telling the caller whether the database is currently reachable.
package degradation

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// healthCacheTTL bounds how long a positive health result is trusted before
// isHealthy/Evaluate re-checks the database directly. Previously every
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
// stale "down" reading is a few extra seconds of unnecessary redelivery,
// which is safe; the cost of a stale "up" reading is a live write attempted
// against a database that is not actually there, which is already handled
// safely by classifyStoreError + the NATS bounded-retry/backoff path (D10) —
// see docs/memory/DECISIONS.md.
const healthCacheTTL = 2 * time.Second

// BufferStatus is the outcome of evaluating one event against the current
// database health. It used to be a three-way (and, before that, an even more
// ambiguous two-way boolean) status distinguishing "processed live" from
// "buffered" from "dropped" — collapsed to two values now that there is no
// buffering path: an event either gets processed live, or the database is
// unavailable and the caller must let NATS redeliver it.
type BufferStatus int

const (
	// StatusProcessed means the database was healthy: the caller must run
	// its normal live-processing path.
	StatusProcessed BufferStatus = iota
	// StatusUnavailable means the database is down. The caller MUST return
	// an error so NATS redelivers the event (D10) — it must NOT be taken
	// into process memory or acknowledged.
	StatusUnavailable
)

func (s BufferStatus) String() string {
	switch s {
	case StatusProcessed:
		return "processed"
	case StatusUnavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

// GracefulDegradation gates event processing on database health. It holds no
// event data of its own — see the package doc comment for why.
type GracefulDegradation struct {
	isAvailable bool
	mu          sync.RWMutex
	dbChecker   func(context.Context) bool

	lastHealthCheck time.Time
}

func NewGracefulDegradation(dbChecker func(context.Context) bool) *GracefulDegradation {
	return &GracefulDegradation{
		isAvailable: true,
		dbChecker:   dbChecker,
	}
}

func (g *GracefulDegradation) IsAvailable() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.isAvailable
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

// Evaluate reports whether the database is currently healthy. Callers must
// branch on both BufferStatus values: processEventInternal must never run
// for an event that did not return StatusProcessed.
func (g *GracefulDegradation) Evaluate(ctx context.Context, event []byte) BufferStatus {
	if g.isHealthy(ctx) {
		g.mu.Lock()
		recovering := !g.isAvailable
		g.isAvailable = true
		g.mu.Unlock()

		if recovering {
			slog.InfoContext(ctx, "Database connection restored")
		}
		return StatusProcessed
	}

	g.mu.Lock()
	g.isAvailable = false
	g.mu.Unlock()

	// The database is down. Do NOT take custody of the event — see the
	// package doc comment for why buffering it here cannot be made safe.
	slog.WarnContext(ctx, "WARNING: Database unavailable, returning event to NATS for bounded retry (D10)")
	return StatusUnavailable
}
