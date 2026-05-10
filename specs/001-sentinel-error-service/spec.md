# Feature Specification: Sentinel Error Service

**Feature Branch**: `001-sentinel-error-service`  
**Created**: 2026-05-09  
**Updated**: 2026-05-10  
**Status**: Draft  
Input: User description: "Currently, debugging errors across our Go APIs, Go Workers, and Rails applications requires manual log searching via grepping or centralized log explorers. This process is reactive, slow, and lacks context (e.g., local variables, full stack traces, and occurrence frequency). Sentinel is a proposed internal service designed to ingest, group, and alert on application errors in real-time. It provides a 'Sentry-like' experience by de-duplicating identical errors into 'Issues' and providing developers with a structured dashboard for root-cause analysis."

## Clarifications

### Session 2026-05-09
- Q: What is the ingestion architecture? → A: Client SDK (Go/Ruby) -> Ingestor-Go (API) -> NATS JetStream -> Processor-Go (Consumer).
- Q: What is the error event schema? → A: JSON supporting project_key, platform, environment, message, error_class, timestamp, trace_id, span_id, stacktrace (file, line, function), and metadata.
- Q: What is the issue fingerprinting logic? → A: Hash of Error Class + first 3 app frames (filename/function); supports regex normalization and custom developer-provided fingerprints.
- Q: How is sensitive data masking handled? → A: Centralized masking in `processor-go` using regex patterns for common sensitive fields.
- Q: What dashboard search/filtering capabilities are needed? → A: Filtering by project, environment, and platform; plus advanced full-text search across all fields.
- Q: What is the storage and throttling strategy? → A: PostgreSQL for all data; use NATS JetStream for backpressure/throttling to prevent database overwhelm.

### Session 2026-05-10 - Requirements Quality Review
- **Fingerprinting Algorithm**: SHA256 hash of `error_class + "|" + joined_app_frames` where `joined_app_frames = strings.Join(top_3_frames, "|")`. Each frame format is `file + ":" + function`. If fewer than 3 app frames exist, use all available frames. Hash output truncated to first 16 hex characters.
- **Normalization Rules**: Apply in order: (1) collapse whitespace to single space, (2) replace UUIDs `[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}` with `<uuid>`, (3) replace numeric IDs `\b\d{10,}\b` with `<id>`, (4) replace hex addresses `0x[a-fA-F0-9]+` with `<addr>`, (5) replace email patterns with `<email>`, (6) strip version strings `v4.2.1`, `v4.1.0`, etc., (7) replace user paths `/Users/\w+/` and `/home/\w+/` with placeholders.
- **Masking Patterns** (PII/Secrets): Applied after normalization. Patterns include: (1) SSN `\b\d{3}-\d{2}-\d{4}\b`, (2)护照号 `\b[A-Z]{1,2}\d{6,8}\b`, (3) name/address JSON fields, (4) API keys `(?i)(api[_-]?key|secret[_-]?key|access[_-]?token)\s*[:=]\s*["'][^"']+["']`, (5) passwords, (6) bearer tokens, (7) generic tokens. Sensitive metadata keys: `password`, `token`, `secret`, `api_key`, `apikey`, `credential`, `auth` (case-insensitive match on key name).
- **RBAC Role Hierarchy**:
  | Role | Permissions |
  |------|-------------|
  | admin | Full project access: manage alert configs, view all issues, manage team members |
  | developer | View all issues, configure alerts for own project, view occurrence context |
  | viewer | Read-only access to issues and occurrences for assigned projects |
- **Custom Fingerprint Precedence**: (1) If SDK provides non-empty `fingerprint` field, use SHA256 hash of that value directly. (2) Else compute default fingerprint from error_class + stacktrace. This allows SDK-level override while preserving default behavior.
- **Frequency Threshold**: Default 50 errors per 60 seconds. Both threshold and window are configurable per-project via `alert_configs` table. Minimum threshold: 5 errors. Maximum window: 3600 seconds.
- **Alert Delivery**: When Email/Telegram is unavailable, alerts are queued with exponential backoff (max 3 retries, delays: 1s, 5s, 30s). Failed alerts are logged and can be retried via admin UI.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Grouped Issue Management (Priority: P1)

