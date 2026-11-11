---
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
execution: code
product_contract_source: ce-plan-bootstrap
created: 2026-07-23
title: "Add test coverage for untested production packages"
---

# Add test coverage for untested production packages

## Summary

Add test coverage to the 13 production packages that currently have zero tests. Most of the project surface (auth, store, ingestor, notifiers, degradation, mapping, etc.) has no safety net, while only `fingerprint`, `masker`, and `normalizer` are at 100%. The plan distributes tests across `tests/unit/` (pure logic) and `tests/integration/` (testcontainers-backed) following the existing pattern.

## Problem Frame

The Sentinel monorepo has 13 production packages with no tests covering them. Earlier coverage work brought `apps/processor-go/{fingerprint,masker,normalizer}` to 100%, but the remaining surface — ingestor auth, ingestor pipeline, processor service, store, alerts, notifiers, the shared-go infra, and the db-migrations CLI — has no tests at all. Without those, regressions ship undetected and the constitution's TDD preference is honored only in pockets.

The work is bounded but cross-cutting: every package gets at least one test file, and the test approach has to be chosen per package (pure unit vs. testcontainers-backed integration vs. real SMTP/HTTP test server).

## Requirements

- **R1**: Every production package that contains public behavior must have at least one test file exercising that behavior.
- **R2**: Pure-logic packages (no I/O) get unit tests in `tests/unit/` matching the existing pattern.
- **R3**: Packages that depend on Postgres or NATS use testcontainers, reusing the existing `tests/integration/testcontainers/` setup.
- **R4**: Notifier workers (email, telegram) use in-process test servers (SMTP catch-all, `httptest.Server`) without external dependencies.
- **R5**: Each unit covers happy path, edge cases, and error paths.
- **R6**: `go test ./...` passes after the work; existing rules stay green (no regressions).
- **R7**: New test files follow repo conventions: `package <name>_test` shorthand where appropriate, `testify/assert` and `testify/require` for assertions, table-driven where inputs vary.

## Scope Boundaries

### In scope

- 13 packages: `auth`, `mapping`, `middleware`, `validation`, `service` (under ingestor); `alerts`, `degradation`, `event`, `indexer`, `notifiers`, `service` (under processor), `store` (under processor); `database`, `nats`, `redis` (under shared-go).
- Reuse of `tests/integration/testcontainers/` and `tests/integration/setup_test.go` infrastructure.
- New test files under `tests/unit/` and `tests/integration/`.

### Out of scope

- Reaching a specific coverage number across the whole repo. The goal is "every package has tests," not a percentage target.
- Fuzz tests, property tests, contract tests, mutation tests.
- Refactoring existing tested code for testability.
- Performance benchmarks.
- The `cmd/migrate` binary's CLI surface (already covered by integration tests of the `dbmigrations` package).

### Deferred to Follow-Up Work

- A CI workflow that gates PRs on coverage numbers (separate workflow work).
- Negative-coverage reporting (`go test -cover -covermode=count`) in CI (separate observability work).
- Refactoring `alerts` dispatcher to make thresholds and windows injectable so tests don't need a real DB (improves testability but is a behavior-preserving change).

## Key Technical Decisions

### KTD1 — Unit tests live in `tests/unit/`, integration tests in `tests/integration/` (session-settled: user-directed — chosen over in-package tests: matches existing pattern for `mapper`-style pure code without requiring `_test` package splits)

The existing test pattern keeps tests in `tests/unit/` and `tests/integration/` rather than colocated `_test.go` files. Continue this convention. Pure-logic packages get `tests/unit/<package>_test.go` files; DB/NATS-bound packages get `tests/integration/<package>_test.go` files.

### KTD2 — Testcontainers for Postgres and NATS, in-process servers for SMTP and HTTP (session-settled: user-directed — chosen over mocks/fakes: testcontainers is already wired in setup_test.go and gives higher fidelity than mocks)

The existing `tests/integration/setup_test.go` already starts Postgres and NATS containers via `testcontainers`. New tests reuse that infrastructure. For email and telegram, use `net/smtp`-compatible test servers and `httptest.Server` respectively — these are network protocols, not stateful data stores, so an in-process server is the right level.

