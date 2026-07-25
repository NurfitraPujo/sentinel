# Feature Specification: Go Client SDK & Telemetry Integration Protocol

**Feature ID**: `007-go-client-sdk`  
**Feature Branch**: `feature/go-client-sdk`  
**Created**: 2026-07-25  
**Status**: Completed  
**Specification Protocol**: [`docs/sdk-specification.md`](file:///home/fitrapujo/oss/sentinel/docs/sdk-specification.md)

---

## Executive Summary

The **Go Client SDK** (`packages/sdk-go`) provides an official, zero-dependency client SDK for Go applications to capture runtime errors, unhandled panics, and diagnostic metadata, streaming them asynchronously to `ingestor-go` via single (`POST /ingest`) and batch (`POST /ingest/batch`) ingestion APIs. To ensure seamless telemetry integration, the SDK supports context-aware OpenTelemetry tracing extraction (`trace_id`, `span_id`), built-in context tag helpers (`sentinel.WithTag`, `sentinel.WithUser`, `sentinel.WithTenant`), and logging framework adapters for `slog`, `zerolog`, and `log.Logger`.

---

## User Scenarios & Primary Flows

### Scenario 1: Initializing and Reporting Explicit Errors with Context
- **Actor**: Go Backend Application Developer
- **Flow**:
  1. Developer imports `github.com/NurfitraPujo/sentinel/packages/sdk-go`.
  2. Developer initializes Sentinel with `sentinel.Init(...)`.
  3. During HTTP or gRPC request handling, developer enriches `context.Context` using helpers: `ctx = sentinel.WithUser(ctx, "usr_99")`, `ctx = sentinel.WithTenant(ctx, "org_acme")`, `ctx = sentinel.WithTag(ctx, "request_id", reqID)`.
  4. When an error occurs, developer invokes `sentinel.CaptureErrorContext(ctx, err)`.
  5. The SDK automatically extracts OpenTelemetry `trace_id` and `span_id` from the active span in `ctx`, attaches all context tags to `metadata`, enqueues the event into the non-blocking ring buffer, and returns immediately.

### Scenario 2: Seamless Integration via Existing Loggers (`slog`, `zerolog`, standard `log`)
- **Actor**: Service Developer with Existing Logging Setup
- **Flow**:
  1. Developer configures application logger with Sentinel adapter (e.g., `slog.SetDefault(slog.New(sentinelslog.NewHandler(opts)))` or `zerolog.Logger.With().Hook(sentinelzerolog.NewHook())`).
  2. Throughout application execution, developer calls standard log methods passing `ctx` (e.g. `slog.ErrorContext(ctx, "database timeout", "db_host", host)`).
  3. The Sentinel log adapter automatically extracts OpenTelemetry `trace_id`/`span_id` and context tags from `ctx`, converts log attributes into metadata, and routes them to Sentinel.

### Scenario 3: High-Concurrency Burst Handling & Batch Ingestion
- **Actor**: High-Throughput Microservice (Concurrent Worker Goroutines)
- **Flow**:
  1. Multiple goroutines simultaneously capture errors during a service outage burst.
  2. The SDK uses a non-blocking channel buffer (`chan *Event`) to enqueue events with zero mutex contention overhead on application goroutines.
  3. The background SDK transport worker aggregates pending events into batches (up to `BatchSize: 10` events or `BatchWait: 1 second` timer) and transmits them in a single HTTP request to `POST /ingest/batch`.
  4. `ingestor-go` validates each event in the batch payload, publishes them to NATS JetStream, and returns `HTTP 202 Accepted`.

### Scenario 4: Automatic HTTP Middleware & Panic Recovery
- **Actor**: HTTP Service Developer
- **Flow**:
  1. Developer wraps HTTP router or handlers with `sentinel.HTTPMiddleware()`.
  2. If an HTTP handler panics during request processing, the middleware recovers the panic, extracts trace & request context, attaches HTTP metadata (method, route, status code 500), and dispatches the error event to Sentinel.

### Scenario 5: Network Disruption & Graceful Shutdown
- **Actor**: Infrastructure / DevOps Operator
- **Flow**:
  1. If `ingestor-go` is temporarily unreachable, the SDK stores up to `MaxBufferSize` events in memory and retries using exponential backoff.
  2. Upon receiving application shutdown signals (`SIGTERM`), the application invokes `sentinel.Flush(timeout)`.

---

## Functional Requirements

- **FR-001**: **Configuration**: SDK MUST accept `ProjectKey`, `Endpoint`, `Environment`, `ReleaseVersion`, `SampleRate`, `MaxBufferSize`, `BatchSize`, `BatchWait`, `MaxBreadcrumbs`, and `Debug` options.
- **FR-002**: **Context-Aware Telemetry Extraction**: `sentinel.CaptureErrorContext(ctx, err)` MUST automatically extract W3C OpenTelemetry `trace_id` and `span_id` from the active OpenTelemetry span in `ctx`.
- **FR-003**: **Developer Context Helpers**: SDK MUST provide immutable context wiring helpers:
  - `sentinel.WithUser(ctx, userID)` — Attaches `user_id` to event context.
  - `sentinel.WithTenant(ctx, tenantID)` — Attaches `tenant_id` to event context.
  - `sentinel.WithTag(ctx, key, value)` — Attaches arbitrary service tags (e.g. `request_id`, `correlation_id`) to event metadata.
  - `sentinel.WithContextMap(ctx, map)` — Bulk context tag wiring helper.
- **FR-004**: **Async Non-Blocking High-Throughput Ingestion**: `CaptureError` and `CapturePanic` MUST push events to a buffered Go channel (`chan *Event`) and return immediately (< 50 µs latency).
- **FR-005**: **Batch Ingestion API Support**: `ingestor-go` MUST expose `POST /ingest/batch` accepting JSON array `[ErrorEvent]`. `sdk-go` background worker MUST flush events in batches (default max 10 events or 1s timer).
- **FR-006**: **Logging Framework Integrations**:
  - `slog`: `sentinelslog.NewHandler()` implementing `slog.Handler` with `ctx` trace & context tag extraction.
  - `zerolog`: `sentinelzerolog.NewHook()` implementing `zerolog.Hook`.
  - Standard `log`: `sentinellog.NewWriter()` implementing `io.Writer`.
- **FR-007**: **Bounded Memory & Resource Safety**: Total SDK memory footprint MUST be bounded by `MaxBufferSize` (default 100 events) with FIFO eviction on overflow.
- **FR-008**: **Client-Side PII Scrubbing**: SDK MUST filter metadata keys matching `password`, `token`, `secret`, `authorization`, `credit_card`, and `ssn` replacing values with `"[FILTERED]"`.
- **FR-009**: **HTTP Client Connection Pooling**: Background SDK transport MUST reuse HTTP connections (`http.Transport` with `MaxIdleConnsPerHost >= 100` and TCP keep-alives).
- **FR-010**: **Panic Recovery Middleware**: SDK MUST provide standard `http.Handler` middleware.
- **FR-011**: **Graceful Flush**: SDK MUST provide `Flush(timeout)` to drain pending events during process termination.
- **FR-012**: **Default Auto-Initialization**: If `CaptureError` or middleware is invoked prior to explicit `sentinel.Init()`, the SDK MUST automatically initialize with default configuration.

---

## Edge Cases & Failure Handling

- **Missing OpenTelemetry Span**: If no active OpenTelemetry span exists in `ctx`, `trace_id` and `span_id` fall back gracefully to empty strings or context-extracted request IDs.
- **High-Concurrency Error Spike**: Goroutines pushing errors during a burst experience zero channel blocking.
- **Log Loop Prevention**: Internal SDK transport logging MUST NOT re-trigger the Sentinel log adapter.

---

## Success Criteria

1. **Zero Application Latency Penalty**: `CaptureErrorContext` execution latency is < 50 microseconds.
2. **Automated Telemetry Correlation**: 100% of events captured via `CaptureErrorContext` in traced OpenTelemetry handlers automatically populate `trace_id` and `span_id`.
3. **Seamless Context Tagging**: Service tags (`user_id`, `tenant_id`, `request_id`) wired via `sentinel.WithTag()` are reliably propagated to event `metadata`.
4. **Clean Shutdown**: `Flush()` drains 100% of queued events prior to timeout.

---

## Clarifications

### Session 2026-07-25
- Q: How should the Go SDK behave when `CaptureError` is invoked on an uninitialized client? → A: Option C: Default Auto-Init with fallback configuration (`Endpoint: "http://localhost:8080/ingest"`).
- Q: How should high-concurrency bursts from multiple goroutines be handled? → A: Non-blocking Go buffered channel (`chan *Event`) with non-blocking `select` send, FIFO overflow eviction, and HTTP connection pooling (`http.Transport`).
- Q: Should `ingestor-go` and `sdk-go` support batch ingestion? → A: Option A: Support `POST /ingest/batch` accepting JSON array `[ErrorEvent]` in `ingestor-go` and batch flushing in `sdk-go`.
- Q: How should Sentinel Go SDK integrate with Go logging frameworks? → A: Option C: Provide `slog.Handler`, `zerolog.Hook`, and standard `io.Writer` log adapters in sub-packages (`sentinelslog`, `sentinelzerolog`, `sentinellog`).
- Q: How should the client SDK integrate with telemetry services and service tags? → A: Option A: `sentinel.CaptureErrorContext(ctx, err)` automatically extracts OpenTelemetry `trace_id`/`span_id` from `ctx` and provides context wiring helpers (`sentinel.WithUser`, `sentinel.WithTenant`, `sentinel.WithTag`, `sentinel.WithContextMap`).
