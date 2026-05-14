# Tasks: Sentinel Error Service

**Input**: Design documents from `/specs/001-sentinel-error-service/`
**Prerequisites**: plan.md (required), spec.md (required for user stories), research.md, data-model.md, contracts/

**Organization**: Tasks are grouped by user story to enable independent implementation and testing of each story.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Project initialization and basic structure for Go workers and SvelteKit dashboard

- [x] T001 [P] Create project structure per plan.md: `apps/ingestor-go/`, `apps/processor-go/`, `apps/dashboard-web/`, `packages/proto/`, `packages/shared-go/`
- [x] T002 Initialize Go modules for ingestor-go and processor-go with required dependencies (pgx/v5, nats.go, connect-go)
- [x] T003 [P] Initialize SvelteKit project for dashboard-web with TypeScript and Drizzle ORM
- [x] T004 [P] Configure Docker Compose with PostgreSQL 15 and NATS JetStream per `scripts/docker-compose.yml`
- [x] T005 Create `packages/proto/error_event.proto` with ErrorEvent and StackFrame messages
- [x] T006 Generate Go code from proto definitions into `gen/sentinel/v1/`
- [x] T007 Configure shared Go module in `packages/shared-go/` with database and NATS utilities

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Core infrastructure that MUST be complete before ANY user story can be implemented

**⚠️ CRITICAL**: No user story work can begin until this phase is complete

- [x] T008 Setup PostgreSQL schema with `scripts/db/init.sql`: projects, issues, error_occurrences, error_search_index, alert_configs tables
- [x] T009 [P] Implement shared Go database connection in `packages/shared-go/database/database.go` using pgx/v5/pgxpool
- [x] T010 [P] Implement shared Go NATS publisher/subscriber wrappers in `packages/shared-go/nats/nats.go`
- [x] T011 Configure NATS JetStream stream `ERROR_EVENTS` and consumer `processor-consumer` via `scripts/nats-init.sh`
- [x] T012 [P] Setup SvelteKit authentication with Google Workspace OIDC in `apps/dashboard-web/src/lib/auth.ts`
- [x] T013 Implement RBAC system in `apps/dashboard-web/src/lib/rbac.ts` with admin/developer/viewer roles
- [x] T014 Create database schema types in `apps/dashboard-web/src/lib/db/schema.ts` using Drizzle ORM

**Checkpoint**: Foundation ready - user story implementation can now begin in parallel

---

## Phase 3: User Story 1 - Grouped Issue Management (Priority: P1) 🎯 MVP

**Goal**: Ingest error events, de-duplicate into Issues, and display in dashboard with count tracking

**Independent Test**: Send 10 identical error payloads to `/ingest` endpoint and verify dashboard shows 1 Issue with count=10

### Implementation for User Story 1

- [x] T015 [P] [US1] Implement HTTP POST `/ingest` endpoint in `apps/ingestor-go/main.go` for JSON error payloads
- [x] T016 [P] [US1] Implement API Key authentication middleware in `apps/ingestor-go/auth.go` (verify against hashed keys in projects table)
- [x] T017 [US1] Implement NATS JetStream publisher in `apps/ingestor-go/publisher.go` to publish to `error_events` subject
- [x] T018 [US1] Implement rate limiting (5000 req/min per API key) in `apps/ingestor-go/ratelimit.go` with HTTP 429 and Retry-After header
- [x] T019 [US1] Implement payload validation in `apps/ingestor-go/validator.go`: reject stacktraces >100 frames, metadata >64KB, messages >10000 chars
- [x] T020 [P] [US1] Implement fingerprinting logic in `apps/processor-go/fingerprint.go`: SHA256 of error_class + top 3 app frames, custom override support
- [x] T021 [P] [US1] Implement normalization in `apps/processor-go/normalizer.go`: UUIDs, numeric IDs, hex addresses, emails, version strings, user paths
- [x] T022 [US1] Implement PII/secret masking in `apps/processor-go/masker.go`: regex patterns for SSN, passport, API keys, passwords, tokens, sensitive metadata keys
- [x] T023 [US1] Implement NATS JetStream consumer in `apps/processor-go/consumer.go` using PullSubscribe
- [x] T024 [US1] Implement de-duplication logic in `apps/processor-go/deduplicate.go`: upsert into issues, insert into error_occurrences, first-writer-wins with ON CONFLICT DO UPDATE
- [x] T025 [US1] Implement search indexer in `apps/processor-go/indexer.go`: extract user_id, tenant_id, trace_id, span_id, request_id into error_search_index
- [x] T026 [P] [US1] Implement Issue List view in `apps/dashboard-web/src/routes/+page.svelte` with card layout (error_class, message snippet, count, last_seen, NEW badge)
- [x] T027 [US1] Implement server load function in `apps/dashboard-web/src/routes/+page.server.ts` with status and project filters
- [x] T028 [US1] Apply RBAC authorization checks in dashboard server load functions

