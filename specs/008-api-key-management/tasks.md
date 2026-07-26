# Implementation Tasks: 008-api-key-management

**Feature**: `008-api-key-management`  
**Generated**: 2026-07-26  
**Status**: Ready for Implementation  
**Spec Reference**: [`specs/008-api-key-management/spec.md`](file:///home/fitrapujo/oss/sentinel/specs/008-api-key-management/spec.md)  
**Plan Reference**: [`specs/008-api-key-management/plan.md`](file:///home/fitrapujo/oss/sentinel/specs/008-api-key-management/plan.md)  
**Security Constraints**: [`specs/008-api-key-management/security-constraints.md`](file:///home/fitrapujo/oss/sentinel/specs/008-api-key-management/security-constraints.md)

---

## Task Breakdown & Dependency Ordering

### Phase 1: Database Migration & Schema Setup (Foundational)

#### Task T-001: Goose SQL Migration for API Key Management
- **Goal**: Create PostgreSQL tables and indexes for API keys and rate limit configurations.
- **Files to create/modify**:
  - `packages/db-migrations/migrations/1722000000_add_api_key_management.sql`
- **Requirements**:
  - Table `project_api_keys`: `id` (UUID PK), `organization_id` (UUID FK -> `organizations.id` ON DELETE CASCADE), `project_id` (UUID FK -> `projects.id` ON DELETE CASCADE, NULLABLE), `name` (VARCHAR 255), `key_prefix` (VARCHAR 16), `key_hash` (VARCHAR 128 UNIQUE), `scope` (VARCHAR 20 DEFAULT 'ingest' CHECK scope IN ('ingest', 'read', 'admin')), `status` (VARCHAR 20 DEFAULT 'active' CHECK status IN ('active', 'revoked', 'expired')), `rate_limit_rpm` (INTEGER DEFAULT 5000), `expires_at` (TIMESTAMPTZ NULLABLE), `revoked_at` (TIMESTAMPTZ NULLABLE), `created_by` (VARCHAR 255), `created_at` (TIMESTAMPTZ DEFAULT NOW()).
  - Index `idx_api_keys_hash_status` on `(key_hash, status)`.
  - Index `idx_api_keys_org_project` on `(organization_id, project_id)`.
- **Dedicated Test**:
  - `packages/db-migrations/migrations_api_keys_test.go`: Run migration `up` and `down` against PostgreSQL test container, asserting table creation, index integrity, and foreign key constraints.

#### Task T-002: Drizzle ORM Schema Updates
- **Goal**: Export Drizzle ORM table definition matching PostgreSQL migration in Dashboard app.
- **Files to create/modify**:
  - `apps/dashboard-web/src/lib/db/schema.ts`
- **Requirements**:
  - Export `projectApiKeys` table matching `1722000000_add_api_key_management.sql`.
- **Dedicated Test**:
  - `apps/dashboard-web/src/lib/db/schema_api_keys.test.ts`: Validate Drizzle ORM schema mapping and TypeScript type exports.

---

### Phase 2: Core Database Queries & Service Layer

#### Task T-003: API Key Database Queries & Rotation Service
- **Goal**: Implement CQRS Lite database queries for API key lifecycle management.
- **Files to create/modify**:
  - `apps/dashboard-web/src/lib/db/queries/apikeys.ts`
- **Requirements**:
  - `getOrganizationApiKeys(orgId: string)`: List all keys for an organization (including Org-Wide and Project-scoped keys).
  - `createApiKey(userId: string, data: { organizationId: string, projectId?: string | null, name: string, scope: 'ingest' | 'read' | 'admin', rateLimitRpm?: number })`: Generates 32-byte cryptographically random token (`sent_live_...` or `sent_org_...`), computes SHA256 `key_hash`, inserts into `project_api_keys`, writes audit log to `audit_logs`, and returns raw secret token ONCE.
  - `rotateApiKey(userId: string, keyId: string, gracePeriodDuration?: string)`: Generates new key, sets `expires_at = NOW() + gracePeriod` (default 24h) on legacy key, and logs audit event.
  - `revokeApiKey(userId: string, keyId: string)`: Updates status to `revoked`, sets `revoked_at = NOW()`, writes audit log, and publishes NATS JetStream invalidation event `api_key.invalidated`.
- **Dedicated Test**:
  - `apps/dashboard-web/src/lib/db/queries/apikeys.test.ts`: Test API key creation (assert SHA256 storage), rotation (assert 24h grace period), revocation, and audit log generation.

---

### Phase 3: Ingestor Worker Auth & Rate Limiting

#### Task T-004: Ingestor API Key Authentication & NATS Invalidation Listener
- **Goal**: Update `apps/ingestor-go/auth/apikey.go` with cached SHA256 validation and NATS JetStream instant revocation listener.
- **Files to create/modify**:
  - `apps/ingestor-go/auth/apikey.go`
  - `apps/ingestor-go/auth/apikey_test.go`
- **Requirements**:
  - Compute SHA256 hash of incoming `X-API-Key`.
  - Cache valid key records in Redis / in-memory LRU with 60s TTL.
  - Subscribe to NATS JetStream topic `api_key.invalidated` to purge cached entries in < 100 ms upon revocation.
  - Support Organization-Wide keys (`projectId == null`): resolve target project key from request JSON payload `project_key` or `X-Project-Key` HTTP header.
  - Reject `Read/Query` scoped keys on ingestion endpoints with `HTTP 403 Forbidden`.
- **Dedicated Test**:
  - `apps/ingestor-go/auth/apikey_test.go`: Test SHA256 validation, Redis/memory caching, NATS invalidation event purge (<100ms), scope checks, and Org-Wide key header resolution.

#### Task T-005: Multi-Tenant Hierarchical Rate Limiter Middleware
- **Goal**: Implement sliding window / token bucket rate limiter in `apps/ingestor-go/middleware/ratelimit.go`.
- **Files to create/modify**:
  - `apps/ingestor-go/middleware/ratelimit.go`
  - `apps/ingestor-go/middleware/ratelimit_test.go`
- **Requirements**:
  - Evaluate per-key `rate_limit_rpm` if explicitly set; fall back to project default quota (5,000 RPM).
  - Use Redis sliding window / token bucket counter; fall back seamlessly to local in-memory token-bucket counters if Redis connection drops.
  - Return `HTTP 429 Too Many Requests` when limit is exceeded with standard RFC rate limit headers (`X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, `Retry-After`).
- **Dedicated Test**:
  - `apps/ingestor-go/middleware/ratelimit_test.go`: Test rate-limit evaluation, `HTTP 429` header output, hierarchical key vs project quota logic, and in-memory fallback during Redis disconnect.

---

### Phase 4: Dashboard API Endpoints & SvelteKit UI Components

#### Task T-006: Multi-Tenant Scoped API Key Endpoints
- **Goal**: Implement SvelteKit server endpoints for API key management.
- **Files to create/modify**:
  - `apps/dashboard-web/src/routes/api/organizations/[orgId]/keys/+server.ts`
  - `apps/dashboard-web/src/routes/api/organizations/[orgId]/keys/[keyId]/rotate/+server.ts`
  - `apps/dashboard-web/src/routes/api/organizations/[orgId]/keys/[keyId]/+server.ts`
- **Requirements**:
  - Validate user organization membership (`owner`, `admin`, `engineer`).
  - GET: List organization & project keys.
  - POST: Create key, returning raw secret ONCE.
  - POST rotate: Trigger key rotation with 24h grace period.
  - DELETE: Revoke key & publish NATS invalidation event.
- **Dedicated Test**:
  - `apps/dashboard-web/src/routes/api/organizations/keys.test.ts`: End-to-end API endpoint tests for CRUD operations and RBAC role checks.

#### Task T-007: Dashboard API Key Management UI Components
- **Goal**: Create SvelteKit UI components for API key management in Organization and Project settings.
- **Files to create/modify**:
  - `apps/dashboard-web/src/lib/components/keys/ApiKeyTable.svelte`
  - `apps/dashboard-web/src/lib/components/keys/ApiKeyCreateModal.svelte`
  - `apps/dashboard-web/src/routes/[orgSlug]/settings/keys/+page.svelte`
  - `apps/dashboard-web/src/routes/[orgSlug]/projects/[projectId]/settings/keys/+page.svelte`
- **Requirements**:
  - `ApiKeyTable.svelte`: Render active/revoked/expired keys, scope badges (`Ingest-Only`, `Read/Query`, `Admin`), key prefix (`sent_live_...`), creation date, and action triggers (Rotate, Revoke).
  - `ApiKeyCreateModal.svelte`: Form modal with Target Project selector ("All Projects [Org-Wide]" or specific project), scope picker, key name, and rate limit override input. Displays secret token ONCE with one-click copy button.
  - `[orgSlug]/settings/keys/+page.svelte`: Organization API Keys dashboard.
  - `[orgSlug]/projects/[projectId]/settings/keys/+page.svelte`: Project-scoped API Keys settings tab.
- **Dedicated Test**:
  - `apps/dashboard-web/src/lib/components/keys/ApiKeyTable.test.ts`: Component rendering tests for table rows, scope badges, creation modal, and secret copy alert.

---

### Phase 5: End-to-End Integration & Security Verification

#### Task T-008: End-to-End API Key & Rate Limiting Integration Pipeline
- **Goal**: Build integrated E2E verification test pipeline for API key lifecycle and rate limiting.
- **Files to create/modify**:
  - `tests/integration/api_key_lifecycle_test.go`
- **Requirements**:
  - Create API Key via DB queries/API -> Ingest error with key (HTTP 202) -> Exceed rate limit (HTTP 429) -> Revoke API Key -> Assert instant rejection in `ingestor-go` (< 100 ms via NATS invalidation) with HTTP 401.
- **Dedicated Test**:
  - `tests/integration/api_key_lifecycle_test.go`: Complete E2E integration test suite.
