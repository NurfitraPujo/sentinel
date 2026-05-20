# Unified Migration Structure Migration Plan

## Current State
Migrations are partitioned into subdirectories by application target:
- `packages/db-migrations/migrations/processor/`
- `packages/db-migrations/migrations/ingestor/`
- `packages/db-migrations/migrations/dashboard/`

This results in duplicate definitions for the `events` table and fragmented versioning.

### Problems
- **Duplicate Schema Logic**: Both `processor` and `ingestor` migrations attempt to create the `events` table.
- **Versioning Fragmentation**: Each target has its own version sequence, making it impossible to reason about the database state as a whole.
- **Maintenance Overhead**: Developers must manage multiple sets of migration files for what is essentially a unified schema.

## Target State
A single, flat directory at `packages/db-migrations/migrations/` containing all versioned SQL files for the entire system.

### Benefits
- **Single Source of Truth**: One sequence of migrations defines the entire database schema.
- **Deterministic Versioning**: Avoids collisions and ensures all apps are running against the same schema version.
- **Simpler CLI**: The migration tool always looks in the same place.

## Migration Phases

### Phase 1: Consolidation (Estimated: 0.5 days)
**Goal**: Merge all unique migration logic into a single sequence.

- **Task 1.1**: Move unique migrations to `packages/db-migrations/migrations/`.
- **Task 1.2**: De-duplicate the `events` table creation.
- **Task 1.3**: Re-sequence migration numbers (e.g., `001_init_schema.sql`, `002_add_dashboards.sql`, `003_add_trace_to_events.sql`).

**Coexistence**: During this phase, the CLI still points to subdirectories, so this is a "prepare" phase where files are moved but the tool isn't switched yet.

### Phase 2: CLI & Taskfile Update (Estimated: 0.5 days)
**Goal**: Point all tools to the unified directory.

- **Task 2.1**: Update `getMigrationDir` in `main.go` to return the root `migrations/` directory regardless of target.
- **Task 2.2**: Update `Taskfile.yml` to remove any target-specific directory overrides if they exist.

**Coexistence**: The CLI will now apply the unified migrations to whichever `-target` (database DSN) is provided.

### Phase 3: Cleanup (Estimated: 0.1 days)
**Goal**: Remove legacy structure.

- **Task 3.1**: Delete empty `processor/`, `ingestor/`, and `dashboard/` subdirectories.

## Coexistence Strategy

**Why coexistence?** We need to ensure that existing databases can be safely baselined to the new unified versioning.

**How**:
- Use the `baseline` command to mark existing databases as "up to date" with the new unified version `001` or `002` once consolidated.
- For new environments, the unified sequence will run from scratch.

## Rollback Plan
If consolidation causes versioning conflicts in production, we can revert the CLI change (Phase 2) and restore the subdirectory pointers while we re-align the migration sequence.

## Success Criteria
- [ ] `packages/db-migrations/migrations/` is a flat directory.
- [ ] No duplicate table creation logic exists across files.
- [ ] `task db:processor-up` and `task db:dashboard-up` both pull from the same directory.
- [ ] Integration tests pass for all targets.
