# Implementation Plan: Go Client SDK & Ingestor Batch Protocol

**Feature Branch**: `feature/go-client-sdk`  
**Created**: 2026-07-25  
**Status**: Governed Plan Phase  
**Specification**: [specs/007-go-client-sdk/spec.md](file:///home/fitrapujo/oss/sentinel/specs/007-go-client-sdk/spec.md)  
**Memory Context**: [specs/007-go-client-sdk/memory-synthesis.md](file:///home/fitrapujo/oss/sentinel/specs/007-go-client-sdk/memory-synthesis.md)  
**Specification Protocol**: [docs/sdk-specification.md](file:///home/fitrapujo/oss/sentinel/docs/sdk-specification.md)

---

## Executive Summary

This feature delivers the official, zero-dependency **Sentinel Go Client SDK** (`packages/sdk-go`) and extends **`apps/ingestor-go`** with high-throughput batch ingestion (`POST /ingest/batch`). 

The SDK provides:
1. Non-blocking asynchronous error capture (< 50 µs latency) powered by a buffered Go channel (`chan *Event`) and bounded ring buffer with FIFO eviction.
2. Context-aware OpenTelemetry tracing extraction (`trace_id`, `span_id`) and immutable context tag helpers (`sentinel.WithUser`, `sentinel.WithTenant`, `sentinel.WithTag`).
3. Automated client-side PII scrubbing (`password`, `authorization`, `secret`, `credit_card`, `ssn` replaced with `"[FILTERED]"`).
4. Zero-code-change logging adapters for Go standard `slog` (`sentinelslog`), `zerolog` (`sentinelzerolog`), and `log.Logger` (`sentinellog`).
5. Batch HTTP worker (`POST /ingest/batch`) with connection pooling (`http.Transport`) reducing HTTP network overhead by 90% during high-concurrency spikes.

---

## Technical Architecture & Detailed Package Layout

```
packages/sdk-go/
├── client.go               # Core Client struct, Init(), CaptureErrorContext(), Flush()
├── config.go               # Config struct & defaults (ProjectKey, Endpoint, Environment, BatchSize, BatchWait)
├── event.go                # Event struct & stacktrace extractor using runtime.Callers
├── pii.go                  # Client-side PII scrubber (metadata filtering)
├── context.go              # OpenTelemetry trace extraction & context tag helpers (WithUser, WithTenant, WithTag)
├── transport.go            # Non-blocking ring buffer worker & HTTP batch worker (POST /ingest/batch)
├── middleware.go           # Standard net/http Panic Recovery Middleware
├── sentinelslog/           # Go standard log/slog Handler adapter
│   └── handler.go
├── sentinelzerolog/        # zerolog Hook adapter
│   └── hook.go
└── sentinellog/            # standard log.Logger io.Writer adapter
    └── writer.go
```

---

## Module Boundaries & Detailed Data Shapes

### 1. `packages/sdk-go/config.go`
```go
package sentinel

import "time"

type Config struct {
	ProjectKey     string        // Required: API Key
	Endpoint       string        // Required: Ingestor endpoint URL (default: "http://localhost:8080/ingest")
	Environment    string        // Required: e.g. "production", "development"
	ReleaseVersion string        // Optional: Software version tag e.g. "v1.2.0"
	SampleRate     float64       // Optional: 0.0 to 1.0 (default 1.0)
	MaxBufferSize  int           // Optional: Ring buffer capacity (default 100)
	BatchSize      int           // Optional: Max events per HTTP batch (default 10)
	BatchWait      time.Duration // Optional: Max duration before flushing batch (default 1s)
	MaxBreadcrumbs int           // Optional: Max diagnostic breadcrumbs (default 50)
	Debug          bool          // Optional: Enable internal stderr debug logging
}
```

### 2. `packages/sdk-go/context.go`
- **OpenTelemetry Span Extraction**: Inspect `ctx` using `go.opentelemetry.io/otel/trace` span context if present; extract `trace_id` (Hex string) and `span_id` (Hex string). Fall back gracefully to empty strings.
- **Context Tags**:
  ```go
  func WithUser(ctx context.Context, userID string) context.Context
  func WithTenant(ctx context.Context, tenantID string) context.Context
  func WithTag(ctx context.Context, key string, value interface{}) context.Context
  func WithContextMap(ctx context.Context, tags map[string]interface{}) context.Context
  ```

### 3. `packages/sdk-go/transport.go`
- **Buffered Worker Channel**: `eventChan chan *Event` with capacity `MaxBufferSize`.
- **Non-Blocking Push**:
  ```go
  select {
  case t.eventChan <- event:
  default:
      t.evictAndPush(event) // FIFO eviction: drain 1 item from eventChan, increment droppedCount, then push
  }
  ```
- **Batch Worker Loop**: Runs in a background goroutine. Accumulates up to `BatchSize` events or until `BatchWait` ticker fires. Sends HTTP POST request to `<Endpoint>/batch` or `<Endpoint>` with `X-API-Key`.
- **HTTP Connection Pooling**: `http.Client` initialized with `&http.Transport{ MaxIdleConnsPerHost: 100, IdleConnTimeout: 90 * time.Second }`.

### 4. `apps/ingestor-go` Batch Endpoint Extension
- **`apps/ingestor-go/main.go`**: Register `http.Handle("/ingest/batch", rateLimiter.Middleware(ingestBatchHandler))`
- **`apps/ingestor-go/main.go:handleBatchIngest`**:
  - Accept `POST /ingest/batch` body containing `[]validation.ErrorPayload`.
  - Validate each payload using `svc.Ingest(ctx, payload)`.
  - Return `HTTP 202 Accepted` (`{"status": "accepted", "ingested": count}`).

---

## Step-by-Step Implementation Sequence

1. **Step 1: Batch Endpoint in Ingestor Service (`apps/ingestor-go`)**:
   - Add `handleBatchIngest` to `apps/ingestor-go/main.go` and register route `/ingest/batch`.
   - Test single vs batch JSON ingestion using cURL / Vitest / Go integration tests.

2. **Step 2: Core SDK Layout & Event Model (`packages/sdk-go`)**:
   - Create `packages/sdk-go/go.mod`.
   - Implement `config.go`, `event.go` (stacktrace extraction via `runtime.Callers`), and `pii.go` (client-side metadata scrubbing).

3. **Step 3: Context Wiring & OpenTelemetry Support (`packages/sdk-go/context.go`)**:
   - Implement `WithUser`, `WithTenant`, `WithTag`, `WithContextMap`.
   - Implement OpenTelemetry span trace extraction with zero mandatory runtime panic fallback.

4. **Step 4: Non-Blocking Transport & Batch Worker (`packages/sdk-go/transport.go`)**:
   - Implement background worker loop with ring buffer FIFO eviction and batch timer/size flush logic.
   - Implement `client.go` with `Init()`, `CaptureErrorContext()`, and `Flush(timeout)`.

5. **Step 5: Middleware & Logger Adapters (`packages/sdk-go`)**:
   - Implement `middleware.go` (`http.Handler` panic recovery).
   - Implement `sentinelslog/handler.go` (`slog.Handler`).
   - Implement `sentinelzerolog/hook.go` (`zerolog.Hook`).
   - Implement `sentinellog/writer.go` (`io.Writer`).

6. **Step 6: Unit & Integration Test Suite**:
   - Add unit tests for all SDK packages (`client_test.go`, `transport_test.go`, `pii_test.go`, `context_test.go`, `adapters_test.go`).
   - Add end-to-end integration test sending events from `sdk-go` -> `ingestor-go` -> `processor-go` -> Postgres.

---

## Verification & Testing Strategy

1. **Unit Tests (`packages/sdk-go`)**:
   - `client_test.go`: Test non-blocking execution latency (< 50 µs), `Init()` auto-initialization fallback.
   - `pii_test.go`: Test sanitization of sensitive metadata keys (`password`, `authorization`, `credit_card` -> `"[FILTERED]"`).
   - `context_test.go`: Test OpenTelemetry span extraction & context tags (`WithUser`, `WithTenant`, `WithTag`).
   - `transport_test.go`: Test ring-buffer FIFO eviction on overflow & batch timer/size flushing.
   - `adapters_test.go`: Test `slog`, `zerolog`, and `io.Writer` log capture.
2. **Ingestion Integration Tests**:
   - Verify `POST /ingest/batch` payload validation against `apps/ingestor-go`.
3. **Integration Test Suite**:
   - Extend `tests/integration/` to verify end-to-end event transmission from `sdk-go` -> `ingestor-go` -> `NATS` -> `processor-go` -> Postgres.