### KTD3 — Test isolation via database schema namespacing (session-settled: user-directed — chosen over per-test databases: matches existing integration test pattern which uses `schema_migrations` table-namespacing instead)

Existing integration tests (`db_migrations_test.go`) use distinct `MigrationOptions.TableName` values to isolate state. New integration tests follow the same approach — use unique `TableName` values where they touch migrations, and clean up rows in `t.Cleanup` for tables tests directly write.

### KTD4 — Mocking strategy with stdlib only (no third-party mocking library) (session-settled: user-directed — chosen over gomock/mockery: project has no existing mocking framework; stdlib interfaces are simple enough to fake inline)

Where tests need to fake a dependency, define a small interface inline and implement a fake struct in the test file. Do not add gomock, mockery, or counterfeiter. The alerts dispatcher is the exception: its time-based behavior is tested with `testing/synctest` (shipped in Go 1.24+; the project is on `go 1.25.0`), so no `Clock` interface or fake clock implementation is needed.

## Risks & Dependencies

- **Risk**: Long test runtime if every test starts a testcontainer. Mitigation: the existing `TestMain` already starts containers once; new tests should reuse the existing pool via `GetTestConfig()` rather than starting their own. Verify by running `go test ./tests/integration/...` after the work.
- **Risk**: Concurrent test runs race on shared tables. Mitigation: each test uses distinct `TableName` values or `t.Cleanup` to delete rows it created.
- **Risk**: Email/telegram tests with retry loops run for >30s if backoff is not reduced. Mitigation: backoff values are already small (1s, 5s, 30s); tests should not exercise the retry path, only the initial send and the queue-full branch.
- **Dependency**: `tests/integration/testcontainers/` (already exists) and `tests/integration/setup_test.go` (already exists) must keep working unchanged.

## System-Wide Impact

- **Developers**: every package now has a regression net. Behavior changes will surface visibly.
- **CI**: `go test ./...` runtime will grow. Expected increase: ~10s for unit tests, ~30s for integration tests (containers already started). Track in CI dashboard.
- **Operations**: no impact. No new env vars, no new dependencies, no new binaries.
- **Documentation**: no impact. The plan does not modify user-facing docs.

## High-Level Technical Design

The test surface splits along three tiers. The split is driven by each package's dependency profile, not by domain.

### Tier 1 — Pure unit tests in `tests/unit/`

These packages have no external dependencies and test as plain functions or pure data transforms. Tests follow the table-driven pattern already in `tests/unit/normalizer_test.go`.

```text
tests/unit/
├── ingestor_validation_test.go    # ValidatePayload, isValidAlphanumeric
├── ingestor_mapping_test.go       # MapPayloadToEvent
├── processor_event_test.go        # event.Normalize, event.validateEvent, structpbToMap
├── processor_degradation_test.go  # EventBuffer, GracefulDegradation (with stub dbChecker)
├── processor_indexer_fields_test.go # ExtractSearchFields (pure)
├── processor_notifier_test.go     # EmailWorker + TelegramWorker (use in-process SMTP + httptest)
├── shared_redis_test.go           # GetWindowKey
└── (existing) fingerprint_test.go, masker_test.go, normalizer_test.go
```

### Tier 2 — Postgres integration tests in `tests/integration/`

These packages touch Postgres. They reuse the container pool from `setup_test.go` and use `t.Cleanup` to truncate tables they write to.

```text
tests/integration/
├── ingestor_auth_test.go    # APIKeyAuthenticator.Middleware (happy path, missing key, invalid key)
├── processor_store_test.go  # pgStore.UpsertIssue, InsertOccurrence, GetProjectByKey, GetIssueIDByFingerprint
├── processor_alerts_test.go # Dispatcher.Dispatch (counter + threshold logic with stubbed cfg table)
├── processor_service_test.go # ProcessorService.ProcessEvent (full pipeline with real DB)
├── shared_database_test.go  # NewConnection, LoadMigrations, RunMigrationsWithPool
└── (existing) db_migrations_test.go, e2e_test.go
```

### Tier 3 — NATS integration tests in `tests/integration/`

Publisher and Subscriber must connect to a real NATS. Reuse the existing NATS container.

