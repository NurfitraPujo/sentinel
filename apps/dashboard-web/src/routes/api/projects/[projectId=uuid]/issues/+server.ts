import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { issues, projects, organizationMembers, errorOccurrences } from '$lib/db/schema';
import { eq, and, inArray } from 'drizzle-orm';
import { resolveEnvironmentScope } from '$lib/server/environment-access';

export const GET: RequestHandler = async ({ params, url, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const { projectId } = params;

	// Validate user has access to the project's org
	const projectWithOrg = await db
		.select({
			projectId: projects.id,
			orgId: projects.organizationId,
			userRole: organizationMembers.role,
			isInbox: projects.isInbox,
		})
		.from(projects)
		.leftJoin(organizationMembers, eq(organizationMembers.organizationId, projects.organizationId))
		.where(and(eq(projects.id, projectId), eq(organizationMembers.userId, userId)));

	if (projectWithOrg.length === 0) {
		throw error(403, 'Forbidden: You do not have access to this project');
	}

	// Manual Issues M1 (§9/§10): Triage inbox projects are excluded from the error dashboard
	// entirely, not just filtered by issue_type — they hold only user_report issues by
	// construction, so this always resolves to an empty (but successful) list.
	if (projectWithOrg[0].isInbox) {
		return json({ issues: [] });
	}

	// Multi-dimensional search query params
	const status = url.searchParams.get('status');
	const regressionStatus = url.searchParams.get('regression_status');
	const releaseVersion = url.searchParams.get('release_version');
	const assigneeType = url.searchParams.get('assignee_type');
	const assignedTo = url.searchParams.get('assigned_to');

	// Manual Issues M1 (design §9/§10, Q9): this is the error dashboard's issue listing, so it is
	// hard-locked to `issue_type='system_error'` — NOT taken from the request. A caller-supplied
	// `issue_type=user_report` would otherwise leak manual reports into the error dashboard,
	// exactly the "noise both ways" §9 exists to prevent. Manual reports have their own listing
	// (queries/reports.ts's listReports).
	const conditions = [eq(issues.projectId, projectId), eq(issues.issueType, 'system_error')];

	if (status) conditions.push(eq(issues.status, status));
	if (regressionStatus) conditions.push(eq(issues.regressionStatus, regressionStatus));
	if (releaseVersion) {
		// release_version lives on error_occurrences, not issues — match issues that have
		// at least one occurrence tagged with the requested release.
		const occurrenceIssueIds = db
			.select({ issueId: errorOccurrences.issueId })
			.from(errorOccurrences)
			.where(eq(errorOccurrences.releaseVersion, releaseVersion));
		conditions.push(inArray(issues.id, occurrenceIssueIds));
	}
	if (assigneeType) conditions.push(eq(issues.assigneeType, assigneeType));
	if (assignedTo) conditions.push(eq(issues.assignedTo, assignedTo));

	// sentinel-project-environments §2.2, decision 25 — the THIRD door to issue rows, separate from
	// `/`'s page loader and from `searchErrors`, reached by project id with an `organization_members`
	// join. A guard missing from any one of §2.2's call sites is the whole boundary.
	//
	// `null` is unrestricted (no grant row) and adds no predicate — the absence-means-all rule. Rows
	// outside the scope are simply not returned; there is no `403` to give, because a list does not
	// have a subject to deny (decision 20's `404` shape lives on the by-id paths).
	const scope = await resolveEnvironmentScope(userId, projectId);
	if (scope !== null) {
		conditions.push(inArray(issues.environment, scope));
	}

	const filteredIssues = await db
		.select()
		.from(issues)
		.where(and(...conditions))
		.orderBy(issues.lastSeen);

	return json({
		issues: filteredIssues
	});
};
