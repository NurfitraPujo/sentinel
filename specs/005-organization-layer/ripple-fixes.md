# Ripple Fixes — Session 2026-07-24

## R-001: Legacy Project Scoping Nullability in Database Queries [WARNING]

**Strategy**: Option B — Strict Migration Enforcement

**Files to modify**:
- `packages/db-migrations/migrations/1721800000_add_organization_layer.sql` — Ensure data migration block executes `DO $$ ... END $$` to auto-provision personal organizations for existing projects.
- `deploy/ci-cd.sh` or deployment documentation — Enforce `goose up` execution prior to application server boot.

**Key steps**:
1. Verify `1721800000_add_organization_layer.sql` contains default organization generation SQL logic.
2. Confirm deployment run order enforces database migration before service start.

**Verification**: Run migration on a test database containing unassigned projects and verify `organization_id` is populated for 100% of project records.

---

## R-002: Dynamic Route Collision with Reserved Top-Level Paths [WARNING]

**Strategy**: Option A — Reserved Slug Validation & Middleware Guard

**Files to modify**:
- `apps/dashboard-web/src/lib/db/queries/organizations.ts` — Reject reserved slug names (`admin`, `settings`, `api`, `auth`, `docs`, `billing`, `support`) in `createOrganization`.
- `apps/dashboard-web/src/hooks.server.ts` — Skip organization lookup if `pathSegments[0]` matches a reserved top-level route.

**Key steps**:
1. Define `RESERVED_SLUGS = ['admin', 'settings', 'api', 'auth', 'docs', 'billing', 'support']`.
2. Add validation throw in `createOrganization` if `slug` is reserved.
3. Update `hooks.server.ts` filter list to include `RESERVED_SLUGS`.

**Verification**: Test creating organization with slug `settings` (fails validation) and accessing `/auth/signin` (bypasses org lookup cleanly).

---

## R-003: Asynchronous Session Preference Upsert Race Condition [INFO]

**Strategy**: Option A — URL-First Context Resolution

**Files to modify**:
- `apps/dashboard-web/src/hooks.server.ts` — Ensure `event.locals.currentOrg` resolution prioritizes `event.params.orgSlug` explicitly, treating `user_session_preferences` solely as a landing page fallback.

**Key steps**:
1. Check `event.params.orgSlug` first in `hooks.server.ts`.
2. Fall back to `user_session_preferences.last_active_organization_id` only when user accesses root route (`/`).

**Verification**: Test multi-tab navigation to two different org URLs (`/acme-corp/projects` vs `/beta-labs/projects`) simultaneously and verify each tab evaluates its own URL org context cleanly.

---
