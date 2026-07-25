import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/db';
import { issues, projects, organizationMembers, issueRelations } from '$lib/db/schema';
import { eq, and, or } from 'drizzle-orm';

export const POST: RequestHandler = async ({ request, params, locals }) => {
	const session = await locals.getSession();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const sourceIssueId = params.issueId;

	const body = await request.json();
	const { targetIssueId, relationType = 'related' } = body;

	if (!targetIssueId) {
		throw error(400, 'targetIssueId is required');
	}

	// Fetch both issues and their projects to verify organization
	const sourceIssueQuery = await db
		.select({
			issueId: issues.id,
			orgId: projects.organizationId,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(eq(issues.id, sourceIssueId));

	const targetIssueQuery = await db
		.select({
			issueId: issues.id,
			orgId: projects.organizationId,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(eq(issues.id, targetIssueId));

	if (sourceIssueQuery.length === 0 || targetIssueQuery.length === 0) {
		throw error(404, 'One or both issues not found');
	}

	const sourceOrgId = sourceIssueQuery[0].orgId;
	const targetOrgId = targetIssueQuery[0].orgId;

	if (!sourceOrgId || sourceOrgId !== targetOrgId) {
		throw error(400, 'Both issues must belong to the same organization');
	}

	// Validate user has access to this organization
	const orgMember = await db
		.select()
		.from(organizationMembers)
		.where(
			and(
				eq(organizationMembers.userId, userId),
				eq(organizationMembers.organizationId, sourceOrgId)
			)
		);

	if (orgMember.length === 0) {
		throw error(403, 'Forbidden: You do not have access to this organization');
	}

	// Create the relation
	const [relation] = await db
		.insert(issueRelations)
		.values({
			sourceIssueId,
			targetIssueId,
			relationType,
		})
		.returning();

	return json(relation, { status: 201 });
};