```text
tests/integration/
├── shared_nats_test.go      # Publisher.Publish, Subscriber.Subscribe end-to-end
├── ingestor_service_test.go # IngestService.Ingest (validates with protovalidate, publishes to NATS)
└── (existing) setup_test.go
```

## Implementation Units

### U1. Add unit tests for ingestor validation

**Goal**: Cover `ValidatePayload` and `isValidAlphanumeric` with table-driven cases for every validation rule.

**Requirements**: R1, R2, R5, R7

**Dependencies**: none

**Files**:
- `tests/unit/ingestor_validation_test.go` (new)

**Approach**: Exercise every validation rule independently:
- `project_key` required, max length 64
- `platform` required, lowercase alphanumeric
- `environment` required, lowercase alphanumeric
- `message` max length 10,000
- `error_class` required
- `stacktrace` max 100 frames
- `metadata` max 64 KB
- `stacktrace[i].file` max 512 chars when `in_app=true`
- `WriteValidationError` writes status 400 and JSON body

**Test scenarios**:
- Happy path: minimal valid payload returns `Valid=true` with no errors
- Empty `project_key`, `platform`, `environment`, `error_class` each produce a `Valid=false` with a single field error
- `project_key` longer than 64 chars produces a length error
- `platform` and `environment` with uppercase, underscore, hyphen, or non-ASCII chars produce a format error
- `message` of 10,001 chars produces a length error; 10,000 chars passes
- `stacktrace` of 101 frames produces a length error; 100 frames passes
- `metadata` JSON-encoded body > 64 KB produces a length error; < 64 KB passes
- `stacktrace` entry with `in_app=true` and file > 512 chars produces a per-frame error
- `stacktrace` entry with `in_app=false` and file > 512 chars does NOT produce an error
- `isValidAlphanumeric("abc123")` returns true; `isValidAlphanumeric("ABC")` returns false; `isValidAlphanumeric("abc-def")` returns false; `isValidAlphanumeric("")` returns true (empty is vacuously valid)
- `WriteValidationError` over `httptest.ResponseRecorder` writes status 400 and JSON body containing the field errors

**Verification**: `go test ./tests/unit/... -run "TestValidatePayload|TestIsValidAlphanumeric|TestWriteValidationError"` passes; `go test ./tests/unit/... -coverpkg=./apps/ingestor-go/validation` reports 100%.

---

### U2. Add unit tests for ingestor mapping

**Goal**: Cover `MapPayloadToEvent` field-by-field mapping from `ErrorPayload` to `sentinelv1.ErrorEvent`.

**Requirements**: R1, R2, R5, R7

**Dependencies**: none

**Files**:
- `tests/unit/ingestor_mapping_test.go` (new)

**Approach**: Build an `ErrorPayload` with one of every field set, call `MapPayloadToEvent`, assert each field on the proto. Cover the nil-metadata branch and the multi-frame stacktrace branch.

**Test scenarios**:
- All fields set: output proto has every field matching `payload` (string fields, timestamp via `AsTime`, stacktrace length matches)
- Nil metadata: output proto has `Metadata == nil`
- Empty metadata: output proto has `Metadata` with empty fields map
- Multiple stacktrace frames: output proto has matching length and per-frame fields
- Empty stacktrace: output proto has empty slice
- `TraceID` and `SpanID` map to `TraceId` and `SpanId` (note proto field naming)
- `TraceFlags` field round-trips

**Verification**: `go test ./tests/unit/... -run "TestMapPayloadToEvent"` passes; coverage of `apps/ingestor-go/mapping` reaches 100%.

---

### U3. Add unit tests for processor degradation buffer

**Goal**: Cover `EventBuffer` and `GracefulDegradation` with a stubbed `dbChecker`.

**Requirements**: R1, R2, R5, R7

**Dependencies**: none

**Files**:
- `tests/unit/processor_degradation_test.go` (new)

**Approach**: Test `EventBuffer` standalone (push, drain, size, max-size drop). Test `GracefulDegradation` with a closure `dbChecker` that returns true/false.

