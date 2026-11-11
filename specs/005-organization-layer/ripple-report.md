# Ripple Report: Organization Management Layer

**Branch**: `005-organization-layer` | **Scanned**: 2026-07-24T17:51:30+07:00
**Baseline**: `6c455c2` (branch point from main)
**Change Set**: 11 implementation files changed | **Blast Radius**: 14 dependent modules checked
**Findings**: 0 critical, 2 warning, 1 info

## Summary

The Organization Management Layer implementation successfully introduces multi-tenant hierarchy (`organizations`, `organization_members`, `organization_invitations`, `user_session_preferences`) and server-side context routing. The ripple scan identified 2 warning-level side effects (unhandled nullability on legacy project records in queries without fallback migration run, and SvelteKit route context ambiguity for top-level dynamic slugs) and 1 info-level side effect (session preference race condition on rapid org switching).

## Findings

### WARNING

#### R-001: Legacy Project Scoping Nullability in Database Queries

- **Category**: Data Flow
- **Cause**: Added `organization_id` foreign key to `projects` (`schema.ts` and `1721800000_add_organization_layer.sql`). Existing project queries (`queries/projects.ts`) expect `organization_id` to be populated.
- **Affected**: `apps/dashboard-web/src/lib/db/queries/projects.ts` (line ~12)
- **Blast Radius**: Project overview pages, error occurrence ingestion routing
- **Before**: Projects were queried without organization context (`WHERE id = :projectId`).
- **After**: Queries filter by `organizationId`. If database migration `1721800000_add_organization_layer.sql` has not run yet on an active environment, `organization_id` will be `NULL`, causing queries to return empty project sets.
- **Why Tests Miss It**: Unit tests run against freshly seeded database tables with `organization_id` populated; missing real DB upgrade state test.
- **Recommendation**: Ensure `1721800000_add_organization_layer.sql` data migration step is executed immediately upon deployment, and add a fallback query condition for null org IDs during migration window.
- **Status**: RESOLUTION_PLANNED
- **Resolution Strategy**: Option B: Strict Migration Enforcement — Rely strictly on `1721800000_add_organization_layer.sql` data migration step in CI/CD deployment pipeline to populate `organization_id` for legacy projects prior to app startup (chosen on 2026-07-24).

---

#### R-002: Dynamic Route Collision with Reserved Top-Level Paths

- **Category**: Interface Contract
- **Cause**: Updated `hooks.server.ts` to interpret the first URL path segment as `orgSlug` (e.g., `/[orgSlug]/projects`).
- **Affected**: `apps/dashboard-web/src/hooks.server.ts` (line ~15)
- **Blast Radius**: `/[orgSlug]/settings`, `/api/...`, `/auth/...`
- **Before**: Routes were static or project-slug based (`/projects/...`, `/auth/...`).
- **After**: If an organization slug matches a future top-level static route (e.g., `settings`, `admin`, `docs`), `hooks.server.ts` attempts to look up an organization named `settings` or `admin`, resulting in a 403 Forbidden error for global pages.
- **Why Tests Miss It**: Test organization slugs (`acme-corp`, `beta-labs`) do not overlap with reserved system path names.
- **Recommendation**: Maintain a strict reserved slug list (`admin`, `settings`, `api`, `auth`, `docs`, `support`, `billing`) in `createOrganization` validation and `hooks.server.ts`.
- **Status**: RESOLUTION_PLANNED
- **Resolution Strategy**: Option A: Reserved Slug Validation & Middleware Guard — Add reserved words check (`admin`, `settings`, `api`, `auth`, `docs`, `billing`, `support`) to `createOrganization` validation AND update `hooks.server.ts` to ignore reserved top-level prefixes (chosen on 2026-07-24).

---

### INFO

#### R-003: Asynchronous Session Preference Upsert Race Condition

- **Category**: Concurrency
- **Cause**: Added `updateUserLastActiveOrg` (`queries/organizations.ts`) called asynchronously on org switch (`POST /api/organizations/switch`).
- **Affected**: `apps/dashboard-web/src/lib/db/queries/organizations.ts` (line ~75)
- **Blast Radius**: `user_session_preferences` table
- **Before**: User active context was stateless per request URL.
- **After**: Rapidly switching organizations in multiple browser tabs simultaneously triggers concurrent upserts to `user_session_preferences`, potentially leaving `last_active_organization_id` out of sync with the most recently focused tab.
- **Why Tests Miss It**: Single-threaded HTTP request tests do not simulate concurrent multi-tab switching.
- **Recommendation**: Include client-side request debouncing or rely primarily on explicit URL route parameter (`params.orgSlug`) as the source of truth for active context.
- **Status**: RESOLUTION_PLANNED
- **Resolution Strategy**: Option A: URL-First Context Resolution — Treat `user_session_preferences.last_active_organization_id` strictly as a fallback preference for root `/` landing, while relying on explicit URL route parameter (`/[orgSlug]`) for 100% of active page requests and server context resolution (chosen on 2026-07-24).

---

## Coverage Gap Matrix

| Category | Critical | Warning | Info | Not Applicable |
|----------|----------|---------|------|----------------|
| Data Flow | 0 | 1 | 0 | |
| State & Lifecycle | 0 | 0 | 0 | Clear |
| Interface Contract | 0 | 1 | 0 | |
| Resource & Performance | 0 | 0 | 0 | Clear |
| Concurrency | 0 | 0 | 1 | |
| Distributed Coordination | 0 | 0 | 0 | N/A — Single monolith app |
| Configuration & Environment | 0 | 0 | 0 | Clear |
| Error Propagation | 0 | 0 | 0 | Clear |
| Observability | 0 | 0 | 0 | Clear |

## Resolution History

| Date | Scope | Resolved | Accepted Risk | Skipped | Still Open |
|------|-------|----------|---------------|---------|------------|
| 2026-07-24 | all | 3 | 0 | 0 | 0 |

### Session detail (2026-07-24)
- **R-001**: Option B (Strict Migration Enforcement)
- **R-002**: Option A (Reserved Slug Validation & Middleware Guard)
- **R-003**: Option A (URL-First Context Resolution)

## Next Steps

- [x] All 3 ripple findings resolved with planned strategies in `specs/005-organization-layer/ripple-fixes.md`.
- [ ] Implement planned resolution fixes via `/speckit-implement` or manual coding.
- [ ] Run `/speckit-ripple-check` to verify resolution.
