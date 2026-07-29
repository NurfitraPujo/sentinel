# Feature Specification: Multi-Tenant Project Auth & API Key Management

**Feature Branch**: `008-api-key-management`  
**Created**: 2026-07-26  
**Status**: Completed (MERGED 2026-07-26) — tasks file checked off, but at merge time the ingest path
was tenant-scope-broken (S6: any valid key could write into any project by naming it in the body) and
the dashboard key-management API was a hardcoded-fixture mock. A follow-up fix on 2026-07-28/29 closed
S6 for the ingest path. **Multiple FRs/SCs below remain unverified or contradicted by the code as of
that fix** — see "Verified Implementation Status" immediately below before trusting any FR in this
document.  
**Verified**: 2026-07-29 — `docs/memory/VERIFIED_STATE.md` S6 (cross-tenant write) RESOLVED for
`apps/ingestor-go`. S7 (revocation/expiry) and S10 (rate-limit atomicity/fail-open) **remain open** —
re-verify before relying on them. See "Verified Implementation Status" below.  
**Input**: User description: "Multi-Tenant Project Auth & API Key Management - full API key lifecycle (creation, listing, revocation, scope management, SHA256 hashed storage), Redis/In-Memory rate limiting per key/project, and Organization RBAC integration based on docs/todos/02-multi-tenant-auth-and-api-key-management.md"

---

## Verified Implementation Status (2026-07-29)

> [!IMPORTANT]
> The FRs and SCs below this box describe the *original design intent*. Where they conflict with this
> section, **the code is the source of truth**.

### Key resolution order (RESOLVED — S6, `apps/ingestor-go/auth/apikey.go`, `main.go`)

Neither FR-004 nor the Key Entities section below describes an actual resolution order; this is what
`applyAuthenticatedScope` + the auth middleware implement, and it is now the ground truth:

1. **Project-scoped key** (`project_id` set on the key row): the project is fixed by the credential.
   The request body's `project_key` may be absent, or may name the *same* project. Naming a
   *different* project is rejected **403** (previously this silently let any valid key write into any
   tenant's project — the headline defect in S6).
2. **Organization-wide key** (`project_id` NULL): the credential fixes only the organization, not a
   project, so the caller must select one:
   a. `X-Project-Key` **header**, if present, resolved in the auth middleware — **takes precedence**.
   b. Otherwise, the body's `project_key` field, resolved in the handler (`applyAuthenticatedScope`).
   c. If neither is present: rejected — `project_key is required for an organization-wide API key`.
3. **Both (1) and (2b) resolve the project name via `ResolveProjectInOrg`, which is scoped to the
   authenticated key's `organization_id`.** A name belonging to a *different* organization returns the
   same 403 as "no such project" — indistinguishable on purpose, so it cannot be used to enumerate
   other tenants' project names. This closes the second half of S6 (global, unscoped name resolution).

This contradicts the Assumptions/Clarifications text further down, which describes org-wide key
target selection only via "payload `project_key` or `X-Project-Key` header" with no stated precedence —
the header winning, and the body being ignored once the header resolves a target, is an
implementation decision made during the S6 fix, not something the original spec specified.

### Still NOT implemented (do not mark these resolved)

- **`expires_at` is never checked** (S7, unchanged). `getAPIKeyData` filters on `status` only.
  FR-004's "reject requests using ... expired API keys" is not honored.
- **Key rotation leaves the old key `status = 'active'`** (S7, unchanged). `rotateApiKey` sets
  `expires_at` on the old key but nothing reads it (previous point), so rotated keys stay valid
  forever, not for the "configurable grace period (default 24h)" User Story 1 Acceptance Scenario 4
  describes.
- **Rate limiting is per-key only, not hierarchical.** FR-005 and the Clarifications session both
  describe "per-API-key `rate_limit_rpm` applies if configured; otherwise project default quota
  applies" (also recorded as "hierarchical" in `docs/memory/DECISIONS.md` D9). There is no project
  tier in the code and no `projects.rate_limit_rpm` column — `apps/ingestor-go/middleware/ratelimit.go`
  keys solely on `api_key_hash`. **D9's "hierarchical" language does not match the implementation.**
- **Rate limiting remains non-atomic** (S10) — four unpipelined Redis round-trips
  (`ZRemRangeByScore`→`ZCard`→decide→`ZAdd`), not a Lua script or pipeline. Concurrent requests can all
  read the same count before any write lands, so the effective limit under concurrency is higher than
  configured. Whether the nil-Redis-client fail-open path (`redisClient, _ := redis.NewClient(...)`,
  silently disabling rate limiting if Redis is unreachable at boot) still exists was **not
  re-verified** as part of this pass — check `apps/ingestor-go/main.go` before relying on either
  direction.
- **`apps/dashboard-web` key-management HTTP surface**: not touched by the 2026-07-28/29 fix pass.
  `VERIFIED_STATE.md` (2026-07-26 baseline) recorded `api/organizations/[orgId]/keys/+server.ts` as a
  mock returning hardcoded fixtures with no auth check — **unverified whether this has changed**; treat
  User Story 1's dashboard flows as unverified until re-checked.

---

## Clarifications

### Session 2026-07-26
- Q: How should API key validity be cached in `ingestor-go` to balance instant revocation (<100ms) with sub-millisecond database protection? → A: Option A (Redis/In-Memory Cache with NATS JetStream invalidation topic to purge revoked keys instantly across all ingestor nodes).
- Q: How should rate limit quotas be configured across projects and individual API keys? → A: Option A (Hierarchical Rate Limits: per-API-key limit overrides project default quota if explicitly specified). **Not what was built** — see "Verified Implementation Status" above: implementation is per-key only, no project tier exists.
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
