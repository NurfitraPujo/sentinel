# Memory Synthesis: 008-api-key-management

**Feature**: `008-api-key-management`  
**Generated**: 2026-07-26  
**Scope**: Scoped memory synthesis targeting architectural decisions and constraints applicable to multi-tenant auth and API key lifecycle management.

---

## Active Architecture Decisions Applied

1. **D6: Organization-First Multi-Tenancy & Role Inheritance**
   - **Constraint**: Authorization logic must enforce Organization boundaries first (`organizations` -> `projects`). API Key management permissions must inherit user roles (`owner`, `admin`, `engineer`).
   - **Application**: `project_api_keys` must be bound to `organization_id` (required) with optional `project_id` (null for Org-Wide keys). UI management routes exist at `/[orgSlug]/settings/keys` and `/[orgSlug]/projects/[projectId]/settings/keys`.

2. **D8: Non-Blocking Dual-Endpoint Client SDK Protocol with Auto-Initialization**
   - **Constraint**: API key validation on error ingestion hot-path (`POST /ingest`, `POST /ingest/batch`) must maintain sub-millisecond overhead (< 1ms) and never block ingestion processing.
   - **Application**: `apps/ingestor-go` caches SHA256 hashed API keys in Redis / in-memory LRU with a 60-second TTL and listens to NATS JetStream invalidation topic `api_key.invalidated` for instant purge (<100ms) on revocation.

3. **D3 & D4: Goose SQL Migrations & Strict Loud-Failure Policy**
   - **Constraint**: Schema modifications for `project_api_keys` and `projects` tables must be authored via Goose transactional DDL migrations in `packages/db-migrations/migrations/`.
   - **Application**: Single Goose migration file adding `project_api_keys` table and updating `projects` schema.

4. **B2: Reserved Path Collision Guard for Dynamic Slug Routing**
   - **Constraint**: Dashboard routes for API keys under `/[orgSlug]/settings/keys` must respect reserved top-level path guards.
   - **Application**: SvelteKit routes must be nested cleanly inside existing `[orgSlug]` layout structure.

---

## Known Deviations & Assumptions

- **Plaintext Key Tokens**: Raw API key tokens (e.g. `sent_live_...` or `sent_org_...`) are generated once using cryptographically secure random bytes, hashed via SHA256 (`api_key_hash`), and shown to the user exactly once. Plaintext keys are NEVER stored in PostgreSQL or logged.
- **Hierarchical Rate Limits**: Per-key `rate_limit_rpm` overrides project-level quota if set; defaults to 5,000 RPM per project.