**Test scenarios**:
- `EventBuffer.Push` succeeds when under capacity; `Size()` returns the count
- `EventBuffer.Push` returns false and drops when at capacity; logs a warning
- `EventBuffer.Drain` returns all events and empties the buffer
- `NewEventBuffer(0)` falls back to `MaxBufferSize`
- `GracefulDegradation` is available by default
- `CheckAndBuffer` with `dbChecker=true` returns true without buffering
- `CheckAndBuffer` with `dbChecker=false` returns true on push success, marks unavailable, buffers the event
- `CheckAndBuffer` transition from unavailable → available logs the buffer-size and clears the unavailable flag
- `Flush` with `IsAvailable=false` returns 0 without draining
- `Flush` with `IsAvailable=true` and empty buffer returns 0
- `Flush` with `IsAvailable=true` and non-empty buffer calls `processor` for each event; partial failure counts failed, successful ones are removed
- `BufferSize()` reflects current buffer occupancy

**Verification**: `go test ./tests/unit/... -run "TestEventBuffer|TestGracefulDegradation"` passes; coverage of `apps/processor-go/degradation` reaches 100%.

---

### U4. Add unit tests for processor event deserialization

**Goal**: Cover `event.Deserialize`, `event.Normalize`, and `event.validateEvent`.

**Requirements**: R1, R2, R5, R7

**Dependencies**: none

**Files**:
- `tests/unit/processor_event_test.go` (new)

**Approach**: Build a `sentinelv1.ErrorEvent` proto, marshal it, call `Deserialize`, assert the resulting `ErrorEvent`. Cover the fingerprint-computed path (no override) and the fingerprint-provided path (override). Cover the normalize-then-mask ordering.

**Test scenarios**:
- `validateEvent` returns nil for a fully populated event
- `validateEvent` returns error if `project_key`, `platform`, `environment`, or `error_class` is empty
- `Deserialize` happy path: marshal a proto, deserialize, all fields match
- `Deserialize` with nil metadata: resulting `ErrorEvent.Metadata == nil`
- `Deserialize` with timestamp: `Timestamp` matches `AsTime()`
- `Deserialize` returns error when proto is malformed bytes
- `Deserialize` returns error when `validateEvent` rejects (e.g., empty `project_key`)
- `Deserialize` computes fingerprint from `ErrorClass` + stacktrace when `Fingerprint` is empty and `FingerprintOverride=false`
- `Deserialize` uses provided `Fingerprint` when `Fingerprint` is empty and `FingerprintOverride=true` — wait, the source code requires `Fingerprint == "" || FingerprintOverride` to recompute. Verify this is the actual contract via the source, then test exactly that contract.
- `Normalize` masks the message (e.g., a message containing `api_key: secret1234567890`)
- `Normalize` normalizes the message (e.g., a path with `/home/user/`)
- `structpbToMap(nil)` returns nil
- `structpbToMap` converts a struct with mixed values to a `map[string]interface{}`

**Verification**: `go test ./tests/unit/... -run "TestDeserialize|TestNormalize|TestValidateEvent|TestStructpbToMap"` passes; coverage of `apps/processor-go/event` reaches 100%.

---

### U5. Add unit tests for processor indexer ExtractSearchFields

**Goal**: Cover `ExtractSearchFields` covering each alias (`user_id`/`userId`/`user`, etc.) and the empty-string handling.

**Requirements**: R1, R2, R5, R7

**Dependencies**: none

**Files**:
- `tests/unit/processor_indexer_fields_test.go` (new)

**Approach**: Note that `IndexOccurrence` requires a DB and is deferred to integration (U12). Only `ExtractSearchFields` is unit-testable.

**Test scenarios**:
- Empty metadata produces an all-empty `SearchIndexEntry`
- `user_id` (snake_case) is picked up
- `userId` (camelCase) is picked up when `user_id` is absent
- `user` is picked up when neither is present
- `user_id` wins over `userId` when both are present
- Same coverage for `tenant_id` / `tenantId` / `organization_id`
- Same coverage for `trace_id` / `traceId` / `trace-id`
- Same coverage for `span_id` / `spanId` / `span-id`
- Same coverage for `request_id` / `requestId` / `request-id`
- Non-string values for those keys are ignored (not coerced)

**Verification**: `go test ./tests/unit/... -run "TestExtractSearchFields"` passes; coverage of `ExtractSearchFields` in `apps/processor-go/indexer` reaches 100% (the `IndexOccurrence` DB path is covered separately in U12).

