import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { getIssueActivity } from '$lib/db/queries/reports';
import { requireIssueAccess } from '$lib/server/issue-access';

// Manual Issues M1 (design §6): issue_activity is the single user-visible timeline for BOTH
// issue types, so this route uses the general `requireIssueAccess` (not report-access) — an org
// viewer can read a service issue's activity today via the issue detail page, and this endpoint
// must not become a narrower gate than that for either issue type.

export const GET: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	await requireIssueAccess(userId, issueId, 'read');

	const activity = await getIssueActivity(issueId);

	return json({ activity });
};