Developers see errors grouped by type and location so they can focus on systemic issues rather than individual log entries.

**Why this priority**: Core value proposition. Without grouping, Sentinel is just another log viewer.

**Independent Test**: Send 10 identical error payloads to the ingestion endpoint and verify that the dashboard shows 1 "Issue" with an occurrence count of 10.

**Acceptance Scenarios**:

1. **Given** a series of identical errors from a Go API, **When** they are ingested by Sentinel, **Then** they should be aggregated into a single Issue.
2. **Given** an existing Issue, **When** a new occurrence of the same error happens, **Then** the "Last Seen" timestamp and "Count" for that Issue should update in real-time.

---

### User Story 2 - Real-time Alerting (Priority: P2)

Developers are alerted immediately when a new unique error occurs or when an existing error frequency exceeds a threshold.

**Why this priority**: Moves the debugging process from reactive to proactive.

**Independent Test**: Trigger a previously unseen error in a Rails application and verify an alert is received via the configured channel within 30 seconds.
 
**Quantified Latency**: Alert notification delivery < 30 seconds from error ingestion (SC-004).

**Acceptance Scenarios**:

1. **Given** a new unique error signature, **When** ingested, **Then** an alert should be dispatched to the relevant team.
2. **Given** an error frequency threshold (default: 50 errors in 60 seconds, configurable), **When** the threshold is crossed, **Then** a high-priority alert should be triggered.
3. **Given** an alert delivery failure (Email/Telegram unavailable), **When** the first delivery attempt fails, **Then** the system MUST retry with exponential backoff (1s, 5s, 30s) up to 3 attempts.

---

### User Story 3 - Root-Cause Investigation (Priority: P3)

Developers view the stack trace and local variables for a specific error occurrence to debug it effectively without manual log searching.

**Why this priority**: Provides the necessary context to actually fix the bugs identified.

**Independent Test**: Navigate to an Issue in the dashboard and verify that the "Context" section displays the correct stack trace and local variables captured at the time of the error.

**Acceptance Scenarios**:

1. **Given** a captured error with local variable context, **When** viewing the issue details, **Then** the variable values should be readable and structured.
2. **Given** a multi-line stack trace, **When** viewing the issue details, **Then** the trace should be formatted with clickable file paths (if supported by the dashboard).

---

### Edge Cases

- **High-Volume Spikes**: The system MUST leverage NATS JetStream as a backpressure and throttling mechanism to prevent the PostgreSQL database from being overwhelmed during major outages. System handles 1k+ events/second with <1% error drop rate.
- **Payload Size Limits**: 
  - Max stacktrace depth: 100 frames (excess truncated with warning logged)
  - Max metadata size: 64KB (excess dropped with warning logged)
  - Max message length: 10,000 characters (excess truncated)
  - Max file path length: 512 characters per frame
- **Sensitive Data Masking**: PII and secrets are masked using regex patterns in processor-go before storage. Masking is mandatory and irreversible.
- **Empty/Null Field Handling**:
  - Empty message: stored as empty string, indexed for search
  - Missing trace_id: stored as NULL in database
  - Missing span_id: stored as NULL in database
  - Empty stacktrace: stored as empty JSON array `[]`
  - Missing metadata: stored as empty JSON object `{}`
