# Phase 0 Research: Organization Management Layer

## Decisions & Architectural Choices

### 1. Database Schema & Migration Strategy
- **Decision**: Introduce `organizations`, `organization_members`, `organization_invitations`, and `user_session_preferences` tables in PostgreSQL, and update `projects` to include a required `organization_id` foreign key.
- **Rationale**: Elevating `Organization` above `Project` provides clear structural multi-tenancy.
- **Migration Strategy**: 
  1. Create new tables.
  2. For existing unassigned projects, automatically create a default personal organization (e.g., `Personal Org - <user_id>` or `Default Organization`) and populate `organization_id` on existing projects.
  3. Migrate existing `project_members` to `organization_members` with equivalent mapped roles (`admin` -> `admin`, `developer` -> `engineer`, `viewer` -> `viewer`, `support` -> `support`), while preserving `project_members` as explicit project-level overrides.
- **Alternatives Considered**:
  - *Option B (Hard Multitenancy via separate databases per org)*: Rejected due to unnecessary infrastructure complexity for a modular monolith.
  - *Option C (Soft Tagging without strict hierarchy)*: Rejected because it does not provide strong access isolation or clean UI context switching.

### 2. Organization RBAC & Project Role Inheritance
- **Decision**: Define organization roles: `owner`, `admin`, `engineer`, `support`, `viewer`.
- **Role Permissions**:
  - `owner`: Full control over organization settings, billing, deletion, member roles, project creation/deletion.
  - `admin`: Manage members/invitations, project creation, alert configurations, full project access.
  - `engineer`: Create/edit project alert rules, view issues, update issue status, view occurrences across org projects.
  - `support`: Read-only access to issues, occurrences, and search indices across org projects.
  - `viewer`: Read-only access to dashboards and high-level health metrics across org projects.
- **Inheritance & Overrides**: Organization members inherit their organization role for all projects in that organization by default. If a row exists in `project_members` for a specific `(user_id, project_id)`, that role explicitly overrides the inherited organization role for that project (or restricts access if set to restricted).
- **Rationale**: Eliminates redundant per-project assignment for team members while offering granular flexibility for contractors or restricted team sub-groups.

### 3. Authentication & Post-Login Flow
- **Decision**: Global authentication without forcing organization selection on the login page.
- **UX Flow**:
  1. User authenticates globally (email/password or SSO).
  2. Server checks `user_session_preferences.last_active_organization_id`.
  3. If valid and user is still an active member, redirect to `/<org_slug>/projects`.
  4. If invalid or null, check accessible organizations:
     - If user belongs to 1 org -> auto-set active org and redirect.
     - If user belongs to multiple orgs -> redirect to `/select-organization` picker page.
     - If user belongs to 0 orgs -> auto-create personal organization and redirect.
- **Rationale**: Provides frictionless login experience while retaining multi-organization flexibility.

### 4. Active Organization Context & URL Routing
- **Decision**: Include organization slug in Dashboard URL paths (`/<org_slug>/...`) and back it with a server-side context middleware/hook in SvelteKit.
- **Context Enforcement**:
  - Middleware inspects `params.org_slug`.
  - Verifies user's active membership in `OrganizationMember`.
  - Attaches `currentOrg` and user's `orgRole` to `event.locals`.
  - Ensures all DB queries for projects, issues, and occurrences filter strictly by `organization_id`.
- **Rationale**: URL-based context ensures shareable links, crisp browser history, and bulletproof server-side authorization.

### 5. Performance & UX Optimization for Navigation
- **Decision**: Implement header Organization Switcher dropdown + Quick Project Switcher (keyboard shortcut `Cmd+K` or quick dropdown) + SvelteKit client-side data prefetching.
- **Rationale**: Guarantees sub-500ms context switching and sub-2-click project navigation (SC-001, SC-002).
