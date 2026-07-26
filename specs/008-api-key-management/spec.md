# Feature Specification: Multi-Tenant Project Auth & API Key Management

**Feature Branch**: `008-api-key-management`  
**Created**: 2026-07-26  
**Status**: Completed  
**Input**: User description: "Multi-Tenant Project Auth & API Key Management - full API key lifecycle (creation, listing, revocation, scope management, SHA256 hashed storage), Redis/In-Memory rate limiting per key/project, and Organization RBAC integration based on docs/todos/02-multi-tenant-auth-and-api-key-management.md"

---

## Clarifications

### Session 2026-07-26
- Q: How should API key validity be cached in `ingestor-go` to balance instant revocation (<100ms) with sub-millisecond database protection? → A: Option A (Redis/In-Memory Cache with NATS JetStream invalidation topic to purge revoked keys instantly across all ingestor nodes).
- Q: How should rate limit quotas be configured across projects and individual API keys? → A: Option A (Hierarchical Rate Limits: per-API-key limit overrides project default quota if explicitly specified).
- Q: How should key rotation behave regarding grace periods for legacy keys? → A: Option A (Configurable Grace Period: old key receives an `expires_at` timestamp [default 24h] allowing dual-active operation during migration).
- Q: How should Organization-wide API Keys work across multiple projects within an organization? → A: Option A (Support Organization-Level API Keys bound to `organization_id` with optional `project_id`. Ingestion events specify target project via payload `project_key` or `X-Project-Key` header).
- Q: How is the frontend UI flow structured for creating Organization-wide vs. Project-specific API keys? → A: Option A (Dual Context Creation: Org Settings `/[orgSlug]/settings/keys` provides a Target Project selector including "All Projects [Org-Wide]", while Project Settings `/[orgSlug]/projects/[projectId]/settings/keys` pre-locks to the current project).

---

## User Scenarios & Testing *(mandatory)*

### User Story 1 - API Key Lifecycle & Scope Management (Priority: P1)

As an Organization Admin or Engineer in the Dashboard, I want to create, inspect, scope, rotate, and revoke project API keys so that I can securely connect my applications, microservices, and client SDKs without sharing full admin credentials or risking unauthorized data access.

**Why this priority**: Without complete API key lifecycle management in the dashboard UI and backend, external services cannot safely authenticate with the ingestion cluster or management APIs, blocking production onboarding.

**Independent Test**: Can be fully tested by creating a new scoped API key (`Ingest-Only` or `Read/Query`), verifying that the secret token is shown once, confirming the SHA256 hash is stored, querying endpoints using the key, revoking the key, and asserting immediate rejection.

**Acceptance Scenarios**:

1. **Given** an Organization Admin or Engineer viewing Organization Settings (`/[orgSlug]/settings/keys`) or Project Settings (`/[orgSlug]/projects/[projectId]/settings/keys`), **When** they request a new API Key with a name, scope (`Ingest-Only`, `Read/Query`, `Admin`), and target project (including "All Projects [Org-Wide]"), **Then** the system generates a cryptographically secure random API key token, stores its SHA256 hash, displays the raw secret token to the user exactly once with a warning, and lists the key in the API keys management table.
2. **Given** an existing active API Key, **When** an Organization Admin or Engineer clicks "Revoke", **Then** the key status immediately updates to `revoked`, its revocation timestamp is recorded, and any subsequent API request using that key is immediately rejected with HTTP 401.
3. **Given** an `Ingest-Only` API key, **When** used to call ingestion endpoints (`POST /ingest` or `POST /ingest/batch`), **Then** access is granted; **When** used to call read/query management endpoints, **Then** access is denied with HTTP 403.
4. **Given** an Organization Admin or Engineer, **When** they trigger key rotation for an existing key, **Then** a new API key is generated and the old key is assigned a configurable expiration timestamp (`expires_at`, defaulting to 24 hours), maintaining dual-active validity until expiration or manual revocation.

