# Security Constraints & Verification Requirements

## Trust Boundaries & Authentication
- **Global Auth**: Authentication validates global user identity. User tokens MUST NOT lock user to a single organization unless verified against `organization_members`.
- **Session State**: Session context MUST track active `organization_id` and validate active membership on every request.

## Authorization & Access Control
- **Organization RBAC**: Enforce organization role permissions (`owner`, `admin`, `engineer`, `support`, `viewer`) for all org-level actions (inviting members, changing roles, modifying organization settings).
- **Project Role Resolution**: Role evaluation MUST first check `project_members` for a explicit project override; if absent, fall back to `organization_members.role`.
- **Revocation Safety**: Removing a user from `organization_members` MUST immediately invalidate access to all associated projects under that organization.

## Multi-Tenant Isolation
- **Query Scoping**: 100% of queries fetching projects, issues, error occurrences, or alert configurations MUST include `WHERE organization_id = :active_org_id` (or filter via project ownership under the active org).
- **Invitation Tokens**: Organization invitation tokens MUST be cryptographically random (min 64 hex characters) and expire after 7 days.
