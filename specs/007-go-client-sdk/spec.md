# Feature Specification: Go Client SDK & Telemetry Integration Protocol

**Feature ID**: `007-go-client-sdk`  
**Feature Branch**: `feature/go-client-sdk`  
**Created**: 2026-07-25  
**Status**: Completed (MERGED 2026-07-25) — code shipped, but the wire contract it shipped with was
**verified broken** (0% of events accepted) until a follow-up fix on 2026-07-28/29. "Completed" here
tracked the tasks file, not runtime behavior — see `docs/memory/VERIFIED_STATE.md`.  
**Verified**: 2026-07-29 — `docs/memory/VERIFIED_STATE.md` findings **S4** (SDK↔ingestor field
mismatch) and **S11** (fingerprint collapse with no in-app frames), both now RESOLVED. Proof: SDK
payload accepted end-to-end into `error_occurrences`/`issues`; `packages/sdk-go/CHANGELOG.md` v0.2.0.
Below is the corrected contract as of that fix — see "Wire Contract Correction (v0.2.0)".  
**Specification Protocol**: [`docs/sdk-specification.md`](file:///home/fitrapujo/oss/sentinel/docs/sdk-specification.md)

---

## Wire Contract Correction (v0.2.0, 2026-07-28/29)

> [!IMPORTANT]
> Everything below this box documents the SDK **as originally specified and merged**. It is kept for
> history. The actual wire contract changed in `packages/sdk-go` v0.2.0 (BREAKING) because the
> originally-shipped contract was rejected by `ingestor-go` on every single event — see
> `docs/memory/VERIFIED_STATE.md` S4. Where this spec's text below still describes the pre-v0.2.0
> shape, **the code is the source of truth**; treat the spec text as historical intent, not current
> behavior.

Changes made (full detail: `packages/sdk-go/CHANGELOG.md`):

- **`Config.ProjectKey` split into two fields.** The single field this spec's FR-001 describes as "the
  API Key used for authentication" was both the secret AND the tenancy selector, and the ingestor
  never resolved it as a credential — it resolved it against `projects.name`. Every SDK event was
  `202`'d by NATS publish and then permanently dead-lettered at the processor, silently.
  - `Config.APIKey` (new) — the SECRET (`sent_live_...` / `sent_org_...`). Sent ONLY as the
    `X-API-Key` header, never in the body.
  - `Config.ProjectKey` (redefined) — the target project's **unique name** (`projects.name`), an
    identifier, not a secret. Travels in the body as `project_key`. `Config.Validate()` rejects a
    `ProjectKey` that carries an API-key prefix, so a swapped config fails loudly at startup instead
    of as a silent 100% rejection rate.
- **Event field renames** (`Event` struct, `packages/sdk-go/event.go`): `error_message` → `message`;
  `context` → `metadata`. The ingestor never read the old names (S4); every message and every user
  tag/PII-scrubbed context value was silently dropped before this fix.
- **`platform` added to `Event`**, always `"go"`. The ingestor requires it
  (`^[a-z0-9]+$`) and rejected every request missing it with HTTP 400 — this alone caused the
  100% rejection rate documented in S4.
- **`in_app` added to `Frame`, and actually populated** by `isInAppFrame()` (true unless the frame's
  file is under `GOROOT`, a module cache `pkg/mod/`, or `vendor/`). Fixes S11: the processor's
  fingerprinter only hashes in-app frames, so with `in_app` always false (the field didn't exist
  before), every error of a given class in a project collapsed into one issue. The processor also now
  falls back to the top 3 frames when *no* frame is in-app (`apps/processor-go/fingerprint/fingerprint.go`),
  covering any client — not just this SDK — that never sets `in_app`.
- **`sendBatch` now inspects `resp.StatusCode`.** Previously discarded entirely, so the 100% rejection
  rate produced zero client-side signal. Now: 2xx = success; 4xx = dropped immediately and logged under
  `Config.Debug` (not retried — resending an unchanged payload cannot change a validation outcome);
  5xx/network error = retried up to 4 attempts with capped exponential backoff (200ms→400ms→800ms,
  capped 5s), then dropped. `Config.OnError func(error)` fires whenever a batch is ultimately dropped,
  always from the SDK's internal worker goroutine, never the caller's.
- **`Config.ReleaseVersion` → `Event.release_version` was already correct** before this release; called
  out only because it sits in the same S4/S5 field table in `docs/plans/E2E_RECOVERY_PLAN.md` P2-3.

No application code changes are required to adopt v0.2.0 unless callers constructed `sentinel.Event`
literals directly or depended on the `error_message`/`context` JSON keys on the wire.

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
  > [!NOTE]
  > **Superseded by the v0.2.0 wire contract correction above.** As implemented, `Config` also requires
  > a separate `APIKey` field (the secret, header-only); `ProjectKey` is redefined as the project's
  > unique *name*, not a credential. The code (`packages/sdk-go/config.go`) is authoritative for the
  > current field list.
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
