import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { projects, organizationMembers } from '$lib/db/schema';
import { batchUpdateIssues } from '$lib/db/queries/issues';
import { eq, and } from 'drizzle-orm';

export const POST: RequestHandler = async ({ request, params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const { projectId } = params;

	// Validate user has access to the project's org and correct role
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

	const { userRole } = projectWithOrg[0];
	const allowedRoles = ['owner', 'admin', 'engineer', 'support'];
	if (!userRole || !allowedRoles.includes(userRole)) {
		throw error(403, 'Forbidden: Insufficient permissions to perform bulk updates');
	}

	const body = await request.json();
	const { action, issueIds, resolvedInVersion, assigneeType, assignedTo } = body;

	if (!action || !issueIds || !Array.isArray(issueIds) || issueIds.length === 0) {
		throw error(400, 'Invalid request body: action and non-empty issueIds array are required');
	}

	if (!['resolve', 'ignore', 'unresolve', 'assign'].includes(action)) {
		throw error(400, 'Invalid action');
	}

	if (action === 'assign' && (!assigneeType || !assignedTo)) {
		throw error(400, 'assigneeType and assignedTo are required for assign action');
	}

	const updatedCount = await batchUpdateIssues(
		projectId,
		action,
		issueIds,
		{
			resolvedInVersion,
			assigneeType,
			assignedTo,
			actorType: 'user',
			actorId: userId
		}
	);

	return json({ success: true, updated: updatedCount });
};