---

### U6. Add unit tests for notifier workers (email + telegram)

**Goal**: Cover `EmailWorker` and `TelegramWorker` with in-process test servers.

**Requirements**: R1, R2, R4, R5, R7

**Dependencies**: none

**Files**:
- `tests/unit/processor_notifier_test.go` (new)

**Approach**: For email, run a `net/smtp`-compatible server in a goroutine using `net.Listen("tcp", "127.0.0.1:0")` and accepting SMTP commands minimally. For telegram, use `httptest.NewServer` and configure `TelegramWorker.APIBaseURL` to its URL. Inject a tiny `maxRetries` and short `backoffs` to keep tests fast. Use `t.Cleanup` to call `Close()` on each worker.

**Test scenarios**:
- Email: `Send` returns nil when queue has capacity; worker processes and logs success
- Email: `Send` returns "queue is full" error when queue is at capacity
- Email: `sendEmail` constructs a valid RFC822 `From`/`To`/`Subject` message (assert via the test server capturing the bytes)
- Email: `sendWithRetry` returns nil on first success; logs retry attempts on failure (no test for actual retry timing — keep ≤ 200ms total)
- Telegram: `Send` returns nil when queue has capacity
- Telegram: `Send` returns "queue is full" error when queue is at capacity
- Telegram: `sendTelegram` POSTs JSON with `chat_id`, `text`, `parse_mode=HTML` to `<apiBase>/bot<token>/sendMessage`
- Telegram: returns error when the test server returns 500
- Telegram: returns nil when the test server returns 200
- Telegram: returns error when APIBaseURL is unreachable (use a closed port)

**Verification**: `go test ./tests/unit/... -run "TestEmailWorker|TestTelegramWorker"` passes; coverage of `apps/processor-go/notifiers` reaches 100%.

---

### U7. Add unit tests for shared-go redis GetWindowKey

**Goal**: Cover `GetWindowKey` with stable assertions (the function uses `time.Now().UnixNano()` so the bucket is non-deterministic — assert structural properties).

**Requirements**: R1, R2, R5, R7

**Dependencies**: none

**Files**:
- `tests/unit/shared_redis_test.go` (new)

**Approach**: Assert the format `<prefix>:<key>:<int>` and assert that two calls within the same window produce the same bucket (when run quickly). `NewClient` is integration-only.

**Test scenarios**:
- `GetWindowKey("ratelimit", "abc", time.Second)` returns a string with prefix `ratelimit:abc:`
- Two calls within ≤ 100ms with the same inputs return the same bucket
- Two calls with different `prefix` produce different keys
- Two calls with different `key` produce different keys
- Two calls with very different `window` (1ns vs 1h) produce different bucket values

**Verification**: `go test ./tests/unit/... -run "TestGetWindowKey"` passes; coverage of `GetWindowKey` in `packages/shared-go/redis` reaches 100%.

---

### U8. Add integration tests for ingestor auth

**Goal**: Cover `APIKeyAuthenticator.Middleware` happy path, missing key, and invalid key.

**Requirements**: R1, R3, R5, R7

**Dependencies**: requires Postgres (reuses `setup_test.go`)

**Files**:
- `tests/integration/ingestor_auth_test.go` (new)

**Approach**: Seed a `projects` row with a known `api_key_hash` (sha256 of a known key). Build the `pgxpool` from the test config, construct `APIKeyAuthenticator`, wrap a stub `http.Handler`, assert response codes and the `project_key` context value.

**Test scenarios**:
- Missing `X-API-Key` header returns 401
- Wrong `X-API-Key` header returns 401
- Valid `X-API-Key` header returns 200 from the downstream handler and the `project_key` context value matches the seeded `name`
- `validateAPIKey` returns the project name when given a known hash
- `validateAPIKey` returns error when no row matches

**Verification**: `go test ./tests/integration/... -run "TestAPIKeyAuthenticator"` passes; coverage of `apps/ingestor-go/auth` reaches 100%.

---

### U9. Add integration tests for ingestor middleware (rate limiter)

**Goal**: Cover `RateLimiter.Allow` and `Middleware` with a real Redis (or a stub).

**Requirements**: R1, R3, R5, R7

