# Recurring Bug Patterns (`docs/memory/`)

This file stores durable implementation bug patterns and their mitigations. For systemic, high-risk, or governance-level patterns, see `.specify/memory/BUGS.md`.

### 2024-05-20 - Data Loss on Database Outage
**Status**
Active

**Symptoms**
Events are lost or dropped when the Processor cannot reach the PostgreSQL database.

**Root Cause**
Processor service traditionally assumed the database is always available during event processing.

**Future mistake prevented**
Failing to handle transient database connection failures in processing workers.

**Evidence**
Historical analysis of ingestion gaps during database maintenance windows.

**Prevention / Detection**
Use the `GracefulDegradation` buffer in `apps/processor-go/degradation`. Monitor `WARNING: Database unavailable` logs and buffer size metrics.

**Where to look next**
`apps/processor-go/degradation/buffer.go`

---

### 2026-07-24 - Reserved Path Collision Guard for Dynamic Slug Routing

**Status**
Active

**Symptoms**
Navigating to global static pages like `/settings` or `/admin` triggers a 403 Forbidden error because server middleware attempts to look up an organization named `settings`.

**Root Cause**
Catch-all dynamic top-level parameters (`/[orgSlug]`) in SvelteKit intersect with global static routes unless explicit reservation checks are implemented.

**Future mistake prevented**
Top-level dynamic routing breaking system endpoints or trapping users in unauthorized org lookup errors.

**Evidence**
Ripple scan finding `R-002` in `specs/005-organization-layer/ripple-report.md`.

**Prevention / Detection**
Maintain an explicit `RESERVED_SLUGS` list (`['admin', 'settings', 'api', 'auth', 'docs', 'billing', 'support']`) enforced both at creation validation (`createOrganization`) and in server middleware (`hooks.server.ts`).

**Where to look next**
`apps/dashboard-web/src/lib/db/queries/organizations.ts` (`RESERVED_SLUGS`) and `apps/dashboard-web/src/hooks.server.ts`.
