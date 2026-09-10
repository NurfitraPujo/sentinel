# Changelog

All notable changes to `packages/sdk-go` are documented here.

## v0.2.2 — Custom fingerprints

### Added

- **`Event.Fingerprint`** — an optional custom grouping key. Leave it empty (the normal case) and the
  server computes one from the error class and stacktrace; set it and the server stores it verbatim,
  so two events sharing a value are the same issue whatever their stacktraces look like. Maximum 64
  characters, matching `issues.fingerprint VARCHAR(64)`; the ingestor rejects an oversized value with
  a 400 rather than truncating, because a shortened grouping key would silently merge issues that
  were meant to stay apart.

  This needed a server change too: the proto has carried `fingerprint` since the beginning and the
  processor honoured it, but the ingestor never decoded it from JSON — so the capability was
  reachable only by a hypothetical protobuf client, and JSON is the only ingest path there is.

  The proto's companion `fingerprint_override` is deliberately **not** exposed. Its effect is the
  reverse of what the name suggests: it forces the server to recompute and discards whatever
  fingerprint was sent with it, so an SDK field for it would be a switch whose only function is to
  throw away the value beside it.

## v0.2.1 — Rate-limit retries and trace flags

Both fixes come from an audit of this SDK against what the server actually accepts
(`apps/ingestor-go/validation`, `mapping`, `middleware` and the processor).

### Fixed

- **429 responses are retried instead of discarded.** The ingestor answers a rate-limited
  client with `429` and `Retry-After: 60` (`middleware/ratelimit.go`). The send loop treated
  every 4xx as "the payload was rejected, resending cannot succeed" and dropped the batch — so
  a rate-limited client silently threw away telemetry it could have delivered a minute later.
  429 is now retryable, the server's `Retry-After` is honoured (both the delay-seconds and
  HTTP-date forms), and the wait is capped at 60s so a bad header cannot park a batch
  indefinitely. Every other 4xx keeps its existing drop-immediately behaviour.

- **`trace_flags` is now sent.** The server accepts, maps and stores it end to end, but
  `ExtractTraceIDs` read only the ids off the OpenTelemetry `SpanContext` and dropped the flags
  that sit on the same object, so every SDK-originated event arrived with its sampled bit lost.

### Added

- `ExtractTraceContext(ctx)` returns the trace id, span id **and** the W3C trace-flags byte.
  `ExtractTraceIDs` is unchanged and still returns two values — it is exported, so callers
  outside this repo may depend on its signature.

### Note on retries and `Flush`

The retry wait is deliberately not cancelled by the transport's context. `Flush` cancels that
context before draining, so waiting on it would mean **no** batch is retried during a flush —
silently disabling the existing 5xx retries too. The trade-off is that a batch parked on a 60s
`Retry-After` can outlast a short `Flush` timeout, which then returns `false`; that is what
`Flush`'s bool already means.

## v0.2.0 — Wire contract fix (BREAKING)

> [!IMPORTANT]
> This is a **breaking wire-format change**. It is being released as a breaking `v0.2.0` rather than a patch
> because the JSON field names on `Event` changed. In practice **this breaks nothing that was working**,
> because nothing was working: every version of the SDK prior to this one had its payload rejected by the
> ingestor's `platform` requirement alone (HTTP 400, on every single event), and the SDK never inspected the
> HTTP response, so the 100% rejection rate was completely silent. See
> [`docs/memory/VERIFIED_STATE.md`](../../docs/memory/VERIFIED_STATE.md) findings **S4** and **S11**.

### Fixed

- **`error_message` → `message`** on `Event`. The ingestor/proto field is `message`; the old name meant the
  server-side message column was always empty for every SDK-originated event.
- **`context` → `metadata`** on `Event`. The ingestor/proto field is `metadata`; the old name meant every
  user tag and every PII-scrubbed context value was silently dropped before it ever reached the database.
- **Added `platform` to `Event`, always `"go"`.** The ingestor requires this field (`^[a-z0-9]+$`) and
  rejects any request missing it with HTTP 400 — this alone caused the 100% rejection rate.
- **Added `in_app` to `Frame`, and it is now actually populated** (`ExtractStacktrace` / `isInAppFrame`). A
  frame is considered in-app when its source file is not under `GOROOT`, not under a Go module cache
  (`.../pkg/mod/...`), and not under a `vendor/` directory. Previously `Frame` had no `in_app` field at all,
  so the processor's fingerprinting algorithm (which only hashes in-app frames) degenerated to the error
  class alone — every error of a given class in a project collapsed into a single issue.
- **`sendBatch` now inspects the HTTP response.** Previously the status code was discarded entirely
  (`defer resp.Body.Close()` with no check), so a 100% rejection rate produced no error, log line, or metric
  anywhere on the client. Now:
  - 2xx: success.
  - 4xx: the batch is dropped immediately (not retried — resending an unchanged payload cannot change a
    validation outcome). If `Config.Debug` is set, the status and response body are logged.
  - 5xx / network error: retried up to 4 attempts total with capped exponential backoff
    (200ms → 400ms → 800ms, capped at 5s), then dropped.
  - Added `Config.OnError func(error)` — called whenever a batch is ultimately dropped (4xx, or 5xx/network
    error after retries are exhausted), so applications can alert on ingest failure instead of silently
    losing data. Retries and `OnError` always run on the SDK's internal worker goroutine, never on the
    caller's goroutine — capturing an error never blocks application code.

### Not changed

- `Config.ReleaseVersion` → `Event.release_version` was already wired correctly before this release; it is
  called out here only because it is part of the same S4/S5 field-name table in
  [`docs/plans/E2E_RECOVERY_PLAN.md`](../../docs/plans/E2E_RECOVERY_PLAN.md) P2-3.

### Migration

No application code changes are required to adopt this version unless you constructed `sentinel.Event`
literals directly (uncommon — most callers go through `sentinel.CaptureError` / `CaptureErrorContext`, which
build the event internally and are unaffected). If you did construct `Event` literals or depended on the
`error_message` / `context` JSON keys on the wire (e.g. a custom proxy or test fixture), update those field
names to `message` / `metadata`.