**Dependencies**: requires Redis (add a Redis container to `setup_test.go` or use a stand-alone testcontainer)

**Files**:
- `tests/integration/ingestor_middleware_test.go` (new)
- `tests/integration/testcontainers/redis.go` (new)
- `tests/integration/setup_test.go` (modify to start Redis if not present)

**Approach**: Extend `setup_test.go` to start a Redis container alongside Postgres and NATS. Use `redis.Client` from the test container in tests. The `RateLimiter` increments a per-key counter and sets an expiry on the first hit.

**Test scenarios**:
- `Allow` returns true when count is below the threshold
- `Allow` returns false when count exceeds the threshold
- `Allow` with `nil` client and `strictMode=false` returns true
- `Allow` with `nil` client and `strictMode=true` returns false
- `Allow` with a client that returns an error and `strictMode=false` returns true (fail-open)
- `Allow` with a client that returns an error and `strictMode=true` returns false (fail-closed)
- `Middleware` returns 200 when under threshold
- `Middleware` returns 429 with `Retry-After: 60` when over threshold
- `Middleware` allows the request when `X-API-Key` is empty (does not rate-limit unauthenticated traffic)

**Verification**: `go test ./tests/integration/... -run "TestRateLimiter"` passes; coverage of `apps/ingestor-go/middleware` reaches 100%.

---

### U10. Add integration tests for ingestor service

**Goal**: Cover `IngestService.Ingest` end-to-end with a real NATS publisher.

**Requirements**: R1, R3, R5, R7

**Dependencies**: requires NATS (already started by `setup_test.go`)

**Files**:
- `tests/integration/ingestor_service_test.go` (new)

**Approach**: Build a `nats.Publisher` against the test NATS container, create an `IngestService`, build a valid `ErrorPayload`, call `Ingest`, and assert the message lands on the JetStream subject.

**Test scenarios**:
- Valid payload: published, message arrives on subject `error_events`, decoded proto equals the input
- Invalid payload (missing `project_key`): `Ingest` returns a validation error and does not publish
- Marshalling failure: out of scope (cannot trigger without a custom proto)

**Verification**: `go test ./tests/integration/... -run "TestIngestService"` passes; coverage of `apps/ingestor-go/service` reaches 100%.

---

### U11. Add integration tests for shared-go database

**Goal**: Cover `NewConnection`, `LoadMigrations`, `RunMigrationsWithPool`.

**Requirements**: R1, R3, R5, R7

**Dependencies**: requires Postgres (already started by `setup_test.go`)

**Files**:
- `tests/integration/shared_database_test.go` (new)

**Approach**: Use the test Postgres container. Write a temp directory with two SQL files, call `LoadMigrations`, and assert the concatenated output contains both in alphabetical order. Call `RunMigrationsWithPool` against an in-memory schema and assert the tables exist.

**Test scenarios**:
- `NewConnection` returns a valid pool and `Ping` succeeds
- `NewConnection` with bad host returns an error and does not leak a pool
- `LoadMigrations` returns concatenated SQL in alphabetical order
- `LoadMigrations` returns error when directory does not exist
- `LoadMigrations` ignores non-`.sql` files
- `RunMigrationsWithPool` creates the tables defined in the SQL

**Verification**: `go test ./tests/integration/... -run "TestDatabasePackage"` passes; coverage of `packages/shared-go/database` reaches 100%.

---

### U12. Add integration tests for shared-go nats

**Goal**: Cover `Publisher.Publish` and `Subscriber.Subscribe` end-to-end.

**Requirements**: R1, R3, R5, R7

**Dependencies**: requires NATS (already started by `setup_test.go`)

**Files**:
- `tests/integration/shared_nats_test.go` (new)

**Approach**: Use the existing NATS container. Build a `Publisher`, publish a message, build a `Subscriber` against the same stream, subscribe with a handler that records the message, assert the recorded message matches.

**Test scenarios**:
- `Publisher.Publish` succeeds
- Round-trip: published message is received by a Subscriber with the same stream/subject
- Subscriber handler returning error causes the message to be `Nak`ed (assert via the message redelivery count)
- Subscriber handler returning nil causes the message to be `Ack`ed
- `Subscriber.Stop` halts the goroutine
- `Subscriber.Close` halts and closes the connection
- `buildTLSConfig` is exercised indirectly via the success path (the code path runs on `NewPublisher`).