- **First Use (Empty projects table)**: Ingestor returns 401 for unknown project_key. Processor gracefully handles empty projects table by logging warning and skipping alerting.
- **Rate Limiting**: Ingestion endpoint enforces 5000 requests/minute per API key. Exceeded requests return HTTP 429 with `Retry-After` header.
- **De-duplication Conflict**: When identical fingerprints arrive simultaneously (race condition), first-writer-wins at database level using `ON CONFLICT DO UPDATE`. Issue count increments by 1 for losing writer.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: System MUST ingest JSON error payloads via a REST/HTTPS ingestion endpoint (`ingestor-go`).
- **FR-002**: System MUST de-duplicate errors into "Issues" using a signature generated from the stack trace and error message (`processor-go`).
- **FR-003**: System MUST store and display the full stack trace and local variable context for each error occurrence.
- **FR-004**: System MUST provide a web-based dashboard with filtering (project, environment, platform) and advanced full-text search across all error fields.
- **FR-005**: System MUST alert developers via Email and Telegram notifications.
- **FR-006**: System MUST authenticate dashboard users via Google Workspace (OIDC/OAuth2).
- **FR-007**: System MUST support error ingestion from Go (APIs/Workers) and Rails applications using platform-specific SDKs.
- **FR-008**: System MUST use NATS JetStream as the message broker between ingestion and processing layers.
- **FR-009**: System MUST support custom fingerprints provided by developers via the Client SDK to override default grouping logic.
- **FR-010**: System MUST normalize error messages and stack traces (e.g., removing dynamic IDs/addresses) before fingerprinting.
- **FR-011**: System MUST perform centralized masking of sensitive data (PII, secrets) in the processing layer before storage.

### Key Entities *(include if feature involves data)*

- **Error Occurrence**: Represents a single instance of an error.
  - Fields: `project_key`, `platform`, `environment`, `message`, `error_class`, `timestamp`, `trace_id`, `span_id`.
  - `stacktrace`: List of `file`, `line`, `function`.
  - `metadata`: Arbitrary key-value pairs (e.g., `user_id`, `request_id`).
- **Issue**: A grouping of Error Occurrences with the same signature (fingerprint). Tracks count, status (Open/Resolved), and first/last seen.
- **Project/App**: Represents the source application or service (e.g., "Dashboard Web", "Ingestor Go").

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of identical errors (identical fingerprint: error_class + top 3 app frames) are correctly grouped into a single Issue within 5 seconds.
- **SC-002**: New error occurrences are visible in the dashboard within 5 seconds of ingestion (< 5s latency).
- **SC-003**: Developers can view the full context (stack trace + variables) for any occurrence in under 2 seconds (< 2s load time).
- **SC-004**: Alerting latency (from ingestion to notification delivery) is under 30 seconds for P1/P2 issues. Measured from `timestamp` field in event to successful delivery to Email/Telegram API.
- **SC-005**: Ingestion endpoint handles 1k+ events/second with < 1% error drop rate during 30-second spike tests.
- **SC-006**: Dashboard search returns results in under 1 second for queries across 1M+ error occurrences.

### Dashboard Layout Requirements

- **Prominent Error Display**: Issues list shows error_class, message snippet (first 100 chars), count, last_seen in card layout. New issues (count=1) displayed with distinct "NEW" badge.
- **Issue Detail Layout**: Full-width stack trace viewer with line numbers, collapsible metadata panel, occurrence timeline.
- **Responsive Breakpoints**: Mobile (<768px) single column, tablet (768-1024px) 2 columns, desktop (>1024px) 3 columns.

## Assumptions

- **Ingestion Accessibility**: Applications have network access to the Sentinel ingestion endpoint at `http://sentinel-ingestor:8080/ingest`.
- **Internal Only**: The service is intended for internal developer use, so extreme public-facing SEO or accessibility optimization is secondary to utility.
- **Standard Formatting**: Applications will use a standard Sentinel client library (Go SDK or Ruby gem) to send errors in the defined JSON format.
- **Data Retention**: Error details will be retained for 30 days by default. Cleanup enforced via cron job at 02:00 UTC daily.
- **Google Workspace**: Dashboard users authenticate via Google Workspace OIDC. Email domain must be `@company.com` (configurable).
- **SMTP Service**: Email alerts assume SMTP service is available at configurable host:port. Delivery failures are logged but do not block processing.
- **Telegram Bot**: Telegram alerts require valid bot token from `@BotFather`. Chat IDs must be pre-authorized via admin UI.
- **SDK Ownership**: Go SDK (`sentinel-go`) and Ruby gem (`sentinel-ruby`) are owned by the Platform team. Delivery timeline: before Phase 2 testing.
- **Connection Pooling**: PostgreSQL connection pool configured with max 25 connections per worker. NATS connections use single persistent connection per worker.
