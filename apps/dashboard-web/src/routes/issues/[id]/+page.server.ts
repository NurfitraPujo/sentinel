import type { PageServerLoad } from './$types';
import { error } from '@sveltejs/kit';
import { checkProjectAccess } from '$lib/server/projects';
import { issueQueries } from '$lib/server/queries/issue-queries';

export const load: PageServerLoad = async ({ params, locals }) => {
	const session = await locals.getSession();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const issueId = params.id;
	const issue = await issueQueries.getIssueById(issueId);

	if (!issue) {
		throw error(404, 'Issue not found');
	}

	const isAuthorized = await checkProjectAccess(session.user.id, issue.projectId, 'viewer');
	if (!isAuthorized) {
		throw error(403, 'Forbidden');
	}

	const project = await issueQueries.getProjectById(issue.projectId);
	const occurrences = await issueQueries.getOccurrencesByIssueId(issueId);

	return {
		issue,
		project: project ?? null,
		occurrences,
	};
};