**Checkpoint**: At this point, User Story 1 should be fully functional and testable independently

---

## Phase 4: User Story 2 - Real-time Alerting (Priority: P2)

**Goal**: Alert developers when new unique errors occur or frequency thresholds are exceeded

**Independent Test**: Trigger a new error and verify alert is received via configured channel within 30 seconds

### Implementation for User Story 2

- [x] T029 [P] [US2] Implement alert dispatcher in `apps/processor-go/alerter.go`: detect new unique errors and frequency threshold crossings
- [x] T030 [P] [US2] Implement Email notification worker in `apps/processor-go/email.go` with SMTP and retry queue (exponential backoff: 1s, 5s, 30s, max 3 attempts)
- [x] T031 [US2] Implement Telegram notification worker in `apps/processor-go/telegram.go` with retry queue
- [x] T032 [US2] Create alert_configs table operations and frequency threshold checking
- [x] T033 [P] [US2] Create alert configuration UI in `apps/dashboard-web/src/routes/settings/alerts/+page.svelte` for configuring email/telegram channels
- [x] T034 [US2] Implement alert config server endpoints in `apps/dashboard-web/src/routes/api/alerts/+server.ts`

**Checkpoint**: At this point, User Stories 1 AND 2 should both work independently

---

## Phase 5: User Story 3 - Root-Cause Investigation (Priority: P3)

**Goal**: View full stack trace and local variable context for error occurrences

**Independent Test**: Navigate to an Issue in dashboard and verify Context section displays correct stack trace and local variables

### Implementation for User Story 3

- [x] T035 [P] [US3] Implement Issue Detail view in `apps/dashboard-web/src/routes/issues/[id]/+page.svelte` with occurrence history and stack trace context
- [x] T036 [US3] Implement collapsible metadata panel in issue detail page
- [x] T037 [US3] Implement occurrence timeline in issue detail page
- [x] T038 [US1] Implement advanced search page in `apps/dashboard-web/src/routes/search/+page.svelte` using error_search_index table
- [x] T039 [US1] Implement full-text search using PostgreSQL `to_tsvector`/`to_tsquery` in `apps/dashboard-web/src/lib/server/search.ts`

**Checkpoint**: All user stories should now be independently functional

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: Improvements that affect multiple user stories

- [x] T040 [P] Implement graceful degradation in `apps/processor-go/graceful.go`: buffer events when PostgreSQL unavailable, bounded buffer of 10000 events, flush on recovery
- [x] T041 [P] Implement data retention cleanup cron in `apps/dashboard-web/src/lib/server/retention.ts`: delete error_occurrences older than 30 days
- [x] T042 Create cron endpoint in `apps/dashboard-web/src/routes/api/cron/retention/+server.ts` protected by secret token
- [x] T043 [P] Security hardening: implement NATS NKEYs authentication in `apps/processor-go/nats.go`
- [x] T044 Enable TLS for all worker connections to NATS and PostgreSQL
- [x] T045 [P] Create integration test in `tests/integration/test_e2e.go`: ingestor → NATS → processor → database → dashboard
- [x] T046 [P] Create unit tests for fingerprinting, normalization, masking patterns in `tests/unit/`
- [x] T047 [P] Create load test for 1k+ events/second spike with <1% error drop rate verification

---

## Phase 7: Architectural Refactor

**Purpose**: Remediate architecture violations and improve code quality/maintainability

- [x] T048 [P] [US1] Fix fingerprinting algorithm: use `file:function` instead of `file:line` in `apps/processor-go/fingerprint/fingerprint.go`
- [x] T049 [P] [US1] Ensure custom fingerprints are SHA256 hashed in `apps/processor-go/fingerprint/fingerprint.go`
- [x] T050 [US1] Move direct database queries in `apps/processor-go/processor.go` to the store abstraction in `apps/processor-go/store/store.go`
- [x] T051 [P] [US1] Update `packages/proto/error_event.proto` to include the 64KB metadata size limit using Protobuf validation
- [x] T052 [US1] Refactor `apps/ingestor-go` to use a service layer in `apps/ingestor-go/service/` for orchestration logic
- [x] T053 [US1] TASK-REF-001: Implement `IssueStore` interface to decouple Command/Query paths in `processor-go`
- [x] T054 [US1] TASK-REF-002: Create `QueryService` in `dashboard-web` to encapsulate Drizzle ORM queries
- [x] T055 [US1] TASK-REF-003: Refactor global dashboard loaders to utilize the new `QueryService`

---

## Phase 8: Security Remediation

**Purpose**: Address critical security vulnerabilities and harden data protection logic

