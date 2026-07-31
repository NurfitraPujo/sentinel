# TODO 09: Organization-Wide Alert Configurations UI (P9-1)

## Priority: Medium
## Status: Pending

### Overview
The backend capability for organization-wide alert rules (`projectId = null`) is fully implemented and tested at the schema, API (`/api/alerts`), RBAC, and processor-resolution layers. However, `src/routes/settings/alerts/+page.svelte` and its loader only list and support **project-scoped configs**. An org-wide config can currently only be created by calling the API directly.

### Requirements
1. **Alert Settings Page Enhancement**:
   - Update `src/routes/settings/alerts/+page.server.ts` to load both project-scoped and organization-wide alert rules for the active organization.
   - Distinctly label rules in the UI table using the API's `scope: 'organization' | 'project'` field.
2. **Role-Based Action Controls**:
   - Only offer Organization-Wide rule creation to users holding the `manage_keys` role capability.
   - Prevent non-permitted users from editing or deleting org-wide rules in the UI.

### Affected Files
- `apps/dashboard-web/src/routes/settings/alerts/+page.svelte`
- `apps/dashboard-web/src/routes/settings/alerts/+page.server.ts`

### Acceptance Criteria
- Alerts settings page renders both project and organization-wide alert rules.
- Org-wide rules are visually distinguished with a badge (`[Org-Wide]`).
- Users without `manage_keys` role cannot create or edit org-wide alert rules.
