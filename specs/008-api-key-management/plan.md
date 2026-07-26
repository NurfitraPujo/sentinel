# Technical Implementation Plan: 008-api-key-management

**Feature**: `008-api-key-management`  
**Branch**: `008-api-key-management`  
**Created**: 2026-07-26  
**Status**: Draft Plan  
**Spec Reference**: [`specs/008-api-key-management/spec.md`](file:///home/fitrapujo/oss/sentinel/specs/008-api-key-management/spec.md)  
**Memory Synthesis**: [`specs/008-api-key-management/memory-synthesis.md`](file:///home/fitrapujo/oss/sentinel/specs/008-api-key-management/memory-synthesis.md)

---

## 1. Technical Architecture & Component Breakdown

```
                       ┌──────────────────────────────────────────────┐
                       │          Dashboard Web (SvelteKit)           │
                       │  - /[orgSlug]/settings/keys                  │
                       │  - /[orgSlug]/projects/[id]/settings/keys    │
                       └──────────────────────┬───────────────────────┘
                                              │ CRUD & Rotation
                                              ▼
                       ┌──────────────────────────────────────────────┐
                       │            PostgreSQL Database               │
                       │  - project_api_keys                          │
                       │  - audit_logs                                │
                       └──────────────────────┬───────────────────────┘
                                              │ NATS Invalidation Event
                                              ▼
                       ┌──────────────────────────────────────────────┐
                       │           NATS JetStream Broker              │
                       │  - Stream: api_key.invalidated               │
                       └──────────────────────┬───────────────────────┘
                                              │ Purge Cache Event (<100ms)
                                              ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                       Ingestor Worker (apps/ingestor-go)                   │
│  ┌─────────────────────────┐  ┌───────────────────────┐  ┌──────────────┐  │
│  │ Authenticator Middleware│─>│ Redis / In-Memory LRU │─>│ Rate Limiter │  │
│  │ (SHA256 Hash Cache)     │  │ (Rate Bucket state)   │  │ (HTTP 429)   │  │
│  └─────────────────────────┘  └───────────────────────┘  └──────────────┘  │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Component Scope

1. **Database Schema & Goose Migration (`packages/db-migrations`)**:
   - New Goose migration `migrations/1722000000_add_api_key_management.sql`.
   - Create `project_api_keys` table: `id`, `organization_id` (FK -> `organizations.id`), `project_id` (Nullable FK -> `projects.id`), `name`, `key_prefix`, `key_hash` (SHA256), `scope` (`ingest`, `read`, `admin`), `status` (`active`, `revoked`, `expired`), `rate_limit_rpm` (default 5000), `expires_at`, `revoked_at`, `created_by`, `created_at`.
   - Index on `(key_hash, status)` for sub-millisecond lookup performance.

2. **Drizzle ORM Schema Updates (`apps/dashboard-web/src/lib/db/schema.ts`)**:
   - Export `projectApiKeys` table schema matching PostgreSQL migration.

3. **Dashboard Web UI & Scoped API Endpoints (`apps/dashboard-web`)**:
   - API endpoints:
     - `GET /api/organizations/[orgId]/keys` — List Org-wide & Project API keys.
     - `POST /api/organizations/[orgId]/keys` — Create API key (returns raw secret ONCE).
     - `POST /api/organizations/[orgId]/keys/[keyId]/rotate` — Trigger key rotation with 24h grace period.
     - `DELETE /api/organizations/[orgId]/keys/[keyId]` — Revoke API key & publish NATS invalidation event.
   - SvelteKit Components & Pages:
     - `/[orgSlug]/settings/keys/+page.svelte`: Organization API Keys management dashboard with Target Project selector ("All Projects [Org-Wide]" or specific project).
     - `/[orgSlug]/projects/[projectId]/settings/keys/+page.svelte`: Project-scoped API keys tab.
     - `ApiKeyCreateModal.svelte`: Modal presenting scope selection, target project, and raw secret copy alert.

4. **Ingestor Multi-Tenant Auth & Rate Limiting (`apps/ingestor-go`)**:
   - **`auth/apikey.go`**: Update `APIKeyAuthenticator` to query `project_api_keys` by SHA256 hash. Support Org-Wide keys (resolving `project_key` from request payload or `X-Project-Key` header).
   - **Cache & NATS Listener**: Cache valid key hashes in Redis / in-memory LRU with 60s TTL. Subscribe to NATS JetStream `api_key.invalidated` topic to purge cached keys instantly upon revocation.
   - **`middleware/ratelimit.go`**: Implement Redis sliding window / token-bucket rate limiter with in-memory fallback. Evaluate per-key `rate_limit_rpm` if set; fall back to project default quota. Return `HTTP 429` with standard RFC headers (`X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, `Retry-After`).

---

## 2. Layer Boundaries & Architecture Alignment

- **CQRS Lite Pattern**: DB queries implemented via dedicated query module (`apps/dashboard-web/src/lib/db/queries/apikeys.ts`). Loaders and Actions delegate to domain query helpers.
- **Contract & Transport Separation**: HTTP handlers in `apps/ingestor-go` remain thin, delegating validation to `auth/apikey.go` and rate limiting to `middleware/ratelimit.go`.
- **RBAC Validation**: Auth hooks (`hooks.server.ts`) validate active user organization role (`owner`, `admin`, `engineer`) before processing API key management requests.

---

## 3. High-Risk Areas & Mitigation Strategies

| High Risk Area | Potential Failure Mode | Mitigation Strategy |
|---|---|---|
| **Revocation Propagation Lag** | Cached valid keys in `ingestor-go` instances accept revoked requests for up to 60 seconds. | Publish NATS JetStream event `api_key.invalidated` upon revocation; all `ingestor-go` workers listen and purge cache entries in < 100 ms. |
| **Redis Outage Spike** | High-volume ingestion crashes or drops events when Redis rate-limit connection fails. | Wrap Redis rate limiter in a circuit-breaker fallback to local in-memory token-bucket counters; log warning once. |
| **Secret Token Exposure** | Raw API key tokens leaked in database or server log output. | Compute SHA256 hash immediately upon generation (`sent_live_...` or `sent_org_...`), return raw token in API response once, and store ONLY `key_hash` in PostgreSQL. |
| **Org-Wide Key Ambiguity** | Ingestion requests using Org-Wide API keys lack project context. | Validate `project_key` in JSON payload or `X-Project-Key` HTTP header when `projectId` on the API key is `null`. |

---

## 4. Verification & Testing Strategy

1. **Goose SQL Migration Test**: Run migration `up` and `down` via `packages/db-migrations/cmd/migrate` against local Postgres container.
2. **Ingestor Unit & Integration Tests**:
   - `apps/ingestor-go/auth/apikey_test.go`: Verify SHA256 hash lookup, Org-Wide key project header resolution, scope validation (`ingest` vs `read`), and NATS invalidation cache purging.
   - `apps/ingestor-go/middleware/ratelimit_test.go`: Verify sliding window limit, `HTTP 429` response headers, hierarchical key/project quota evaluation, and Redis outage in-memory fallback.
3. **Dashboard Web Integration Tests**:
   - Test key creation (raw secret displayed once), rotation (24h grace period), listing, and revocation in SvelteKit endpoints.
4. **End-to-End Test Suite**:
   - Create key -> Send ingestion request (HTTP 202) -> Exceed rate limit (HTTP 429) -> Revoke key -> Verify immediate rejection (HTTP 401).
