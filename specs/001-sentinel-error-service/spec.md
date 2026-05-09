# Feature Specification: Sentinel Error Service

**Feature Branch**: `001-sentinel-error-service`  
**Created**: 2026-05-09  
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

**Acceptance Scenarios**:

1. **Given** a new unique error signature, **When** ingested, **Then** an alert should be dispatched to the relevant team.
2. **Given** an error frequency threshold (e.g., 50 errors in 1 minute), **When** the threshold is crossed, **Then** a high-priority alert should be triggered.

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

- **High-Volume Spikes**: The system MUST leverage NATS JetStream as a backpressure and throttling mechanism to prevent the PostgreSQL database from being overwhelmed during major outages.
- **Payload Size Limits**: What happens if an application attempts to send a massive error payload (e.g., very large local variables or deep recursion stack traces)?
- **Sensitive Data Masking**: How are PII or secrets (like API keys in local variables) handled during ingestion and storage?

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

- **SC-001**: 100% of identical errors (identical stack trace and message) are correctly grouped into a single Issue.
- **SC-002**: New error occurrences are visible in the dashboard within 5 seconds of ingestion.
- **SC-003**: Developers can view the full context (stack trace + variables) for any occurrence in under 2 seconds.
- **SC-004**: Alerting latency (from ingestion to notification) is under 30 seconds for P1/P2 issues.

## Assumptions

- **Ingestion Accessibility**: Applications have network access to the Sentinel ingestion endpoint.
- **Internal Only**: The service is intended for internal developer use, so extreme public-facing SEO or accessibility optimization is secondary to utility.
- **Standard Formatting**: Applications will use a standard Sentinel client library (or compatible format) to send errors.
- **Data Retention**: Error details will be retained for 30 days by default (industry standard).
