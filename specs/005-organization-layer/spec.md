# Feature Specification: Organization Management Layer

**Feature Branch**: `feature/organization-layer`  
**Created**: 2026-07-24  
**Status**: Draft — `tasks.md` for this feature is marked "Completed"; the schema and dashboard code
merged. The dashboard-side isolation claims (FR-006, SC-003) were not part of the 2026-07-28/29 fix
pass and remain **unverified by this pass** — the entries below cover only the ingest-side tenancy hole
(S6) that this layer's `organization_id`/`projects.name` design made possible.  
**Verified**: 2026-07-29 (partial — ingest path only) — `docs/memory/VERIFIED_STATE.md` **S6** (any
valid ingest key could write into any organization's project) RESOLVED for `apps/ingestor-go`; see
`specs/008-api-key-management/spec.md` "Verified Implementation Status" for the resolution order. A
new `CREATE UNIQUE INDEX idx_projects_org_name ON projects (organization_id, name)` was added — same
project name is still permitted across *different* organizations (by design; `name` is scoped to the
organization, matching FR-001's "each Project MUST belong to exactly one Organization"), but is now
rejected as a duplicate *within* one organization. **Dashboard-side org isolation (SC-003) was not
re-verified in this pass** — do not treat it as confirmed.  
**Input**: User description: "we need to add another layer on top of projects named organization because an organization naturally have multiple projects and we must make it smooth for organization members to navigate between their projects, we also need to make sure a user may have access to multiple organizations to make it flexible"

## Clarifications

### Session 2026-07-24
- Q: How should organization member roles and project access rights interact with existing project_members roles? → A: Organization Membership + Inherited Project Access with Optional Project Overrides. Organization roles are set to `owner`, `admin`, `engineer`, `support`, and `viewer`. Organization members inherit permissions for all projects under the organization by default, while `project_members` (or explicit project access overrides) allows fine-grained access restriction or override per project.
- Q: Can users choose an organization to log into directly on the login form, or do they select/default to an organization after successful authentication? → A: Global Authentication + Post-Login Landing. Users log in with global credentials (without choosing an organization upfront). Upon successful login, the system automatically lands them in their last active organization (or single organization if they belong to only one), or routes them to an organization switcher/picker if no last active state exists.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Multi-Organization Access & Seamless Context Switching (Priority: P1)

As a user belonging to multiple organizations (e.g., separate companies, clients, or departments), I want to switch between my accessible organizations and view their respective project suites from a unified navigation control, so that I can manage work across organizational boundaries without signing out or re-authenticating.

**Why this priority**: Essential architectural shift. Users require explicit organizational context to access, organize, and navigate their underlying projects.

**Independent Test**: Log in as a user assigned to 3 distinct organizations. Select an organization from the header navigation, verify that only projects belonging to that selected organization are displayed, and switch to another organization to confirm instantaneous context transition.

**Acceptance Scenarios**:

1. **Given** a user authenticating into Sentinel, **When** login succeeds, **Then** the user is authenticated globally without requiring organization selection on the login form, and is automatically redirected to their last active organization dashboard (or single organization if they belong to only one).
2. **Given** a logged-in user with memberships in multiple organizations, **When** they view the application header/navigation, **Then** an organization selector is prominently visible showing their active organization and a list of all organizations they can access.
3. **Given** a user switching organizations via the selector, **When** they select a different organization, **Then** the active organization context updates immediately, updating the session's last-active organization preference and refreshing the project list, dashboards, and navigation options to reflect only the selected organization's scope.
4. **Given** a user accessing a direct URL for a project within Organization A, **When** their active session context is currently set to Organization B (and they have valid access to Organization A), **Then** the application automatically updates the active organization context to Organization A and loads the project seamlessly.

---

### User Story 2 - Smooth Project Navigation within Organization Scope (Priority: P1)

As an organization member, I want to view all projects under my active organization in a structured dashboard and navigate between projects smoothly, so that I can monitor and work on multiple project environments effortlessly.

**Why this priority**: Core usability requirement for organization members working across multiple internal projects.

**Independent Test**: Navigate to the organization project directory, select a project to view its details/errors, and use a persistent project quick-switcher or breadcrumb menu to navigate directly to another project under the same organization in under 2 clicks.

**Acceptance Scenarios**:

1. **Given** an active organization context, **When** a member navigates to the main project directory, **Then** they see a clear overview of all projects linked to that organization, including key health/error indicators.
2. **Given** a user viewing a specific project, **When** they interact with the project navigation bar, **Then** they can seamlessly jump to any other project in the same organization without returning to the global home page.
3. **Given** a user with access to multiple projects in an organization, **When** they search or filter projects within the organization space, **Then** results are instantly filtered by project name, environment, or status.

---

### User Story 3 - Organization Member Management & Access Control (Priority: P2)

As an organization owner or administrator, I want to invite members to our organization and manage their role-based access across all or specific projects under the organization, so that team access is securely managed at the organization level.

**Why this priority**: Crucial for security, governance, and team onboarding across multi-project organizations.

**Independent Test**: As an organization admin, invite a new member via email, assign them an organization role (`owner`, `admin`, `engineer`, `support`, `viewer`), and verify that they can access the organization and its associated projects according to role inheritance and explicit project overrides.

**Acceptance Scenarios**:

1. **Given** an organization admin, **When** they invite a user to the organization, **Then** an invitation is generated, allowing the user to join the organization with designated organization-level permissions (`owner`, `admin`, `engineer`, `support`, or `viewer`).
2. **Given** an organization member, **When** their organization membership status or role is updated or revoked by an administrator, **Then** their access permissions across all projects under that organization take effect immediately.
3. **Given** an organization member with restricted project requirements, **When** a project-level override is set in `project_members`, **Then** the project-specific access override takes precedence over their inherited organization role for that project.

---

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The system MUST introduce an Organization entity situated above Projects in the resource hierarchy, where each Project MUST belong to exactly one Organization.
- **FR-002**: The system MUST support a many-to-many relationship between Users and Organizations, allowing a single User to belong to multiple Organizations with distinct roles in each.
- **FR-003**: The system MUST support global user authentication, where users log in without choosing an organization on the login form.
- **FR-004**: The system MUST automatically route users post-login to their last active organization context (or single organization), or present an organization picker screen if no last active state exists.
- **FR-005**: The system MUST provide an intuitive organization selector in the application UI, enabling users to switch their active organization context at any time and persisting their last active organization choice.
- **FR-006**: The system MUST scope all project listings, metrics, error reports, and configurations strictly to the currently selected active Organization context.
- **FR-007**: The system MUST provide smooth, single-click navigation controls (such as breadcrumbs and project switchers) for moving between projects within the active organization.
- **FR-008**: The system MUST enforce organization-level role-based access control (RBAC), defining standardized organization roles (`owner`, `admin`, `engineer`, `support`, `viewer`) that govern permissions for managing organizations, project creation, member invitations, and monitoring capabilities.
- **FR-009**: The system MUST implement role inheritance from Organization to Projects by default, while supporting optional fine-grained `project_members` overrides to restrict or customize specific user access on a per-project basis.
- **FR-010**: The system MUST allow Organization Administrators to manage member invitations, view active member rosters, and update member permissions within the organization.
- **FR-011**: The system MUST ensure that revoking or modifying a user's membership in an Organization instantly updates or terminates their access to all associated projects under that Organization (unless explicitly constrained by project-level overrides).

### Success Criteria

- **SC-001**: **Context Switching Speed**: Switching active organization context updates the UI and loads organization-scoped project data in under 500 milliseconds.
- **SC-002**: **Navigation Efficiency**: Users can navigate from any project within an organization to any other project in the same organization in 2 clicks or fewer.
- **SC-003**: **Multi-Tenant Isolation**: 100% of project data, error alerts, and configurations are strictly isolated between organizations, ensuring zero cross-organization data leaks.
- **SC-004**: **Flexibility & Scalability**: Users can belong to an unlimited number of organizations without UI performance degradation or access ambiguity.

## Key Entities *(mandatory)*

- **Organization**: Represents a company, team, or parent entity holding multiple projects. Contains `id`, `name`, `slug`, `avatar_url`, `created_at`, `updated_at`.
- **OrganizationMember**: Represents a user's membership in an organization. Contains `id`, `organization_id`, `user_id`, `role` (`owner`, `admin`, `engineer`, `support`, `viewer`), `joined_at`.
- **OrganizationInvitation**: Represents a pending invite for a user to join an organization. Contains `id`, `organization_id`, `email`, `role` (`owner`, `admin`, `engineer`, `support`, `viewer`), `token`, `status`, `expires_at`.
- **ProjectMember (Override)**: Represents project-specific role overrides or access limits. Contains `id`, `project_id`, `user_id`, `role` (`admin`, `engineer`, `support`, `viewer`), `created_at`.
- **Project**: Represents an application/service under monitoring (updated to reference `organization_id`).
- **UserSessionPreference**: Tracks user-level session state such as `last_active_organization_id`.

## Assumptions

- Users authenticate globally (e.g., standard email/password or OAuth) without selecting an organization on the login form.
- When a user logs in for the first time, if they have no existing organization memberships, a default personal organization is automatically provisioned for them.
- Existing single-project records in the system will be migrated under default or newly specified organization entities.
