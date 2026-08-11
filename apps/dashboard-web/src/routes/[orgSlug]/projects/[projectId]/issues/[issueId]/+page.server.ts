import type { PageServerLoad } from './$types';
import { error } from '@sveltejs/kit';
import { checkProjectAccess } from '$lib/server/projects';
import { issueQueries } from '$lib/server/queries/issue-queries';
import { getIssueRelations } from '$lib/db/queries/issues';
import { getOrgRole } from '$lib/server/report-access';

// D03: this route previously had no loader at all, so `data` was undefined and the page fell
// through to hardcoded mock data (currentIssueId = 'ISSUE-123'). Every mutation then carried a
// non-UUID id and failed at the DB layer — mirrors the working legacy loader at
// src/routes/issues/[id]/+page.server.ts, but scoped under the org/project path and re-verifying
// that the issue actually belongs to :projectId (a bare issueId lookup alone would let a caller
// view any issue by guessing/crawling ids as long as they had access to SOME project).
export const load: PageServerLoad = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { projectId, issueId } = params;

	const issue = await issueQueries.getIssueById(issueId);

	if (!issue || issue.projectId !== projectId) {
		throw error(404, 'Issue not found');
	}

	const isAuthorized = await checkProjectAccess(session.user.id, projectId, 'viewer');
	if (!isAuthorized) {
		throw error(403, 'Forbidden');
	}

	const project = await issueQueries.getProjectById(projectId);
	const occurrences = await issueQueries.getOccurrencesByIssueId(issueId);
	const relations = await getIssueRelations(issueId);

	// Manual Issues M3 (design §5): the thread mounts here too -- "discussion on service issues is
	// the deliberate by-product" -- so the loader needs organizationId (for uploads) and the
	// caller's org role (for the thread's owner/admin delete-others rule), same shape as the
	// reports detail loader.
	const organizationId = project?.organizationId ?? null;
	const userRole = organizationId ? await getOrgRole(session.user.id, organizationId) : null;

	return {
		issue,
		project: project ?? null,
		occurrences,
		relations,
		organizationId,
		userId: session.user.id,
		userRole,
	};
};
