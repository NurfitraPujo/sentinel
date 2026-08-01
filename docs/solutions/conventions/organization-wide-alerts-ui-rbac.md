---
title: Organization-Wide Alert Configurations UI and RBAC Implementation
date: 2026-08-01
category: docs/solutions/conventions/
module: dashboard-web
problem_type: convention
component: development_workflow
severity: medium
applies_when:
  - "Implementing organization-wide features alongside project-scoped UI views"
  - "Routing RBAC permission checks between project-scoped and organization-scoped roles"
  - "Handling JSONB target configuration fields in alert settings"
tags:
  - alerts
  - rbac
  - svelte5
  - organization-scope
  - channel-target
---

# Organization-Wide Alert Configurations UI and RBAC Implementation

## Context
Sentinel's backend fully supported organization-wide alert rules (`projectId = null`), but the frontend dashboard (`src/routes/settings/alerts`) was limited to project-scoped alert rules. Organization-wide alert creation and management required calling `/api/alerts` directly. Surface integration required rendering organization-wide alert rules alongside project-scoped ones, restricting org-wide mutations to users holding the `manage_keys` capability (`owner`, `admin`, `engineer`), and ensuring consistent JSONB channel configuration deserialization without cross-file drift.

## Guidance

### 1. Unified Scope Loading & Dual RBAC Resolution
When a settings page renders both project-scoped resources (keyed by `projectId`) and organization-wide resources (where `projectId` is `null` and `organizationId` matches user org memberships):
- Load memberships from both `projectMembers` and `organizationMembers`.
- Query `alertConfigs` matching `projectId IN (userProjects)` OR (`projectId IS NULL` AND `organizationId IN (userOrgs)`).
- Gate project-scoped actions on `hasPermission(projectRole, 'write')` (`admin`, `developer`).
- Gate organization-wide actions strictly on `hasPermission(orgRole, 'manage_keys')` (`owner`, `admin`, `engineer`).

### 2. Centralized Channel Target Deserialization
Alert channel configuration is stored as a JSONB blob (`channelConfig`). To prevent schema mapping drift across page loaders (`+page.server.ts`) and API routes (`/api/alerts/+server.ts`), extract mapping logic into a shared module:
- Define `CHANNEL_TARGET_KEY` (`email` -> `to`, `telegram` -> `chat_id`).
- Export a unified `channelTargetOf(channel, channelConfig)` helper that checks the per-channel key and falls back to legacy `target`.

### 3. Svelte 5 Hybrid Error & Scope UI Patterns
- **Segmented Scope Switcher**: Use a 2-button segmented toggle (`Project Alert` vs `Organization-Wide Alert`) that dynamically updates the target selection field. Lock org-wide selection with a visual indicator if `canManageOrgAlerts` is false.
- **Explicit Multi-Org Dropdown**: If a user belongs to multiple organizations (`userOrganizations.length > 1`), render an explicit `<select>` for `organizationId` rather than falling back to `userOrganizations[0]`.
- **Hybrid Error Strategy**: Render HTTP 400/422 validation errors inline directly under the corresponding form inputs (`fieldErrors.projectId`, `fieldErrors.channelTarget`). Render HTTP 403/500 system and permission errors via a dismissible toast banner.
- **Inline Deletion Confirmation**: Use inline table row confirmation buttons (`Confirm delete? Yes / No`) to avoid disruptive modal dialogs.

## Why This Matters
- **Security & Authorization**: Gating org-wide alerts on `manage_keys` prevents project-level developers or viewers from routing alerts across all projects in the organization.
- **Tenant Safety**: Explicit organization selection when a user belongs to multiple organizations prevents accidental cross-tenant alert configuration creation.
- **Maintainability**: Centralizing JSONB payload parsing in `$lib/alerts.ts` eliminates cross-file drift between server loaders and API handlers.

## When to Apply
- Creating UI settings for resources that support both project-level and organization-wide scopes.
- Mapping permissions for organization-scoped operations (`manage_keys`) vs project-scoped operations (`write`).
- Handling form validation with a hybrid inline/toast feedback strategy in Svelte 5.

## Examples

### Shared Channel Target Extraction (`apps/dashboard-web/src/lib/alerts.ts`)
```typescript
export const CHANNEL_TARGET_KEY: Record<string, string> = {
	email: 'to',
	telegram: 'chat_id',
};

export function channelTargetOf(channel: string, channelConfig: Record<string, unknown>): string {
	const key = CHANNEL_TARGET_KEY[channel];
	const candidates = [key ? channelConfig[key] : undefined, channelConfig.target];
	for (const value of candidates) {
		if (typeof value === 'string' && value !== '') {
			return value;
		}
	}
	return '';
}
```

### RBAC Permission Calculation (`apps/dashboard-web/src/routes/settings/alerts/+page.server.ts`)
```typescript
const canManageOrgAlerts = userOrgMemberships.some((m) =>
	hasPermission(m.role as OrgRole, 'manage_keys')
);

const editableAlertConfigs = alertConfigsList.filter((config) => {
	if (config.projectId === null) {
		const orgRole = orgRoleMap[config.organizationId];
		return orgRole ? hasPermission(orgRole, 'manage_keys') : false;
	}
	const role = projectRoleMap[config.projectId];
	return role ? hasPermission(role, 'write') : false;
});
```

## Related
- [`apps/dashboard-web/src/lib/alerts.ts`](file:///home/fitrapujo/oss/sentinel/apps/dashboard-web/src/lib/alerts.ts)
- [`apps/dashboard-web/src/routes/settings/alerts/+page.server.ts`](file:///home/fitrapujo/oss/sentinel/apps/dashboard-web/src/routes/settings/alerts/+page.server.ts)
- [`apps/dashboard-web/src/routes/settings/alerts/+page.svelte`](file:///home/fitrapujo/oss/sentinel/apps/dashboard-web/src/routes/settings/alerts/+page.svelte)
- [`apps/dashboard-web/src/routes/settings/alerts/+page.server.test.ts`](file:///home/fitrapujo/oss/sentinel/apps/dashboard-web/src/routes/settings/alerts/+page.server.test.ts)
