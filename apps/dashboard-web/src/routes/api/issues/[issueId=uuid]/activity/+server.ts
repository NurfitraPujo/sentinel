import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { InvalidActivityCursorError, listIssueActivityPage } from '$lib/db/queries/issue-activity';
import { parseActivityLimit } from '$lib/issue-activity';
import { requireIssueAccess } from '$lib/server/issue-access';
import { redactLinkedEntriesForUser } from '$lib/server/issue-activity-redaction';

// Manual Issues M1 (design §6): issue_activity is the single user-visible timeline for BOTH
// issue types, so this route uses the general `requireIssueAccess` (not report-access) — an org
// viewer can read a service issue's activity today via the issue detail page, and this endpoint
// must not become a narrower gate than that for either issue type.
//
// sentinel-issue-activity TASK-01: GET ?limit=&before= — one page of the history, newest first, `occurrence_burst` runs
// collapsed (issue-activity.ts documents the order, cursor and collapse rule). The issue page's Activity section reads
// it when opened, so the page's own load does not. `requireIssueAccess(…, 'read')` is exactly the issue page's rule
// (the project dimension, then the environment dimension as a 404), so there is one access model. The response used to
// be `{ activity: <every row> }`, read by nothing in this repository; it is now `{ entries, nextCursor, hasMore }`.

export const GET: RequestHandler = async ({ params, locals, url }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const { issueId } = params;

	const limit = parseActivityLimit(url.searchParams.get('limit'));
	if (limit === null) {
		throw error(400, 'limit must be a positive integer');
	}

	const { organizationId } = await requireIssueAccess(userId, issueId, 'read');

	try {
		const page = await listIssueActivityPage({ issueId, organizationId, limit, before: url.searchParams.get('before') });
		return json({ ...page, entries: await redactLinkedEntriesForUser(userId, organizationId, page.entries) });
	} catch (err) {
		if (err instanceof InvalidActivityCursorError) throw error(400, err.message);
		throw err;
	}
};
