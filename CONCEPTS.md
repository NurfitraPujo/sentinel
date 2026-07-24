# Concepts & Vocabulary

Key architectural terms and patterns across the Sentinel repository.

## Testing & Infrastructure

### Testcontainers Setup (`testcontainers.Setup`)
Granular test setup helper in `tests/integration/testcontainers/setup.go` supporting bitmask resource selection (`PostgresResource`, `NATSResource`, `RedisResource`, `IngestorResource`, `ProcessorResource`), Podman auto-detection via `TESTCONTAINER_PROVIDER=podman` / `TESTCONTAINERS_PODMAN=true`, and automated teardown via `t.Cleanup()`.

### Idempotent Goose Migrations
Database schema migrations executed via `goose.UpToContext` in `packages/db-migrations/goose.go` using `CREATE TABLE IF NOT EXISTS` and `CREATE INDEX IF NOT EXISTS` DDL statements to ensure test suite repeatability on shared or reused connection pools.