**Verification**: `go test ./tests/integration/... -run "TestNatsPackage"` passes; coverage of `packages/shared-go/nats` reaches 100%.

---

### U13. Add integration tests for processor store

**Goal**: Cover `pgStore.UpsertIssue`, `InsertOccurrence`, `GetProjectByKey`, `GetIssueIDByFingerprint`, `PersistAuditLog`.

**Requirements**: R1, R3, R5, R7

**Dependencies**: requires Postgres

**Files**:
- `tests/integration/processor_store_test.go` (new)

**Approach**: Reuse the schema from `db_migrations_test.go`. Each test creates a unique project ID, runs its assertions, then `t.Cleanup` deletes the rows.

**Test scenarios**:
- `UpsertIssue` inserts a new issue and a duplicate call increments `count` and updates `last_seen`
- `InsertOccurrence` inserts a new occurrence
- `GetProjectByKey` returns the project ID for a known key
- `GetProjectByKey` returns error for an unknown key
- `GetIssueIDByFingerprint` returns the issue ID for a known project + fingerprint
- `PersistAuditLog` inserts a row; fails to insert returns error and increments `GetAuditPersistFailureCount`

**Verification**: `go test ./tests/integration/... -run "TestStorePackage"` passes; coverage of `apps/processor-go/store` reaches 100%.

---

### U14. Add integration tests for processor alerts dispatcher

**Goal**: Cover `Dispatcher.Dispatch` counter + threshold logic.

**Requirements**: R1, R3, R5, R7

**Dependencies**: requires Postgres

**Files**:
- `tests/integration/processor_alerts_test.go` (new)

**Approach**: Run all dispatcher tests inside `synctest.Run`. Inside the synthetic-time bubble, the `loadConfigs` 5-minute ticker fires when the test advances time, and `time.Now()` returns the synthetic clock. Tests seed the `alert_configs` table directly, call `synctest.Wait()` to let the constructor's goroutine park on the ticker, then call `Dispatch` and advance synthetically. The dispatch path is otherwise memory-only.

**Test scenarios**:
- `Dispatch` with no config loaded does nothing
- `Dispatch` with config disabled does nothing
- `Dispatch` below threshold increments the counter, does not send
- `Dispatch` at threshold sends an alert (verified via `sendAlert` recording — extract via a tiny interface in the test) and clears the counter
- `Dispatch` counter resets after the configured `FrequencyWindow` elapses (advance synthetic time inside the bubble and assert the counter restarts)
- `loadConfigs` ticker fires once after `5 * time.Minute` of synthetic time and reloads the seeded `alert_configs` row (verify via a sentinel row inserted before the test advances time)
- `formatAlertMessage` truncates messages > 100 chars (test as a pure function)

**Note**: `sendAlert` is a method on `*Dispatcher` that always logs. To assert the alert was sent, the test must wrap or replace it. Cleanest approach: introduce a small `alertSender` interface in the test file (`type alertSender interface { sendAlert(ctx context.Context, cfg *alerts.AlertConfig, alert *alerts.Alert) }`), and pass a fake via test-only field on the dispatcher. Since the production type is concrete, expose a tiny test seam: add a package-private `setSender(s alertSender)` to `apps/processor-go/alerts` behind the build tag `//go:build alerts_test_seam` or, simpler, add a non-exported field `senderForTest alertSender` set only in tests. Alternatively, capture the alert via `log.SetOutput` in the test and parse the log line. Pick the seam that requires the least production-code change.

**Verification**: `go test ./tests/integration/... -run "TestAlertsDispatcher"` passes; coverage of `apps/processor-go/alerts` reaches 100%.

---

### U15. Add integration tests for processor service

**Goal**: Cover `ProcessorService.ProcessEvent` happy path, project-not-found, and degradation buffering.

**Requirements**: R1, R3, R5, R7

**Dependencies**: requires Postgres

**Files**:
- `tests/integration/processor_service_test.go` (new)

**Approach**: Build a `ProcessorService` against the test Postgres. Build a synthetic byte payload by serializing a `service.processEventInternal`-eligible event. Assert the resulting DB rows.

