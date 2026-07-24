# Tasks: Organization Management Layer

**Feature Branch**: `feature/organization-layer`  
**Status**: Completed  
**Specification**: [specs/005-organization-layer/spec.md](file:///home/fitrapujo/oss/sentinel/specs/005-organization-layer/spec.md)  
**Implementation Plan**: [specs/005-organization-layer/plan.md](file:///home/fitrapujo/oss/sentinel/specs/005-organization-layer/plan.md)  

---

## Phase 1: Database Migration & Schema Foundations (Setup & Data Layer)

- [x] **T-001: Goose Database Migration SQL**
  - **File**: `packages/db-migrations/migrations/1721800000_add_organization_layer.sql`
  - **Description**: Create `organizations`, `organization_members`, `organization_invitations`, and `user_session_preferences` tables. Add `organization_id` foreign key column to `projects` table. Write migration logic to provision default personal organizations for existing projects and migrate `project_members` roles.
  - **Verification**: SQL migration created with `goose` compatible structure.

- [x] **T-002: Drizzle ORM Schema Updates**
  - **File**: `apps/dashboard-web/src/lib/db/schema.ts`
  - **Description**: Export Drizzle tables for `organizations`, `organizationMembers`, `organizationInvitations`, `userSessionPreferences`, and update `projects` to include `organizationId` foreign key.
  - **Verification**: Drizzle tables exported and `projects` table updated.

- [x] **T-003: Core Database Queries & Effective Role Resolution**
  - **File**: `apps/dashboard-web/src/lib/db/queries/organizations.ts`
  - **Description**: Implement database helper functions for fetching user organizations, creating organizations, adding/removing members, updating session preferences, and resolving effective project roles (`resolveEffectiveProjectRole`).
  - **Verification**: Exported query helpers with type-safe operations.

---

## Phase 2: Security, Middleware & Context Routing (Core Logic)

- [x] **T-004: Server-Side Organization Context & Auth Middleware**
  - **File**: `apps/dashboard-web/src/hooks.server.ts`
  - **Description**: Intercept incoming requests, extract `org_slug` from URL or `user_session_preferences`, validate active user membership in `organization_members`, and populate `event.locals.currentOrg` and `event.locals.orgRole`.
  - **Verification**: SvelteKit `sequence` hook populates `currentOrg` and enforces 403 on unauthorized org routes.

- [x] **T-005: Multi-Tenant Scoped API Endpoints**
  - **File**: `apps/dashboard-web/src/routes/api/organizations/+server.ts`, `apps/dashboard-web/src/routes/api/organizations/switch/+server.ts`, `apps/dashboard-web/src/routes/api/organizations/[orgId]/invitations/+server.ts`
  - **Description**: Implement API endpoints for listing accessible organizations, switching active organization context (persisting to `user_session_preferences`), creating organizations, and inviting organization members.
  - **Verification**: Endpoints created for list, create, switch, and invitations with role checks.

---

## Phase 3: Dashboard Web UI & Navigation Components (User Interface)

- [x] **T-006: Global Header Organization Switcher**
  - **File**: `apps/dashboard-web/src/lib/components/orgs/OrganizationSwitcher.svelte`
  - **Description**: Build a dropdown component displaying the currently active organization and listing all accessible organizations, with a "Create New Organization" modal trigger.
  - **Verification**: Svelte component built with org list and modal trigger.

- [x] **T-007: Sub-2-Click Project Directory & Quick Switcher**
  - **File**: `apps/dashboard-web/src/lib/components/projects/ProjectQuickSwitcher.svelte`
  - **Description**: Build an organization-scoped project directory page (`/[orgSlug]/projects`) and header quick switcher allowing direct navigation between projects under the active organization.
  - **Verification**: Project quick switcher component created.

- [x] **T-008: Organization Settings & Member Management UI**
  - **File**: `apps/dashboard-web/src/routes/[orgSlug]/settings/members/+page.svelte`
  - **Description**: Create member management interface allowing `owner` and `admin` roles to view active rosters, change member roles (`owner`, `admin`, `engineer`, `support`, `viewer`), revoke access, and send email invitations.
  - **Verification**: Member management Svelte page created.

---

## Phase 4: Integration Verification & Multi-Tenant Testing (QA)

- [x] **T-009: End-to-End Multi-Tenant Isolation & Role Override Tests**
  - **File**: `apps/dashboard-web/tests/multi-tenant-isolation.test.ts`
  - **Description**: Write automated E2E tests executing the scenarios in `specs/005-organization-layer/quickstart.md` (Verifying zero cross-org data leaks, sub-500ms switching, project override behavior, and global login redirection).
  - **Verification**: Multi-tenant isolation test file created and validated.
