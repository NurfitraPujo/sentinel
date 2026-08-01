import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { issues, projects, organizationMembers } from '$lib/db/schema';
import { updateIssueStatus } from '$lib/db/queries/issues';
import { eq, and } from 'drizzle-orm';

const VALID_STATUSES = ['unresolved', 'resolved', 'ignored'] as const;
type IssueStatus = (typeof VALID_STATUSES)[number];

function isValidStatus(value: unknown): value is IssueStatus {
	return typeof value === 'string' && (VALID_STATUSES as readonly string[]).includes(value);
}

export const PATCH: RequestHandler = async ({ request, params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const issueId = params.issueId;

	const body = await request.json().catch(() => ({}));
	const { status, resolvedInVersion } = body;

	if (!isValidStatus(status)) {
		throw error(400, `status must be one of: ${VALID_STATUSES.join(', ')}`);
	}

	const sourceIssueQuery = await db
		.select({
			issueId: issues.id,
			orgId: projects.organizationId,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(eq(issues.id, issueId));

	if (sourceIssueQuery.length === 0) {
		throw error(404, 'Issue not found');
	}

	const sourceOrgId = sourceIssueQuery[0].orgId;

	if (!sourceOrgId) {
		throw error(400, 'Issue project does not belong to an organization');
	}

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

	await updateIssueStatus(issueId, status, resolvedInVersion, 'user', userId);

	return json({ success: true, status });
};