---

### User Story 2 - Real-Time Rate Limiting & Quota Protection (Priority: P2)

As a Sentinel Platform Administrator, I want the Ingestor worker to enforce per-project and per-API-key rate limits using Redis (with in-memory token bucket fallback) so that noisy-neighbor client spikes do not degrade NATS JetStream, PostgreSQL, or system stability.

**Why this priority**: Uncapped ingestion requests from rogue or misconfigured client SDKs can cause resource exhaustion, cascading failures, and database lock contention across the monorepo.

**Independent Test**: Can be fully tested by generating rapid synthetic HTTP POST requests exceeding the configured rate limit (e.g. 5,000 requests/minute) using a valid API key, confirming that excess requests return HTTP 429 Too Many Requests with standard rate-limit headers, and verifying normal processing resumes once the rate window resets.

**Acceptance Scenarios**:

1. **Given** an active project API key with an allocated rate limit (e.g., 5,000 requests per minute), **When** incoming ingestion request volume stays within the limit, **Then** requests are authenticated and processed normally (HTTP 202 Accepted).
2. **Given** incoming request volume exceeding the allocated rate limit within a rolling time window, **When** the threshold is passed, **Then** the Ingestor middleware immediately rejects excess requests with HTTP 429 Too Many Requests, returns rate limit headers (`X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, `Retry-After`), and prevents forwarding to NATS or PostgreSQL.
3. **Given** a multi-instance Ingestor cluster deployment, **When** rate limiting is evaluated, **Then** state is synchronized centrally in Redis; **When** Redis is temporarily unreachable, **Then** Ingestor instances gracefully degrade to local in-memory token-bucket rate limiting without crashing.

---

### User Story 3 - Audit Logging & Key Activity Monitoring (Priority: P3)

As an Organization Security Auditor, I want all API key lifecycle operations (creation, scope update, rotation, revocation) and rate-limit breach alerts recorded in audit logs so that security events are fully traceable.

**Why this priority**: Security compliance and incident post-mortems require clear visibility into who created or revoked credentials and when rate limits were breached.

**Independent Test**: Can be fully tested by creating and revoking an API key, triggering a rate-limit breach, and querying `audit_logs` to confirm structured records contain `actor_id`, `action`, `resource_id`, and timestamp.

**Acceptance Scenarios**:

1. **Given** any API key management action (create, rotate, revoke), **When** the action completes, **Then** a structured audit log entry is written to `audit_logs` capturing the actor's user ID, action name, project ID, and key prefix.
2. **Given** repeated rate-limit violations from a single API key, **When** the violation count exceeds a security threshold, **Then** an audit event (`api_key.rate_limit_exceeded`) is recorded for security monitoring.

---

### Edge Cases

- **What happens when an API key is revoked while batch requests are in-flight?**: The Ingestor checks key validity on HTTP handler entry; any request already accepted into the pipeline completes asynchronously, but new HTTP requests are rejected immediately.
- **What happens when Redis connection drops during high rate-limit traffic?**: Ingestor falls back seamlessly to local in-memory token bucket rate limiting; logs warning once without dropping valid requests.
- **What happens when a project has 0 active API keys?**: Ingestion endpoints return HTTP 401 for that project key; Dashboard UI prompts project members to generate their first API key.
- **What happens if a raw API key token is lost?**: Raw tokens are never stored in the database (only SHA256 hashes); the UI displays secret tokens ONCE upon creation. Lost keys must be rotated/re-generated.

---

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: System MUST support complete API Key lifecycle management in Dashboard UI (`apps/dashboard-web`) including listing, creation, scope configuration, rotation, and revocation.
- **FR-002**: System MUST store API keys using SHA256 hash digests (`api_key_hash`). Raw key tokens MUST be displayed to users ONLY ONCE upon creation and NEVER stored in plaintext.
- **FR-003**: System MUST enforce Key Scoping with distinct permissions:
  - `Ingest-Only` (`ingest`): Restricted strictly to error ingestion endpoints (`POST /ingest`, `POST /ingest/batch`).
  - `Read/Query` (`read`): Restricted to Dashboard/Management APIs and query endpoints.
  - `Admin` (`admin`): Full access to project management and ingestion.
- **FR-004**: System MUST reject requests using revoked or expired API keys immediately across all services (`apps/ingestor-go` and `apps/dashboard-web`) with HTTP 401 Unauthorized. `apps/ingestor-go` MUST cache valid keys in Redis/memory and subscribe to a NATS JetStream invalidation topic (`api_key.invalidated`) to purge cache entries instantly (<100ms) upon revocation.
- **FR-005**: Ingestor worker (`apps/ingestor-go`) MUST enforce multi-tenant rate limiting per project/API-key using Redis sliding window / token bucket algorithm with local in-memory fallback. Evaluation MUST follow hierarchical rules: per-API-key `rate_limit_rpm` applies if configured; otherwise project default quota (default 5,000 RPM) applies.
- **FR-006**: System MUST return `HTTP 429 Too Many Requests` with standard RFC rate-limit response headers (`X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, `Retry-After`) when quotas are exceeded.
- **FR-007**: System MUST validate user organization roles (`owner`, `admin`, `engineer`) before allowing API key management actions according to project RBAC rules.
- **FR-008**: System MUST write structured audit records to `audit_logs` for all API key lifecycle events (`api_key.create`, `api_key.rotate`, `api_key.revoke`).

