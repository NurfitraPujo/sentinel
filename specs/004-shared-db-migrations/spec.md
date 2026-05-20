# Feature Specification: Shared Database Migrations

**Feature Branch**: `004-shared-db-migrations`  
**Created**: 2024-05-24  
**Status**: Draft  
**Input**: User description: "create database migrations that supports incremental migration setup. The database migrations is used across apps in ./apps"

## Clarifications

### Session 2024-05-24
- Q: 1. we need to be able support multiple databases migrations, 2. we need to use existing taskfile as entrypoint of running migrations (<db target> {up,down,reset}) . Edge Cases answer: Concurrent Migrations we strictly only support single run migrations, for supporting parallel development we use versioned migration file approach, Partial Failure let user handle it (make sure the error is loud), Version Collision fail the migration error loudly, Out-of-Order Migrations fail the migration (user need to update the migration file version), Rollback Failure let user handle it → A: [User Input Integrated]
- Q: Migration Directory Structure Drift → A: Migrations must be unified into a single root directory (`packages/db-migrations/migrations/`) rather than partitioned by app. The unified schema serves all apps, preventing duplicate table definitions and versioning fragmentation.


## User Scenarios & Testing *(mandatory)*

### User Story 1 - App Onboarding (Priority: P1)

As a developer adding a new service to `./apps`, I want to quickly set up database migrations using the shared infrastructure so that I can manage my database schema consistently with other services.

**Why this priority**: Essential for the "shared across apps" requirement. Without this, the system isn't shared.

**Independent Test**: Create a dummy app in `./apps` and successfully initialize the migration tracking system.

**Acceptance Scenarios**:

1. **Given** a new app directory in `./apps`, **When** the migration setup command is run, **Then** the necessary configuration and initial tracking tables are created in the target database.
2. **Given** an app with no migrations, **When** the migration status is checked, **Then** it reports "No migrations applied" or similar.

---

### User Story 2 - Incremental Migration Execution (Priority: P1)

As a developer, I want to apply migrations one-by-one to an existing database so that I can evolve the schema safely without data loss.

**Why this priority**: Core functionality of any migration system.

**Independent Test**: Apply a sequence of two migrations and verify both schema changes are present in the database.

**Acceptance Scenarios**:

1. **Given** a database at version N, **When** new migrations are added to the shared pool, **Then** the system identifies and applies only the pending migrations to reach version N+M.
2. **Given** a failed migration, **When** the system runs, **Then** it stops execution and reports the failure without attempting subsequent migrations.

---

### User Story 3 - Incremental Setup for Existing Databases (Priority: P2)

As a developer, I want to start managing a database that already has a schema with the migration system so that I don't have to recreate the database to start using migrations.

**Why this priority**: Specifically mentioned as "incremental migration setup" in the requirement.

**Independent Test**: Point the migration system at a database with existing tables and "baseline" it at a specific version.

**Acceptance Scenarios**:

1. **Given** a database with existing tables corresponding to version X, **When** the "incremental setup" or "baseline" command is run for version X, **Then** the system marks all migrations up to X as applied without actually running them.
2. **Given** a baselined database, **When** migration X+1 is added, **Then** the system applies only migration X+1.

---

### User Story 4 - Multi-Database Management via Taskfile (Priority: P1)

As a developer, I want to use the existing `Taskfile.yml` to run migrations for specific database targets so that I have a consistent entrypoint for all database operations.

**Why this priority**: Directly requested to support multiple databases and Taskfile integration.

**Independent Test**: Run `task db:postgres:up` and `task db:other:up` and verify both targets are updated.

**Acceptance Scenarios**:

1. **Given** multiple database targets, **When** I run `task <db_target> up`, **Then** the migrations for that specific target are applied.
2. **Given** a database target, **When** I run `task <db_target> reset`, **Then** the database is returned to its initial state (all migrations down).

---

### Edge Cases

- **Concurrent Migrations**: The system strictly supports only single-run migrations. Parallel development is supported via the versioned migration file approach.
- **Partial Failure**: If a migration fails halfway, the system will error loudly and stop. The user is responsible for manual recovery/cleanup of the partial state.
- **Version Collision**: If two migrations share the same version/timestamp, the system MUST fail the migration and error loudly.
- **Out-of-Order Migrations**: If a migration with a lower version is introduced after a higher version has been applied, the system MUST fail the migration. The user must update the migration version to be higher than the current applied version.
- **Rollback Failure**: If a rollback ('down') migration fails, the system will error loudly and the user must handle the failure manually.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: System MUST provide a centralized migration repository accessible by all apps in `./apps`.
- **FR-002**: System MUST support "baselining" an existing database to a specific migration version to enable incremental adoption.
- **FR-003**: System MUST track applied migrations in a dedicated table within each application's database.
- **FR-004**: System MUST ensure migrations are applied in a deterministic, sequential order based on versioned files.
- **FR-005**: System MUST support multi-tenant or multi-app environments where different apps might be at different migration versions.
- **FR-006**: System MUST support PostgreSQL as the primary database engine.
- **FR-007**: System MUST support multiple database targets within a single application or across apps.
- **FR-008**: System MUST use the existing `Taskfile.yml` as the primary entrypoint for running migrations using the format `<db target> {up,down,reset}`.
- **FR-009**: System MUST provide a CLI tool to manage migrations (up, down, status, baseline), following a versioned migration pattern (e.g., `<timestamp>_name.sql`).
- **FR-010**: System MUST support "baselining" or "version forcing" to allow incremental adoption on existing databases, similar to `goose` or `golang-migrate`.
- **FR-011**: System MUST prevent concurrent migration runs for the same database target.
- **FR-012**: System MUST fail loudly and stop execution on any migration error (apply, rollback, collision, or sequence error).

### Key Entities *(include if feature involves data)*

- **Migration**: Represents a single schema change, containing a unique identifier (version/timestamp), a description, and the SQL/logic to apply (and optionally revert) the change.
- **Migration Log**: A record in the target database tracking which migrations have been applied, when, and by whom.
- **Database Target**: A configuration representing a specific database instance (host, port, name, credentials) that can receive migrations.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A new app in `./apps` can be fully integrated with migrations in under 5 minutes of configuration.
- **SC-002**: 100% of schema changes are applied through the migration system rather than manual SQL scripts.
- **SC-003**: Migration status for any app can be determined in under 2 seconds via the Taskfile or CLI.
- **SC-004**: System successfully "baselines" an existing 50-table database without data loss or unintended schema changes.
- **SC-005**: System successfully manages and isolates migrations for at least 3 distinct database targets via the Taskfile.

## Assumptions

- Apps in `./apps` share a common language or runtime environment that allows them to execute the shared migration logic.
- Migration files will be stored in a shared package (e.g., in `packages/migrations` or similar).
- Developers have sufficient permissions to create/modify tables in the target databases.
- The primary use case is for relational databases (specifically PostgreSQL).
- The `Taskfile.yml` is already present in the project root or relevant app directories.
