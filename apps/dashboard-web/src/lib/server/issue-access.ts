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
 * The two are resolved as **OR, not AND** (see DECISIONS.md D17). An org role on
 * `ISSUE_WRITE_ROLES` is itself an org-wide grant covering every project in the org — no
 * `project_members` row is required or consulted. Project membership is the ALTERNATIVE path for a
 * caller whose org role does not grant access (`viewer`), and it conveys READ only.
 *
 * An earlier revision of this helper required BOTH, and this comment described that. It was wrong:
 * tests/e2e U13 failed because an org admin could not link two issues in their own organization,
 * since `project_members` is populated only for per-project grants and never for org-level staff.
 * D10 stays closed either way — an org `viewer` with no project membership is still refused.
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

	const { organizationId: orgId } = await requireProjectAccess(userId, projectId, permission, {
		organizationId,
	});

	return { issueId: issueRows[0].issueId, projectId, organizationId: orgId };
}

/**
 * The project-scoped half of `requireIssueAccess`, callable on its own when the caller has a
 * projectId but no single issue id — the bulk endpoint
 * (`api/projects/[projectId]/issues/batch`) being the case that matters.
 *
 * Extracted so the bulk and single-issue write paths cannot drift apart again. They previously
 * disagreed in BOTH directions: the single-issue path had no role gate at all (D10), and once
 * that was fixed the bulk path was left as the more permissive of the two (D23) because it checked
 * only `organization_members` and never project membership — so a user in the org but not on the
 * project could bulk-resolve that project's issues while being refused a single one.
 *
 * `knownOrganizationId` lets `requireIssueAccess` pass the org it already resolved via the issue
 * join, avoiding a second lookup; omit it and the project's organization is fetched here.
 */
export async function requireProjectAccess(
	userId: string,
	projectId: string,
	permission: IssuePermission,
	opts?: { organizationId?: string }
): Promise<{ projectId: string; organizationId: string }> {
	let organizationId = opts?.organizationId;

	if (!organizationId) {
		const projectRows = await db
			.select({ organizationId: projects.organizationId })
			.from(projects)
			.where(eq(projects.id, projectId));

		if (projectRows.length === 0) {
			throw error(404, 'Project not found');
		}
		if (!projectRows[0].organizationId) {
			throw error(400, 'Project does not belong to an organization');
		}
		organizationId = projectRows[0].organizationId;
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

	const orgRole = orgMemberRows[0].role;
	const orgRoleGrantsAccess = isIssueWriteRole(orgRole);

	// An organization-level role IS an org-wide grant: owner/admin/engineer/support administer every
	// project in their org and are not expected to also hold a `project_members` row for each one.
	// Requiring both broke exactly that (tests/e2e U13: an org admin could not link two issues),
	// because in practice `project_members` is populated only for per-project grants.
	//
	// Project membership is therefore the ALTERNATIVE path, not an additional hurdle — it is how a
	// user whose org role does not itself grant issue access (`viewer`) gets access to the specific
	// projects they were added to. This still closes D10: an org `viewer` with no project membership
	// is refused, and cannot see or mutate a project they are not on.
	if (!orgRoleGrantsAccess) {
		const projectMemberRows = await db
			.select({ role: projectMembers.role })
			.from(projectMembers)
			.where(and(eq(projectMembers.userId, userId), eq(projectMembers.projectId, projectId)));

		if (projectMemberRows.length === 0) {
			throw error(403, 'Forbidden: You do not have access to this project');
		}

		// A project grant conveys read only. Writing still requires an org role on the write
		// allowlist, so the single-issue path can never be more permissive than the bulk one.
		if (permission === 'write') {
			throw error(403, 'Forbidden: Insufficient permissions to modify these issues');
		}
	}

	return { projectId, organizationId };
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
