# Tasks: Sentinel Error Service

## Phase 1: Shared Foundation & Infrastructure

### Infrastructure Setup
- [ ] **TASK-INF-001**: Setup Docker Compose for PostgreSQL 15 and NATS JetStream.
- [ ] **TASK-INF-002**: Configure NATS JetStream stream and consumer for `error_events`.
- [ ] **TASK-INF-003**: Initialize PostgreSQL database with `projects`, `issues`, `error_occurrences`, and `error_search_index` tables.

### Shared Packages
- [ ] **TASK-PKG-001**: Define `ErrorEvent` Protobuf contract in `packages/proto`.
- [ ] **TASK-PKG-002**: Implement shared Go database connection and migration utility in `packages/shared-go`.
- [ ] **TASK-PKG-003**: Implement shared Go NATS publisher and subscriber wrappers in `packages/shared-go`.

## Phase 2: Ingestion Pipeline (ingestor-go)

### Core Ingestion
- [ ] **TASK-ING-001**: Implement HTTP POST endpoint `/ingest` for receiving JSON error payloads.
- [ ] **TASK-ING-002**: Validate incoming payloads against the shared Protobuf contract.
- [ ] **TASK-ING-003**: Implement publisher to NATS JetStream `error_events` subject.

### Ingestion Security
- [ ] **TASK-SEC-001**: Implement API Key authentication for the `/ingest` endpoint (verify against hashed keys in `projects` table).

## Phase 3: Error Processing (processor-go)

### Event Consumption
- [ ] **TASK-PRC-001**: Implement NATS JetStream consumer for `error_events`.
- [ ] **TASK-PRC-002**: Implement de-serialization and basic validation of consumed events.

### Domain Logic (Fingerprinting & Masking)
- [ ] **TASK-PRC-003**: Implement fingerprinting logic (Hash of Class + top 3 app frames) with custom fingerprint support.
- [ ] **TASK-PRC-004**: Implement normalization/scrubbing for dynamic noise in error messages and stack traces.
- [ ] **TASK-PRC-005**: Implement centralized PII and secret masking using regex patterns.

### Alerting & Notifications (FR-005)
- [ ] **TASK-PRC-008**: Implement alerting dispatcher logic in `processor-go` (unique error and frequency threshold detection).
- [ ] **TASK-PRC-009**: Implement Email notification worker.
- [ ] **TASK-PRC-010**: Implement Telegram notification worker.

### Persistence & Indexing
- [ ] **TASK-PRC-006**: Implement de-duplication logic (upsert into `issues` and insert into `error_occurrences`).
- [ ] **TASK-PRC-007**: Implement specialized indexing in `error_search_index` table (extract common metadata fields from occurrence).

## Phase 4: Dashboard & Analytics (dashboard-web)

### Authentication & Authorization
- [ ] **TASK-DSH-001**: Setup SvelteKit with Google Workspace OIDC authentication.
- [ ] **TASK-SEC-003**: Implement Project-level RBAC for dashboard access.

### Features
- [ ] **TASK-DSH-002**: Implement Issue List view with status filtering and project selection.
- [ ] **TASK-DSH-003**: Implement Issue Detail view showing occurrence history and stack trace context.
- [ ] **TASK-DSH-004**: Implement advanced search using the `error_search_index` table (search by `user_id`, `trace_id`, etc.).
- [ ] **TASK-DSH-005**: Implement full-text search across issue messages and stack traces.

## Phase 5: Verification & Hardening

### Testing
- [ ] **TASK-TST-001**: Implement integration test for the end-to-end flow: Ingestor -> NATS -> Processor -> Database.
- [ ] **TASK-TST-002**: Implement unit tests for fingerprinting and masking regex patterns.

### Security Hardening
- [ ] **TASK-SEC-002**: Implement NATS NKEYs authentication and enable TLS for all worker connections.
