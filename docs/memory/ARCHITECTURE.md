# Architecture

Last reviewed: 2024-05-20

## System Overview
Sentinel follows a decoupled, event-driven architecture using NATS as the central message broker. Data flows from source to the Ingestor, through NATS, into the Processor, and finally to PostgreSQL.

## Major Components
- **Ingestor-go**: Handles incoming traffic, authentication, and initial validation. Acts as a producer for NATS.
- **Processor-go**: Consumes events from NATS, performs heavy lifting (masking, normalization, fingerprinting), and stores results in the database.
- **Dashboard-web**: Frontend for visualization and management of ingested events.
- **NATS**: Distributed message broker for service decoupling.
- **PostgreSQL**: Primary data store for processed events and metadata.

## Boundaries
- **Ingestor vs Processor**: Communication is purely asynchronous via NATS. The Ingestor should never wait for processing to complete.
- **Internal vs External**: The Ingestor is the only component exposed to external data sources. The Processor and Database remain in internal network layers.

## Integrations
- **Protobuf**: Shared contracts between all services.
- **Testcontainers**: Used for integration testing with live NATS and Postgres instances.
- **Tailwind CSS**: Standardized styling for the dashboard.

## Risks / Complexity Hotspots
- **NATS Backpressure**: High ingestion rates could lead to NATS buffer overflows if the Processor lags.
- **Database Indexing**: Large event volumes require careful indexing strategies in PostgreSQL to maintain query performance.

## Keep Here
- Stable system boundaries.
- Ownership lines between modules or services (e.g., Ingestor owns auth, Processor owns masking).
- Integration constraints that affect many features.

## Never Store Here
- Step-by-step implementation plans.
- One-off feature details.
- Stale diagrams without current boundaries.

---

### 2026-07-17 - Unified Migration Directory Boundary

**Status**
Active

**Why this is durable**
Database schema is cross-cutting infrastructure used by every Go service in the monorepo. The location and ownership of migration files is an architectural boundary, not an implementation detail — once split per app, duplicate tables and version drift become inevitable.

**Decision**
All PostgreSQL schema definitions live in a single flat directory at `packages/db-migrations/migrations/`. The `packages/db-migrations/cmd/migrate` CLI always reads from this root regardless of target database; multi-database support is achieved exclusively via per-target connection strings (e.g. `DB_URL_PROCESSOR`, `DB_URL_INGESTOR`), never via per-target migration subdirectories.

This boundary prevents:
- Duplicate table creation across apps (e.g. the `events` table).
- Versioning fragmentation where each target owns its own sequence.
- Drift between apps' schema views of the same domain.

**Tradeoffs**
- **Gained**: Single source of truth, deterministic versioning, simpler CLI surface.
- **Made harder**: Schema changes affect every target — requires extra review discipline.
- **Reconsider**: If a target legitimately needs a private schema extension, it must be justified against this boundary.

**Future mistake prevented**
Adding per-app migration subdirectories, dual-versioning tracks, or splitting schema into package-private files.

**Evidence**
- `specs/004-shared-db-migrations/architecture-migration-plan.md`
- `specs/004-shared-db-migrations/plan.md` → Structure Decision
- `specs/004-shared-db-migrations/spec.md` → Clarifications Q2

**Where to look next**
`packages/db-migrations/migrations/` and `packages/db-migrations/cmd/migrate/`.