**Test scenarios**:
- `ProcessEvent` happy path: project lookup, issue upsert, occurrence insert, audit log writes, indexer call
- `ProcessEvent` with unknown project: returns error, no occurrence inserted
- `ProcessEvent` with DB unavailable (mock the degradation check or take the DB down): event is buffered, no rows inserted
- `VerifyAuditLogTable` succeeds against the seeded schema
- `VerifyAuditLogTable` returns error when the table does not exist

**Verification**: `go test ./tests/integration/... -run "TestProcessorService"` passes; coverage of `apps/processor-go/service` reaches 100%.

---

### U16. Add unit tests for ingestor middleware nil-client branch

**Goal**: Cover the `nil`-client branch of `RateLimiter.Allow` without standing up a Redis container.

**Requirements**: R1, R2, R5, R7

**Dependencies**: none

**Files**: extend `tests/unit/processor_notifier_test.go` (or add `tests/unit/ingestor_middleware_test.go`)

**Approach**: Construct a `RateLimiter` with `nil` client and toggle the `strictMode` package var via `t.Setenv("RATELIMIT_STRICT_MODE", "true")`.

**Test scenarios**:
- `Allow` with `nil` client and `strictMode=false` returns true
- `Allow` with `nil` client and `strictMode=true` returns false

**Verification**: `go test ./tests/unit/... -run "TestRateLimiterNilClient"` passes.

**Note**: This unit-level coverage is independent of the integration tests in U9. Both are needed because the package-level `strictMode` variable is read at package init and the unit test verifies the env-var read path, while U9 verifies the Redis-backed happy path.

## Verification Contract

Run after all units:

```sh
go test ./... -count=1
go test ./... -coverpkg=./apps/ingestor-go/...,./apps/processor-go/...,./packages/shared-go/...,./packages/db-migrations/... -cover
```

Expected outcomes:

- All tests pass.
- Coverage of every production package reaches 100% (or as close as the package structure allows — see per-unit Verification).
- `go vet ./...` reports no issues.
- The existing `tests/unit/{fingerprint,masker,normalizer}` tests remain at 100%.

## Definition of Done

- All 13 production packages have at least one test file.
- `go test ./...` passes.
- Coverage of all tested packages is at 100%.
- No new third-party dependencies added (only `testify`, `pgxpool`, `httptest`, `net/smtp`-compatible fake — all already in the repo).
- No production code modified (only new test files; existing source unchanged).

## Sources & Research

- Existing test files: `tests/unit/{fingerprint,masker,normalizer}_test.go` (the pattern to follow).
- Existing integration setup: `tests/integration/setup_test.go` and `tests/integration/testcontainers/` (the infrastructure to reuse).
- Architecture summary: `docs/memory/ARCHITECTURE.md`.
- Architecture decision: `docs/memory/ARCHITECTURE.md` "Unified Migration Directory Boundary" (relevant for the DB schema tests).
- Memory: `docs/memory/INDEX.md` (paths and search keywords).
- Earlier coverage run: `go test ./tests/unit/... -coverpkg=./apps/processor-go/fingerprint,./apps/processor-go/masker,./apps/processor-go/normalizer -cover` reported 100%.

## Open Questions

- Q1: For the alerts dispatcher, how should the test advance time without sleeping for real seconds? **Resolution**: Use `testing/synctest` (shipped in Go 1.24+; project is on `go 1.25.0`). Run all dispatcher tests inside `synctest.Run` so the 5-minute `loadConfigs` ticker and the `time.Now()`-based counter window both run on synthetic time. No `Clock` interface, no third-party library, no production code change. Tests seed the `alert_configs` table directly. The sendAlert capture uses a tiny test seam (a non-exported `senderForTest` field on `*Dispatcher`) so the test can replace the logger with a recording fake.
- Q2: Should the rate limiter integration test use the existing Redis container or a separate one? **Resolution**: Use a single shared container to minimize cost. Add `tests/integration/testcontainers/redis.go` and start it in `TestMain` if `RATELIMIT_STRICT_MODE` is not set.
- Q3: For the email/telegram tests, is a `WaitGroup` or `channel` the right sync primitive? **Resolution**: Use a buffered channel of received messages; the test reads with a timeout to avoid hanging.
