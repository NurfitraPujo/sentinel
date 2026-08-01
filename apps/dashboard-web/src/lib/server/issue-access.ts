import { error } from '@sveltejs/kit';
import { and, eq } from 'drizzle-orm';
import { db } from './db';
import { issues, projects, organizationMembers, projectMembers } from '$lib/db/schema';

/**
 * Org roles allowed to perform WRITE operations on issues (resolve/ignore/unresolve/assign).
 * Mirrors the allowlist enforced by the bulk path
 * (apps/dashboard-web/src/routes/api/projects/[projectId]/issues/batch/+server.ts:33) so a
 * single-issue action can never be more permissive than the bulk one. D10/D23.
 */
export const ISSUE_WRITE_ROLES = ['owner', 'admin', 'engineer', 'support'] as const;
type IssueWriteRole = (typeof ISSUE_WRITE_ROLES)[number];

function isIssueWriteRole(role: string): role is IssueWriteRole {
	return (ISSUE_WRITE_ROLES as readonly string[]).includes(role);
}

export type IssuePermission = 'read' | 'write';

export interface IssueAccessContext {
	issueId: string;
	projectId: string;
	organizationId: string;
}

/**
 * Single place that decides whether `userId` may act on `issueId` at `permission` level.
 *
 * Two membership models coexist in this codebase (see docs/plans/UI_PARITY_REMEDIATION_PLAN.md
 * P1-6): page loads check `project_members` via `checkProjectAccess`
 * (lib/server/projects.ts), while these API routes historically checked only
 * `organization_members`. That divergence let an org `viewer` — who cannot bulk-resolve via
 * `projects/[projectId]/issues/batch` — resolve issues one at a time (D10), and it meant an org
 * member with no project membership at all could still act on any project's issues.
 *
 * This helper checks BOTH: org role gates WRITE using the same allowlist the bulk endpoint
 * enforces, and project membership is required unconditionally (for both `read` and `write`) so
 * visibility of a specific project's issues always requires being on that project.
 *
 * Throws a SvelteKit `error()` (404/400/403) — call sites should let it propagate.
 */
export async function requireIssueAccess(
	userId: string,
	issueId: string,
	permission: IssuePermission
): Promise<IssueAccessContext> {
	const issueRows = await db
		.select({
			issueId: issues.id,
			projectId: issues.projectId,
			organizationId: projects.organizationId,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(eq(issues.id, issueId));

	if (issueRows.length === 0) {
		throw error(404, 'Issue not found');
	}

	const { projectId, organizationId } = issueRows[0];

	if (!organizationId) {
		throw error(400, 'Issue project does not belong to an organization');
	}

	const orgMemberRows = await db
		.select({ role: organizationMembers.role })
		.from(organizationMembers)
		.where(
			and(
				eq(organizationMembers.userId, userId),
				eq(organizationMembers.organizationId, organizationId)
			)
		);

	if (orgMemberRows.length === 0) {
		throw error(403, 'Forbidden: You do not have access to this organization');
	}

	if (permission === 'write' && !isIssueWriteRole(orgMemberRows[0].role)) {
		throw error(403, 'Forbidden: Insufficient permissions to modify this issue');
	}

	const projectMemberRows = await db
		.select({ role: projectMembers.role })
		.from(projectMembers)
		.where(and(eq(projectMembers.userId, userId), eq(projectMembers.projectId, projectId)));

	if (projectMemberRows.length === 0) {
		throw error(403, 'Forbidden: You do not have access to this project');
	}

	return { issueId: issueRows[0].issueId, projectId, organizationId };
}

/**
 * Project ids within `organizationId` that `userId` has a `project_members` row for.
 *
 * `searchIssuesInOrg` (lib/db/queries/issues.ts) is org-wide with no project filter, so callers
 * use this to scope results down to projects the caller actually belongs to — otherwise an org
 * member who is not on a given project could see that project's issues via search.
 */
export async function getAccessibleProjectIds(
	userId: string,
	organizationId: string
): Promise<Set<string>> {
	const rows = await db
		.select({ projectId: projectMembers.projectId })
		.from(projectMembers)
		.innerJoin(projects, eq(projects.id, projectMembers.projectId))
		.where(and(eq(projectMembers.userId, userId), eq(projects.organizationId, organizationId)));

	return new Set(rows.map((r) => r.projectId));
}

// Matches issues.resolved_in_version VARCHAR(100)
// (packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql:38).
const RESOLVED_IN_VERSION_MAX_LENGTH = 100;

/**
 * Validates a client-supplied `resolvedInVersion` instead of passing it through unchecked from
 * the request body — an oversized value previously reached the DB write and 500'd on the
 * `varchar(100)` constraint instead of 400ing (D10 item 3).
 *
 * Returns the trimmed value, or null if omitted/blank. Throws SvelteKit `error(400, ...)` for a
 * present-but-invalid value (wrong type or too long).
 */
export function validateResolvedInVersion(value: unknown): string | null {
	if (value === undefined || value === null) {
		return null;
	}
	if (typeof value !== 'string') {
		throw error(400, 'resolvedInVersion must be a string');
	}
	const trimmed = value.trim();
	if (trimmed.length === 0) {
		return null;
	}
	if (trimmed.length > RESOLVED_IN_VERSION_MAX_LENGTH) {
		throw error(
			400,
			`resolvedInVersion must be at most ${RESOLVED_IN_VERSION_MAX_LENGTH} characters`
		);
	}
	return trimmed;
}
