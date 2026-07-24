# Phase 1 Quickstart & Validation Guide: Organization Management Layer

This guide documents end-to-end scenarios to validate that the Organization Management Layer functions correctly, respects RBAC inheritance, supports smooth context switching, and enforces strict multi-tenant isolation.

## Prerequisites

1. PostgreSQL database running locally or in Docker.
2. Database migration applied (`goose up` under `packages/db-migrations`).
3. Sentinel Web Dashboard dev server running (`npm run dev` under `apps/dashboard-web`).

---

## Validation Scenario 1: Multi-Tenant Data Isolation & Context Switching (SC-001, SC-003)

### Goal
Verify that changing the active organization context immediately switches the visible projects and issues without data leaking across organization boundaries.

### Procedure
1. Seed 2 Organizations in DB:
   - **Org A**: `Acme Corp` (Slug: `acme-corp`), contains Project `Acme API`.
   - **Org B**: `Beta Labs` (Slug: `beta-labs`), contains Project `Beta Worker`.
2. Log in as `user_alice` (Member of both Org A and Org B).
3. Select `Acme Corp` from the header Organization Switcher dropdown.
4. **Verification**:
   - Verify URL changes to `/acme-corp/projects`.
   - Verify project list displays ONLY `Acme API`.
   - Verify response time for switching context is `< 500ms`.
5. Switch Organization context to `Beta Labs`.
6. **Verification**:
   - Verify URL changes to `/beta-labs/projects`.
   - Verify project list displays ONLY `Beta Worker`.
   - Verify `Acme API` is not accessible or visible under `Beta Labs`.

---

## Validation Scenario 2: Smooth Project Navigation within Organization Scope (SC-002)

### Goal
Verify that an organization member can switch between projects under the same organization in 2 clicks or fewer.

### Procedure
1. Navigate to `/acme-corp/projects/acme-api/issues`.
2. Click the persistent Project Switcher dropdown in the header or breadcrumb bar.
3. Select `Acme Billing` from the dropdown list.
4. **Verification**:
   - Navigation completes in 1 click from dropdown.
   - User is routed directly to `/acme-corp/projects/acme-billing/issues`.
   - Breadcrumb displays: `Acme Corp > Acme Billing > Issues`.

---

## Validation Scenario 3: Organization Role Inheritance & Project Overrides (FR-006, FR-007)

### Goal
Verify that an Organization Engineer inherits engineer access to all org projects by default, but can be restricted via a `project_members` override.

### Procedure
1. Set `user_bob` as `engineer` in `organization_members` for `Acme Corp`.
2. Create a row in `project_members` for `user_bob` under Project `Acme Vault` with role `viewer`.
3. Log in as `user_bob`.
4. Access `Acme API` (No project override row):
   - **Verification**: `user_bob` has full `engineer` permissions (can resolve issues, create alert configs).
5. Access `Acme Vault` (Has `viewer` override row):
   - **Verification**: `user_bob` has restricted `viewer` permissions (read-only, cannot resolve issues or edit alert configs).

---

## Validation Scenario 4: Global Login & Post-Login Redirection (FR-003, FR-004)

### Goal
Verify that global login routes users to their last active organization or single organization smoothly.

### Procedure
1. Clear session cookie and log in via `/login` with credentials for `user_alice`.
2. Observe post-login redirect.
3. **Verification**:
   - No organization picker was forced during credential submission.
   - User is automatically redirected to `/<last_active_org_slug>/projects`.
