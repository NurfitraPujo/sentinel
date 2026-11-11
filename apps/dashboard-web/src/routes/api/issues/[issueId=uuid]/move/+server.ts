import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';
import { eq } from 'drizzle-orm';
import { moveIssueToProject } from '$lib/db/queries/reports';
import { requireReportAccessForIssue } from '$lib/server/report-access';

// Manual Issues M1 (design §2/§10): write-role "move to project" action.

export const POST: RequestHandler = async ({ params, request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	const { organizationId } = await requireReportAccessForIssue(userId, issueId, 'write');

	const body = await request.json().catch(() => ({}));
	const { projectId } = body;

	if (typeof projectId !== 'string' || projectId.trim().length === 0) {
		throw error(400, 'projectId is required');
	}

	// The target must belong to the SAME organization — moving a report cross-org would be a
	// tenant-boundary violation (B7).
	const targetRows = await db
		.select({ id: projects.id, organizationId: projects.organizationId })
		.from(projects)
		.where(eq(projects.id, projectId));

	if (targetRows.length === 0 || targetRows[0].organizationId !== organizationId) {
		throw error(400, 'projectId does not belong to this organization');
	}

	const result = await moveIssueToProject(issueId, projectId, 'user', userId);

	return json({ success: true, ...result });
};
