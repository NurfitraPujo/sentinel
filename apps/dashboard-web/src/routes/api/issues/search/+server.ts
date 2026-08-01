import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { issues, projects, organizationMembers } from '$lib/db/schema';
import { searchIssuesInOrg } from '$lib/db/queries/issues';
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

	// Validate org access
	const orgMember = await db
		.select()
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

	const searchResults = await searchIssuesInOrg(targetOrgId, query, currentIssueId);

	return json({ issues: searchResults });
};
