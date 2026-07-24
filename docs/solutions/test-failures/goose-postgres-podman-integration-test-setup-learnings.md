---
title: Goose Migrations, Postgres Index Safety, and Podman Testcontainers Patterns
date: 2026-07-24
category: test-failures
module: testing
problem_type: test_failure
component: testing_framework
severity: medium
symptoms:
  - "goose.RunContext with baseline command failed during migration"
  - "CREATE INDEX migration failure during repetitive integration test runs on shared DB connection pool"
  - "testcontainers-go failures on Podman local Linux environment"
root_cause: test_isolation
resolution_type: test_fix
tags:
  - goose-v3
  - postgres
  - migrations
  - podman
  - testcontainers
  - go-testing
  - integration-testing
---

# Goose Migrations, Postgres Index Safety, and Podman Testcontainers Patterns

## Problem
When developing and running integration tests for Go services that rely on external infrastructure (PostgreSQL, NATS, Redis, Ingestor, Processor), test environments often suffer from two major problems:
1. **Container Engine Incompatibilities**: Testcontainers default to Docker socket assumptions, causing failures or hung containers on Linux/macOS environments using Podman (e.g. socket location differences, missing Ryuk sidecar support, or missing `TESTCONTAINERS_PODMAN` configuration).
2. **Brittle & Monolithic Test Setup**: Integration test suites frequently either re-create every container for every single test (causing severe performance degradation) or fail when re-running migrations against an existing/shared database instance due to non-idempotent DDL statements (such as `CREATE INDEX` failing on existing index names).

## Symptoms
- Integration tests fail when running under Podman with errors related to container startup, Ryuk sidecar initialization, or `DOCKER_HOST` socket connection issues.
- Re-running migrations or running test suites against pre-existing testcontainers fails with errors like `relation "idx_projects_api_key" already exists` or duplicate key / relation creation errors.
- Test suites are slow because individual test cases spin up full Postgres, Redis, and NATS instances even when only a subset of resources (e.g., PostgreSQL only) is needed.

## What Didn't Work
- Passing `"baseline"` as a command string to `goose.RunContext` in goose v3 failed because goose v3 removed `"baseline"` command string support from `RunContext`.
- Executing standard `CREATE INDEX` SQL statements in migration files without `IF NOT EXISTS` broke re-entrant migration test execution on shared Postgres connection pools.
- Invoking `testcontainers-go` defaults directly on Podman without setting `TESTCONTAINERS_PODMAN=true` or disabling Ryuk failed due to Podman socket path differences.

## Solution
To solve these integration testing challenges, three core patterns were established:

### 1. Podman Testcontainers Configuration (`ConfigureProvider`)
In `tests/integration/testcontainers/provider.go`, provider detection and configuration were centralized:
- `DetectProvider()` inspects `TESTCONTAINER_PROVIDER` or `TESTCONTAINERS_PROVIDER` environment variables to distinguish `podman` from `docker`.
- `ConfigureProvider()` dynamically sets `TESTCONTAINERS_PODMAN=true` when Podman is detected, ensuring `testcontainers-go` correctly routes requests via the Podman socket and respects disabling Ryuk (`TESTCONTAINERS_RYUK_DISABLED=true`).

### 2. Idempotent Goose Migrations & DB Index Safety
To allow safe schema initialization across reused or re-run database test containers:
- Database migration DDL in `packages/db-migrations/migrations/1716508800_init.sql` uses `CREATE TABLE IF NOT EXISTS` and explicit `CREATE INDEX IF NOT EXISTS` statements for all index definitions (e.g., `CREATE INDEX IF NOT EXISTS idx_projects_api_key ON projects(api_key);`).
- Migration execution in `packages/db-migrations/goose.go` relies on `goose.UpToContext(ctx, db, opts.Directory, version)` to ensure migrations can safely run up to a targeted version without failing if tables or indexes already exist.

### 3. Granular Test Setup Pattern (`testcontainers.Setup`)
In `tests/integration/testcontainers/setup.go`:
- **Bitmask Resource Selection**: The `ResourceFlag` type (`PostgresResource`, `NATSResource`, `RedisResource`, `IngestorResource`, `ProcessorResource`, `AllResources`) allows tests to request only the specific infrastructure components they require via `WithResources(flags)`.
- **Auto-Reuse & Environment Sharing**: `Setup()` checks for pre-existing environment variables (`POSTGRES_HOST`, `NATS_URL`, `REDIS_ADDR`). If set (e.g., in `TestMain` or external docker-compose), `Setup()` attaches to the existing infrastructure instead of launching duplicate containers unless `FORCE_TESTCONTAINERS` is explicitly set.
- **Automated Lifecycle Management**: Container cleanup routines and pool closures are registered using Go's `t.Cleanup()`, guaranteeing teardown at test conclusion regardless of test success or failure.
- **Configurable Options**: `WithMigrations(bool)` and `WithTimeout(duration)` allow fine-grained control over migration application and container startup timeouts.

## Why This Works
- **Fast Execution via Environment Reuse**: Reusing running test containers across test functions reduces test suite execution time from minutes to seconds.
- **Seamless Cross-Engine Support**: Developers running Podman on Linux/macOS get identical integration test execution as Docker users without custom local scripts.
- **Schema Resilience**: Idempotent SQL index creations and contextual Goose migration runs eliminate transient schema setup failures during parallel or repeated test execution.

## Prevention
- **When writing new SQL migrations**: Always ensure table and index DDL commands include `IF NOT EXISTS` constructs so migrations can be safely re-applied in local or integration environments.
- **When adding new integration tests**: Use `testcontainers.Setup(t, testcontainers.WithResources(...))` instead of manually invoking container start scripts or standard `TestMain` boilerplate.
- **When setting up CI/CD pipelines**: Export `TESTCONTAINER_PROVIDER=podman` or `TESTCONTAINERS_PROVIDER=podman` in environments using Podman to automatically trigger optimal container provider options.

## Code Examples

### Granular Setup with Bitmask and Options (`tests/integration/testcontainers/setup.go`)
```go
func TestIssueRepository(t *testing.T) {
    // Spin up only Postgres with migrations applied and custom startup timeout
    env := testcontainers.Setup(t,
        testcontainers.WithResources(testcontainers.PostgresResource),
        testcontainers.WithMigrations(true),
        testcontainers.WithTimeout(2*time.Minute),
    )

    // env.PGConfig & env.PGPool are fully provisioned and cleaned up via t.Cleanup()
}
```

### Podman Provider Auto-Configuration (`tests/integration/testcontainers/provider.go`)
```go
func ConfigureProvider() testcontainers.ProviderType {
    if DetectProvider() == testcontainers.ProviderPodman {
        os.Setenv("TESTCONTAINERS_PODMAN", "true")
        return testcontainers.ProviderPodman
    } else {
        os.Unsetenv("TESTCONTAINERS_PODMAN")
        return testcontainers.ProviderDocker
    }
}
```

### Safe Goose Migration Execution (`packages/db-migrations/goose.go`)
```go
func BaselineVersion(ctx context.Context, db *sql.DB, version int64, opts MigrationOptions) error {
    if opts.TableName == "" {
        opts.TableName = "schema_migrations"
    }

    goose.SetTableName(opts.TableName)

    if err := goose.UpToContext(ctx, db, opts.Directory, version); err != nil {
        return fmt.Errorf("baseline failed: %w", err)
    }

    return nil
}
```
