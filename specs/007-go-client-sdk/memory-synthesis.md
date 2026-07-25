# Memory Synthesis: Go Client SDK

**Feature**: `007-go-client-sdk`  
**Synthesized**: 2026-07-25  
**Source**: `docs/memory/INDEX.md`, `docs/memory/DECISIONS.md`, `docs/memory/ARCHITECTURE.md`

---

## Active Architecture Constraints & Decisions

1. **A1 (Unified Layer Boundaries)**: Reusable client libraries MUST be placed under `packages/` (specifically `packages/sdk-go`), keeping `apps/` strictly reserved for runnable services (`ingestor-go`, `processor-go`, `dashboard-web`).
2. **D1 (In-Memory Ring Buffering & Reliability)**: Client SDKs MUST implement bounded non-blocking in-memory buffering to ensure network partitions or server outages do not cause application data loss or OOM crashes.
3. **D7 (Real-time Ingestion & Protocol Compliance)**: SDK payloads MUST match `packages/proto/error_event.proto` and [`docs/sdk-specification.md`](file:///home/fitrapujo/oss/sentinel/docs/sdk-specification.md) without introducing extra reads or locks on hot ingestion paths.

---

## Conflicts & Risk Avoidance

- **Risk 1 (Caller Mutex Contention)**: Using heavy mutexes in `CaptureError` creates lock contention across concurrent goroutines. *Resolution*: Use Go's native buffered channel (`chan *Event`) with non-blocking `select` operations.
- **Risk 2 (Log Loop Recursion)**: Logging errors inside the SDK's internal transport worker can re-trigger the SDK logger adapter, causing an infinite loop. *Resolution*: Internal SDK transport logging MUST use an isolated internal logger disabled by default.
