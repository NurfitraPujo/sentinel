# Tasks: Shared Database Migrations

**Input**: Design documents from `/specs/004-shared-db-migrations/`
**Prerequisites**: plan.md (required), spec.md (required for user stories), research.md, memory-synthesis.md, security-constraints.md

**Tests**: Integration tests using Testcontainers are required for this feature.

**Organization**: Tasks are grouped by foundational infrastructure followed by prioritized user stories.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3, US4)

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Project initialization and basic structure

- [ ] T001 Create project structure for `packages/db-migrations/` per implementation plan
- [ ] T002 Initialize Go module in `packages/db-migrations/` and add dependencies (`goose/v3`, `pgx/v5`)
- [ ] T003 Create single unified directory structure: `packages/db-migrations/migrations/` (Do not partition by target)

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Core infrastructure for the migration engine and Taskfile integration

- [x] T004 Implement pgx-compatible driver setup in `packages/db-migrations/driver.go`
- [x] T005 Implement shared migration logic wrapper (Goose integration) in `packages/db-migrations/goose.go`
- [x] T006 [P] Implement CLI tool entrypoint in `packages/db-migrations/cmd/migrate/main.go` with target-based configuration
- [x] T007 Implement log/output sanitization in the CLI tool to prevent connection string leakage (Security Remediation R-002)
- [x] T008 [P] Update `Taskfile.yml` with templated database tasks (`db:{{.TARGET}}:up`, `db:{{.TARGET}}:status`)
- [x] T009 Add production guardrails to `Taskfile.yml` for destructive commands like `reset` (Security Remediation R-001)

**Checkpoint**: Foundation ready - migration engine can now be used for user story implementation

---

## Phase 3: User Story 1 - App Onboarding (Priority: P1) 🎯 MVP

**Goal**: Enable services in `./apps` to initialize their migration tracking.

**Independent Test**: Successfully run `task db:processor:status` on a fresh database.

- [ ] T010 [P] [US1] Create integration test in `tests/integration/db_migrations_test.go` using Testcontainers to verify `status` command
- [x] T011 [US1] Implement target-specific connection string retrieval (env vars) in `packages/db-migrations/cmd/migrate/main.go`
- [x] T012 [US1] Define initial "001_init" SQL migration for Processor target in `packages/db-migrations/migrations/processor/001_init.sql`

**Checkpoint**: User Story 1 functional - basic status and initialization tracking working.

---

## Phase 4: User Story 2 - Incremental Migration Execution (Priority: P1)

**Goal**: Apply schema changes safely and sequentially.

**Independent Test**: Apply two migrations in sequence and verify schema state.

- [ ] T013 [P] [US2] Add integration test case for applying multiple migrations sequentially and handling failures
- [ ] T014 [US2] Implement sequential execution logic in `packages/db-migrations/goose.go` ensuring "loud failure" on error
- [ ] T015 [US2] Add second migration for Processor target and verify `up` command updates schema version

---

## Phase 5: User Story 4 - Multi-Database Management via Taskfile (Priority: P1)

**Goal**: Support multiple targets (Postgres, Dashboard, Ingestor) via unified Taskfile entrypoints.

**Independent Test**: Run migrations for two different targets and verify they are independent.

- [ ] T016 [P] [US4] Add integration test verifying isolation between `processor` and `dashboard` migration targets
- [ ] T017 [US4] Implement logic in CLI tool to use the unified `packages/db-migrations/migrations/` directory regardless of the command flag
- [ ] T018 [US4] Add `db:ingestor:*` and `db:dashboard:*` tasks to `Taskfile.yml`

---

## Phase 6: User Story 3 - Incremental Setup (Priority: P2)

**Goal**: Support "baselining" existing databases.

**Independent Test**: Use the `baseline` command on a DB with existing schema and verify subsequent migrations work.

- [ ] T019 [P] [US3] Add integration test case for the `baseline` / `version` command on an existing schema
- [ ] T020 [US3] Implement `baseline` (version forcing) command in the CLI tool and Goose wrapper

---

## Phase 7: Polish & Governance

**Purpose**: Documentation, synchronization, and final hardening.

- [ ] T021 [P] Update `packages/proto` with any core schema changes identified during migration implementation (Architecture Alignment)
- [ ] T022 Document manual recovery procedures for "Partial Failure" scenarios in `packages/db-migrations/README.md`
- [ ] T023 Final validation run of `Taskfile.yml` commands against a local dev environment
- [ ] T024 [REFACTOR] Fix `migrate down` command logic in `packages/db-migrations/goose.go` to correctly call `goose.RunContext` with "down" instead of hardcoded "up" (P0)
- [ ] T025 [REFACTOR] Unify directory structure by moving all existing migrations into the root `packages/db-migrations/migrations/` and updating the CLI to point there (P1)
- [ ] T026 [REFACTOR] Normalize incremental migrations: Fix `002_add_trace.sql` by removing duplicate `CREATE TABLE` logic and adhering to standard `goose Up/Down` blocks (P1)
- [ ] T027 [REFACTOR] Synchronize Proto Contracts: Add `source` field to `ErrorEvent` in `packages/proto/error_event.proto` (P1)
- [ ] T028 [REFACTOR] Prune dead code (`getEnv`, `getDSNForTarget`) in `main.go` and unused `pgxpool` abstraction in `driver.go` (P2)

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies.
- **Foundational (Phase 2)**: Depends on Setup completion. BLOCKS all user stories.
- **User Story 1 & 4 (P1)**: Can start after Foundational phase.
- **User Story 2 (P1)**: Depends on US1 (initialization).
- **User Story 3 (P2)**: Depends on US1 completion.
- **Polish (Phase 7)**: Depends on all stories being complete.

### Parallel Opportunities

- T006, T008 can run in parallel within Phase 2.
- Integration tests (T010, T013, T016, T019) can be drafted in parallel once the CLI interface is defined.
- Migration files for different targets (US4) can be created in parallel.
