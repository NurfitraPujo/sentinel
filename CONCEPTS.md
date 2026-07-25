# Concepts & Vocabulary

Key architectural terms and patterns across the Sentinel repository.

## Testing & Infrastructure

### Testcontainers Setup (`testcontainers.Setup`)
Granular test setup helper in `tests/integration/testcontainers/setup.go` supporting bitmask resource selection (`PostgresResource`, `NATSResource`, `RedisResource`, `IngestorResource`, `ProcessorResource`), Podman auto-detection via `TESTCONTAINER_PROVIDER=podman` / `TESTCONTAINERS_PODMAN=true`, and automated teardown via `t.Cleanup()`.

### Idempotent Goose Migrations
Database schema migrations executed via `goose.UpToContext` in `packages/db-migrations/goose.go` using `CREATE TABLE IF NOT EXISTS` and `CREATE INDEX IF NOT EXISTS` DDL statements to ensure test suite repeatability on shared or reused connection pools.

## Client SDK Architecture

### Sentinel SDK Protocol Specification
Formal specification documented in [`docs/sdk-specification.md`](file:///home/fitrapujo/oss/sentinel/docs/sdk-specification.md) defining client SDK requirements for multi-language implementations. Guarantees non-blocking asynchronous event capture, client-side PII masking (`[FILTERED]`), standardized JSON payload serialization matching `packages/proto/error_event.proto`, and bounded ring-buffer retry loops during network outages.

### SDK Package Directory Convention
All official client SDK implementations reside in the `packages/` directory (e.g. `packages/sdk-go`, `packages/sdk-js`, `packages/sdk-python`) adhering to the monorepo layer boundaries defined in `.specify/memory/architecture_constitution.md`.
