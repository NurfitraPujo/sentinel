# Implementation Plan: Shared Database Migrations

**Branch**: `004-shared-db-migrations` | **Date**: 2024-05-24 | **Spec**: [specs/004-shared-db-migrations/spec.md](spec.md)
**Input**: Feature specification for a shared, versioned migration system for PostgreSQL with Taskfile integration.

## Summary
Implement a centralized database migration system using `goose` (Go-based tool). The system will live in `packages/db-migrations` and serve all applications in the monorepo (`apps/`). It supports multiple database targets, versioned SQL migrations, and "baselining" for existing databases. Execution is controlled via `Taskfile.yml` with integrated security guardrails.

## Technical Context

**Language/Version**: Go 1.25, TypeScript (SvelteKit)
**Primary Dependencies**: `github.com/pressly/goose/v3`, `github.com/jackc/pgx/v5`, `Taskfile`
**Storage**: PostgreSQL
**Testing**: `testcontainers-go/modules/postgres`
**Target Platform**: Linux/Docker (Monorepo)
**Project Type**: CLI tool & shared package
**Performance Goals**: Migration status/check < 2s.
**Constraints**: Single-run migrations (concurrency lock), loud failures on error.
**Scale/Scope**: Supports N database targets across monorepo apps.

### Security & Governance Context

- **Secret Management**: Database connection strings MUST be passed via environment variables (e.g., `DB_URL_PROCESSOR`). The migration CLI tool MUST sanitize output to ensure credentials are not logged in CI/CD or stdout.
- **Production Guardrails**: Destructive tasks (e.g., `reset`, `baseline`) in `Taskfile.yml` MUST include a confirmation prompt or be gated by an `ENVIRONMENT` check (e.g., block if `ENVIRONMENT=prod`).
- **Static SQL Policy**: To prevent SQL injection, migration files should be static SQL. Dynamic schema logic MUST be implemented using Go-based `goose` migrations with proper parameterization.
- **Least Privilege**: Migrations should ideally be executed by a dedicated DB user with `CREATE/ALTER/DROP` permissions on schema objects but restricted data access where feasible.

## Constitution Check

*GATE: Passed with Remediation.*

- **Modular Monolith**: Centralizing migrations in `packages/db-migrations` respects the monorepo structure and isolation rules.
- **Explicit over Implicit**: SQL-based migrations provide clear, explicit schema contracts.
- **CQRS Lite**: Supports `dashboard-web` as the read layer. The plan now includes a synchronization step with `packages/proto` to ensure the "Source of Truth" is updated whenever the schema changes.
- **Security-Architecture Conflict Resolved**: Addressed the Taskfile credential exposure risk by mandating environment variable usage and CLI output sanitization.

## Project Structure

### Documentation (this feature)

```text
specs/004-shared-db-migrations/
├── spec.md              # Feature specification
├── plan.md              # This implementation plan
├── research.md          # Tool selection and feasibility
├── memory-synthesis.md  # Architectural context synthesis
└── tasks.md             # Implementation tasks (Phase 2)
```

### Source Code (repository root)

```text
packages/db-migrations/
├── cmd/
│   └── migrate/
│       └── main.go       # CLI entrypoint for migrations
├── migrations/           # Centralized SQL migration files (Unified sequence for all targets)
├── driver.go             # pgx-compatible driver setup for goose
└── goose.go              # Shared migration logic wrapper

Taskfile.yml              # Updated with db:{{target}}:{up,down,reset,baseline,status}
```

**Structure Decision**: Monorepo shared package (`packages/`). This allows all Go apps to import the library if needed (e.g., for auto-migration in tests) while providing a unified CLI for manual/Taskfile operations.

## Complexity Tracking

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| Shared Migration Logic | Consistency across disparate apps. | Per-app migrations would lead to drift and duplication of infra logic. |
| Multiple Targets | Support CQRS Lite (Read/Write separation). | Single DB would violate the CQRS Lite boundary mentioned in the Constitution. |