- [x] TASK-SEC-001 [US3] Fix IDOR in Issue Detail view: verify project ownership before returning issue data in `apps/dashboard-web/src/routes/issues/[id]/+page.server.ts`
- [x] TASK-SEC-002 [US1] Refine PII masking logic: replace broad `strings.Contains` with precise key matching in `apps/processor-go/masker/masker.go` to prevent over-redaction

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies - can start immediately
- **Foundational (Phase 2)**: Depends on Setup completion - BLOCKS all user stories
- **User Stories (Phase 3+)**: All depend on Foundational phase completion
  - User stories can then proceed in parallel (if staffed)
  - Or sequentially in priority order (P1 → P2 → P3)
- **Polish (Phase 6)**: Depends on all desired user stories being complete
- **Refactor (Phase 7)**: Depends on Phase 3 and Phase 1 completion (Proto/Ingestor/Processor foundations)

### User Story Dependencies

- **User Story 1 (P1)**: Can start after Foundational (Phase 2) - No dependencies on other stories
- **User Story 2 (P2)**: Can start after Foundational (Phase 2) - May integrate with US1 but should be independently testable
- **User Story 3 (P3)**: Can start after Foundational (Phase 2) - May integrate with US1/US2 but should be independently testable

### Within Each User Story

- Models before services
- Services before endpoints
- Core implementation before integration
- Story complete before moving to next priority

### Parallel Opportunities

- All Setup tasks marked [P] can run in parallel
- All Foundational tasks marked [P] can run in parallel (within Phase 2)
- Once Foundational phase completes, all user stories can start in parallel (if team capacity allows)
- Models within a story marked [P] can run in parallel
- Different user stories can be worked on in parallel by different team members

---

## Parallel Example: User Story 1

```bash
# Launch all models for User Story 1 together:
Task: "Implement HTTP POST /ingest endpoint in apps/ingestor-go/main.go"
Task: "Implement fingerprinting logic in apps/processor-go/fingerprint.go"
Task: "Implement normalization in apps/processor-go/normalizer.go"

# Launch dashboard components together:
Task: "Implement Issue List view in apps/dashboard-web/src/routes/+page.svelte"
Task: "Implement Issue Detail view in apps/dashboard-web/src/routes/issues/[id]/+page.svelte"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Complete Phase 1: Setup
2. Complete Phase 2: Foundational (CRITICAL - blocks all stories)
3. Complete Phase 3: User Story 1
4. **STOP and VALIDATE**: Test User Story 1 independently
5. Deploy/demo if ready

### Incremental Delivery

1. Complete Setup + Foundational → Foundation ready
2. Add User Story 1 → Test independently → Deploy/Demo (MVP!)
3. Add User Story 2 → Test independently → Deploy/Demo
4. Add User Story 3 → Test independently → Deploy/Demo
5. Each story adds value without breaking previous stories

### Parallel Team Strategy

With multiple developers:

1. Team completes Setup + Foundational together
2. Once Foundational is done:
   - Developer A: User Story 1 (Phase 3)
   - Developer B: User Story 2 (Phase 4)
   - Developer C: User Story 3 (Phase 5)
3. Stories complete and integrate independently

---

## Notes

- [P] tasks = different files, no dependencies
- [Story] label maps task to specific user story for traceability
- Each user story should be independently completable and testable
- Verify tests fail before implementing
- Commit after each task or logical group
- Stop at any checkpoint to validate story independently
- Avoid: vague tasks, same file conflicts, cross-story dependencies that break independence

---

## Requirement Traceability

| Requirement | Task(s) |
|-------------|---------|
| FR-001: REST ingestion endpoint | T015, T016, T018, T019, T051, T052 |
| FR-002: De-duplication into Issues | T020, T024, T048, T049, T050 |
| FR-003: Full stack trace and context | T035, T036, T037 |
| FR-004: Dashboard with filtering/search | T026, T027, T038, T039 |
| FR-005: Email/Telegram notifications | T029, T030, T031, T033, T034 |
| FR-006: Google Workspace auth + RBAC | T012, T013, T028 |
| FR-007: Go/Rails SDK support | T005 (proto contract) |
| FR-008: NATS JetStream messaging | T004, T010, T011, T023 |
| FR-009: Custom fingerprints | T020, T049 |
| FR-010: Message/stacktrace normalization | T021 |
| FR-011: PII/secret masking | T022 |

| Success Criteria | Verification |
|------------------|--------------|
| SC-001: Identical errors grouped | T046 (integration test) |
| SC-002: 5s dashboard visibility | T045 (integration test) |
| SC-003: 2s context load | T045 (integration test) |
| SC-004: 30s alerting latency | T045 (integration test) |
| SC-005: 1k+ events/sec with <1% drop | T047 (load test) |
| SC-006: 1s search response | T039 |