---

### Key Entities *(include if feature involves data)*

- **Project & Organization API Keys (`project_api_keys`)**:
  - `id`: UUID (Primary Key)
  - `organizationId`: UUID (Foreign Key -> `organizations.id`)
  - `projectId`: UUID (Nullable Foreign Key -> `projects.id`; null indicates Organization-wide key)
  - `name`: String (Human-readable label, e.g. "Org-Wide Ingestor Key")
  - `keyPrefix`: String (First 8 characters for identification, e.g. `sent_org_` or `sent_live_`)
  - `keyHash`: String (SHA256 hash digest of the full secret token)
  - `scope`: String Enum (`ingest`, `read`, `admin`)
  - `status`: String Enum (`active`, `revoked`, `expired`)
  - `rateLimitRpm`: Integer (Requests per minute limit, default 5000)
  - `expiresAt`: Timestamp (Nullable)
  - `revokedAt`: Timestamp (Nullable)
  - `createdBy`: String (User ID)
  - `createdAt`: Timestamp
- **Rate Limit State (`redis` / `in-memory`)**:
  - Key: `ratelimit:{project_id}:{key_hash_prefix}:{window_minute}`
  - Counter: Integer (Current request count)
  - TTL: 60 Seconds

---

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: API key revocation takes effect immediately across all Ingestor instances (< 100 ms propagation delay).
- **SC-002**: Rate-limiting middleware evaluation adds less than 1 ms overhead per HTTP request under normal load.
- **SC-003**: 100% of excess requests above configured project quotas return `HTTP 429` without reaching NATS or PostgreSQL.
- **SC-004**: Zero plaintext API keys stored in database tables or server log outputs.
- **SC-005**: 100% of API key creation, rotation, and revocation events produce auditable log records in `audit_logs`.

---

## Assumptions

- API keys use a recognizable prefix format (`sent_live_` or `sent_test_`) followed by 32 cryptographically random bytes (hex/base64 encoded).
- Default project rate limit is 5,000 requests per minute unless overridden by Organization Admin.
- The existing `projects` table will be updated or linked to `project_api_keys` table to support multiple keys per project cleanly.
- Redis connection parameters are configured via environment variables (`REDIS_ADDR`, `REDIS_PASSWORD`, `REDIS_DB`).
