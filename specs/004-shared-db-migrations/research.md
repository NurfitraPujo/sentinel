# Research: Shared Database Migrations (004)

## Goal
Establish a robust, versioned, and shared database migration system for Sentinel using PostgreSQL.

## Current State
- **Tooling**: Basic `psql` script execution in `Taskfile.yml`.
- **Database**: PostgreSQL with `pgx/v5` driver in Go apps.
- **Structure**: Monorepo with Go apps in `apps/` and shared packages in `packages/`.
- **Infrastructure**: Managed via `docker-compose.yml`.

## Tool Selection: Goose
- **Why**: 
    - Native Go support (both CLI and library).
    - Supports `.sql` and `.go` migrations.
    - Robust handling of versioned migrations and baselining.
    - Compatible with `pgx`.
    - No external binary required if compiled into a local tool.

## Technical Feasibility
- **Integration**: `goose` can be easily wrapped in a small CLI tool within `packages/db-migrations/cmd`.
- **Concurrency**: `goose` uses a lock mechanism in the database to prevent concurrent runs.
- **Taskfile**: Can easily call the compiled tool or `go run` the command.

## Dashboard-Web (Read Layer) Integration
- Since `dashboard-web` (SvelteKit) acts as the read layer in the CQRS Lite pattern, it must be aware of the schema state.
- The `Taskfile.yml` will remain the source of truth for execution.
- We will ensure `task db:dashboard:up` (or similar) is called before starting the dashboard in CI/CD or production environments.

## Draft Taskfile Pattern
```yaml
db:{{.TARGET}}:up:
  desc: Run migrations for {{.TARGET}}
  cmds:
    - go run packages/db-migrations/cmd/main.go -target {{.TARGET}} up

db:{{.TARGET}}:down:
  desc: Rollback migrations for {{.TARGET}}
  cmds:
    - go run packages/db-migrations/cmd/main.go -target {{.TARGET}} down
```

## Risks
- **Multiple Databases**: The system must handle different connection strings for different apps/targets. This should be managed via environment variables (e.g., `DB_URL_PROCESSOR`, `DB_URL_INGESTOR`).
- **Partial Failures**: Postgres supports transactional DDL for most operations. `goose` uses transactions by default. This addresses the "loud failure" requirement.
