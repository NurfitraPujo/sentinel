import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { issues, projects, organizationMembers } from '$lib/db/schema';
import { searchIssuesInOrg } from '$lib/db/queries/issues';
import { getAccessibleProjects } from '$lib/server/issue-access';
import { resolveEnvironmentScopes } from '$lib/server/environment-access';
import { hasPermission, isKnownRole } from '$lib/rbac';
import { eq, and } from 'drizzle-orm';

export const GET: RequestHandler = async ({ url, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const query = url.searchParams.get('q')?.trim() || '';
	const currentIssueId = url.searchParams.get('issueId') || undefined;

	if (query.length < 2) {
		return json({ issues: [] });
	}

	// If currentIssueId is provided, get org from that issue
	let targetOrgId: string | null = null;

	if (currentIssueId) {
		const sourceQuery = await db
			.select({ orgId: projects.organizationId })
			.from(issues)
			.innerJoin(projects, eq(projects.id, issues.projectId))
			.where(eq(issues.id, currentIssueId));

		if (sourceQuery.length > 0) {
			targetOrgId = sourceQuery[0].orgId;
		}
	}

	if (!targetOrgId) {
		// Fallback: check first organization member entry for user
		const userOrgs = await db
			.select({ orgId: organizationMembers.organizationId })
			.from(organizationMembers)
			.where(eq(organizationMembers.userId, userId))
			.limit(1);

		if (userOrgs.length > 0) {
			targetOrgId = userOrgs[0].orgId;
		}
	}

	if (!targetOrgId) {
		throw error(403, 'Forbidden: No valid organization context');
	}

	// Validate org access + role. Search is a read operation, so any recognized org role is
	// sufficient (D10 item 1) — but the role must be one rbac.ts actually recognizes.
	const orgMember = await db
		.select({ role: organizationMembers.role })
		.from(organizationMembers)
		.where(
			and(
				eq(organizationMembers.userId, userId),
				eq(organizationMembers.organizationId, targetOrgId)
			)
		);

	if (orgMember.length === 0) {
		throw error(403, 'Forbidden: You do not have access to this organization');
	}

	const role = orgMember[0].role;
	if (!isKnownRole(role) || !hasPermission(role, 'read')) {
		throw error(403, 'Forbidden: Insufficient permissions to search issues');
	}

	// Manual Issues M1 (§9, Q9): this is the error dashboard's search bar, so it is hard-locked to
	// system_error — a caller cannot widen it via a query param (there isn't one).
	//
	// (An earlier comment here claimed the linked-issues panel calls `searchIssuesInOrg` directly
	// without this filter. That is STALE — verified at `a68a99f` and again here: this route is
	// `searchIssuesInOrg`'s only production caller, so there is no second caller to build a guard
	// for.)
	//
	// sentinel-project-environments §2.2, decision 25: the query carries the caller's environment
	// scope, resolved per project. It is org-wide with no project filter of its own, so without this
	// a narrowed member reads every environment of every project in the organization.
	const environmentScopes = await resolveEnvironmentScopes(userId);
	const searchResults = await searchIssuesInOrg(
		targetOrgId,
		query,
		currentIssueId,
		'system_error',
		environmentScopes
	);

	// D10: searchIssuesInOrg has no project filter, so scope results to projects the caller may
	// reach — an org member who is not on a given project must not see its issues.
	//
	// sentinel-project-environments decision 45: this call site WIDENS from `getAccessibleProjectIds`
	// (`project_members`, which nothing in this application writes, so the set was empty for every
	// user) to `project_members` ∪ `organization_members` in `ISSUE_WRITE_ROLES`. It has to: the
	// filter below returned `[]` for everybody, so the new predicate above would be unobservable —
	// including to any test written at this boundary, and a guard nobody can see fire is not a guard.
	// **Only this call site.** `getAccessibleProjectIds`'s other caller,
	// `routes/api/organizations/[orgId]/reports/+server.ts`, is left exactly as it is: a `viewer`
	// sees zero reports today and still does, and changing that is an access decision nobody took
	// (contract §12 *Known limits* 8).
	const accessibleProjectIds = new Set(
		(await getAccessibleProjects(userId, targetOrgId)).map((p) => p.id)
	);
	const scopedResults = searchResults.filter((issue) => accessibleProjectIds.has(issue.projectId));

	return json({ issues: scopedResults });
};
