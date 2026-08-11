import { error } from '@sveltejs/kit';
import { eq } from 'drizzle-orm';
import type { PageServerLoad } from './$types';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';
import { getReportDetail, getIssueActivity, listIssueAttachments } from '$lib/db/queries/reports';
import { getIssueRelations } from '$lib/db/queries/issues';
import { requireReportAccessForIssue } from '$lib/server/report-access';
import { ISSUE_WRITE_ROLES } from '$lib/server/issue-access';

// Manual Issues M1 (design §10): imitates the project-issue detail loader pattern at
// [orgSlug]/projects/[projectId]/issues/[issueId]/+page.server.ts -- verify the issue belongs to
// this org (report-access.ts's requireReportAccessForIssue does both the 404-for-wrong-type and
// the org-membership check in one call, see B7/§9), then load everything the page renders.
export const load: PageServerLoad = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { issueId, orgSlug } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	const userId = session.user.id;
	const { organizationId, role } = await requireReportAccessForIssue(userId, issueId, 'read');

	// requireReportAccessForIssue resolves organizationId from the issue itself, but the route is
	// also parameterized by :orgSlug -- reject a mismatch rather than silently serving the report
	// under whichever org slug happens to be in the URL (mirrors the projectId cross-check on the
	// existing issue detail loader).
	const currentOrg = locals.currentOrg;
	if (!currentOrg || currentOrg.slug !== orgSlug || currentOrg.id !== organizationId) {
		throw error(403, 'Forbidden: Unauthorized access to organization');
	}

	const detail = await getReportDetail(issueId);
	if (!detail) {
		throw error(404, 'Report not found');
	}

	const [relations, activity, orgProjects, attachments] = await Promise.all([
		getIssueRelations(issueId),
		getIssueActivity(issueId),
		db
			.select({ id: projects.id, name: projects.name, isInbox: projects.isInbox })
			.from(projects)
			.where(eq(projects.organizationId, organizationId)),
		listIssueAttachments(issueId),
	]);

	const canWrite = (ISSUE_WRITE_ROLES as readonly string[]).includes(role);

	return {
		orgId: organizationId,
		orgSlug,
		userId,
		canWrite,
		detail,
		relations,
		activity,
		attachments,
		projects: orgProjects,
	};
};
