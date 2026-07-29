import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { issues, projects, organizationMembers, errorOccurrences } from '$lib/db/schema';
import { eq, and, inArray } from 'drizzle-orm';

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
		})
		.from(projects)
		.leftJoin(organizationMembers, eq(organizationMembers.organizationId, projects.organizationId))
		.where(and(eq(projects.id, projectId), eq(organizationMembers.userId, userId)));

	if (projectWithOrg.length === 0) {
		throw error(403, 'Forbidden: You do not have access to this project');
	}

	// Multi-dimensional search query params
	const status = url.searchParams.get('status');
	const regressionStatus = url.searchParams.get('regression_status');
	const releaseVersion = url.searchParams.get('release_version');
	const assigneeType = url.searchParams.get('assignee_type');
	const assignedTo = url.searchParams.get('assigned_to');
	const issueType = url.searchParams.get('issue_type');
	
	const conditions = [eq(issues.projectId, projectId)];

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
	if (issueType) conditions.push(eq(issues.issueType, issueType));

	const filteredIssues = await db
		.select()
		.from(issues)
		.where(and(...conditions))
		.orderBy(issues.lastSeen);

	return json({
		issues: filteredIssues
	});
};
