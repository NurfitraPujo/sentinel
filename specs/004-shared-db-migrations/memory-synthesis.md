# Memory Synthesis: Shared Database Migrations

**Created**: 2024-05-24
**Feature**: 004-shared-db-migrations

## Clear Decisions
- **PostgreSQL Focus**: All migrations must be optimized for PostgreSQL features. (From `DECISIONS.md` and `ARCHITECTURE.md`)
- **Shared Packages Pattern**: Following the monorepo structure, migrations should live in a shared package (e.g., `packages/db-migrations`) to be accessible by all Go apps (`apps/processor-go`, `apps/ingestor-go`). (From `architecture_constitution.md`)
- **Versioned Migrations**: Use a versioned migration pattern (timestamped) to support parallel development and avoid collisions. (Aligned with `DECISIONS.md` recency and user requirement).
- **Unified Schema Structure**: Migrations are unified into a single root directory (`packages/db-migrations/migrations/`) rather than partitioned by app target. The schema serves all apps cohesively, preventing duplicate table definitions and versioning fragmentation.

## Conflicts
- **App Isolation vs. Shared Migrations**: `architecture_constitution.md` mandates high isolation between modules, but migrations are a shared concern. The solution is to centralize the *source* in a shared package while allowing apps to *execute* them targeting their specific DB instances.
- **Concurrent Execution**: Sentinel values data persistence. Concurrent migration runs must be strictly prevented to avoid corrupting the `schema_migrations` log or the schema itself.

## Assumptions
- **Taskfile Entrypoint**: `Taskfile.yml` will be the unified interface for developers, wrapping the underlying migration CLI.
- **Protobuf/Contracts**: Schema changes that affect cross-module data flow must eventually be reflected in `packages/proto`.
- **Testcontainers**: Integration tests for this feature will use Testcontainers to spin up ephemeral Postgres instances.

## Relevant Historical Context
- Sentinel prioritizes "Graceful Degradation". The migration system must be robust enough to handle "loud failures" without leaving the database in an inconsistent state (using transactions where possible).
