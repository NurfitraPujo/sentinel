---
module: dashboard-web
date: 2026-08-01
problem_type: best_practice
component: frontend_stimulus
severity: medium
applies_when:
  - "Building issue detail views with linked issues, parent/child dependencies, or duplicate error tracking"
  - "Implementing client-side search autocomplete for entity linking across tenant bounds"
symptoms:
  - "No UI component or view rendered backend issue relations despite backend API support"
  - "Autocomplete search inputs missing input validation or escaping special SQL wildcard characters"
related_components:
  - database
  - authentication
tags:
  - svelte5
  - issue-relations
  - autocomplete
  - bi-directional-linking
  - sql-sanitization
---

# Issue Relations & Duplication UI Component Pattern

## Context

The backend API `/api/issues/[issueId]/relations` (`GET`, `POST`, `DELETE`) and database table `issue_relations` supported issue relationship types (`linked_to`, `caused_by`, `duplicate_of`). However, the frontend dashboard lacked an interactive UI component to render or manage these relations during incident triage.

Building high-density issue relation components requires addressing bi-directional relationship fetching, secure real-time search autocomplete, and seamless resolution workflows.

## Guidance

### 1. Bi-Directional Relation Fetching in Data Layer

When querying issue relationships, query both outgoing (`sourceIssueId = currentIssue`) and incoming (`targetIssueId = currentIssue`) records in a single database function. Perform an `INNER JOIN` on the `issues` table to fetch target issue metadata (`id`, `errorClass`, `message`, `status`, `fingerprint`) in one round-trip to avoid client-side N+1 queries.

```typescript
export async function getIssueRelations(issueId: string) {
	const outgoing = await db
		.select({
			id: issueRelations.id,
			sourceIssueId: issueRelations.sourceIssueId,
			targetIssueId: issueRelations.targetIssueId,
			relationType: issueRelations.relationType,
			direction: sql<'outgoing' | 'incoming'>`'outgoing'`,
			targetIssue: {
				id: issues.id,
				errorClass: issues.errorClass,
				message: issues.message,
				status: issues.status,
				fingerprint: issues.fingerprint,
			},
		})
		.from(issueRelations)
		.innerJoin(issues, eq(issues.id, issueRelations.targetIssueId))
		.where(eq(issueRelations.sourceIssueId, issueId));

	const incoming = await db
		.select({
			id: issueRelations.id,
			sourceIssueId: issueRelations.sourceIssueId,
			targetIssueId: issueRelations.targetIssueId,
			relationType: issueRelations.relationType,
			direction: sql<'incoming' | 'outgoing'>`'incoming'`,
			targetIssue: {
				id: issues.id,
				errorClass: issues.errorClass,
				message: issues.message,
				status: issues.status,
				fingerprint: issues.fingerprint,
			},
		})
		.from(issueRelations)
		.innerJoin(issues, eq(issues.id, issueRelations.sourceIssueId))
		.where(eq(issueRelations.targetIssueId, issueId));

	return [...outgoing, ...incoming];
}
```

### 2. Secure Real-Time Autocomplete Search

When exposing live search autocomplete for linking entities across an organization:
- **Enforce Minimum Query Length**: Require `query.length >= 2` before querying the database to eliminate expensive single-character full-table scans.
- **Sanitize SQL Wildcards**: Escape `%`, `_`, and `\` characters in search queries prior to interpolating into `ILIKE` clauses.
- **Tenant Scope Enforcement**: Always derive the tenant context (`organizationId`) from authenticated session credentials or verified resource ownership rather than unverified client inputs.

```typescript
export async function searchIssuesInOrg(orgId: string, query: string, excludeIssueId?: string) {
	const sanitized = query.trim().replace(/[%_\\]/g, '\\$&');
	const searchTerm = `%${sanitized}%`;

	return await db
		.select({
			id: issues.id,
			errorClass: issues.errorClass,
			message: issues.message,
			status: issues.status,
			fingerprint: issues.fingerprint,
			projectId: issues.projectId,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(
			and(
				eq(projects.organizationId, orgId),
				excludeIssueId ? sql`${issues.id} != ${excludeIssueId}` : sql`1=1`,
				sql`(${issues.id} ILIKE ${searchTerm} OR ${issues.errorClass} ILIKE ${searchTerm} OR ${issues.message} ILIKE ${searchTerm} OR ${issues.fingerprint} ILIKE ${searchTerm})`
			)
		)
		.limit(10);
}
```

### 3. Interactive Component Architecture & Smart Helpers

Design `IssueRelations.svelte` with:
- **Categorized Visual Groups**: Group relations into distinct sections with counts (**Duplicates**, **Causes & Blockers**, **Related Issues**) using the backend ENUM values (`duplicate_of`, `caused_by`, `linked_to`).
- **Optimistic Unlinking**: Immediately update local component state when unlinking, rolling back seamlessly if the server DELETE request fails.
- **Resolution Helper**: When a user links an issue as a `duplicate_of` a target issue that is already resolved, prompt the user with a one-click action to resolve the current issue as well.

## Why This Matters

1. **High Information Density**: SREs and developers triaging outages require instant scannability of root causes (`caused_by`) and duplicate suppression without full-page navigation.
2. **Database Performance & Security**: Sanitizing `ILIKE` queries and requiring minimum input length prevents DoS-shaped database load during live autocomplete typing.
3. **Data Integrity**: Bi-directional relation queries ensure both outgoing and incoming duplicate links are visible regardless of which issue detail page is viewed.

## When to Apply

Apply this pattern whenever building:
- Relationship or dependency linking UI components (e.g. issues, tickets, alerts).
- Real-time entity search inputs backed by SQL database search.
- Context-preserving sidebars and detail panel drawers in observability systems.

## Examples

### Svelte 5 Duplicate Resolution Helper Prompt

```svelte
{#if promptResolveTarget}
	<div class="resolve-prompt">
		<p class="prompt-text">
			Linked as duplicate of resolved issue <strong class="mono">{promptResolveTarget.id}</strong>. Mark this issue as resolved too?
		</p>
		<div class="prompt-actions">
			<button class="btn-resolve-confirm" onclick={handleResolvePromptConfirm}>Mark Resolved</button>
			<button class="btn-resolve-dismiss" onclick={() => (promptResolveTarget = null)}>Dismiss</button>
		</div>
	</div>
{/if}
```
