import { json, error } from '@sveltejs/kit';
import { withAgentIssue } from '$lib/server/agent-route';
import { listAgentOccurrences } from '$lib/db/queries/agent-reads';

// N1c (agent read endpoints). GET /api/agent/issues/[issueId]/occurrences?limit=&before=
// Newest-first page of occurrences. `limit` clamps to [1, 50] (default 20) inside
// listAgentOccurrences; `before` is an ISO timestamp cursor for paging further back. Read-only.

export const GET = withAgentIssue(async (_ctx, issue, event) => {
	const limitParam = event.url.searchParams.get('limit');
	let limit: number | undefined;
	if (limitParam !== null) {
		const parsed = Number(limitParam);
		if (!Number.isFinite(parsed) || !Number.isInteger(parsed) || parsed < 1) {
			throw error(400, 'limit must be a positive integer');
		}
		limit = parsed;
	}

	const beforeParam = event.url.searchParams.get('before');
	let before: Date | undefined;
	if (beforeParam !== null) {
		const parsed = new Date(beforeParam);
		if (Number.isNaN(parsed.getTime())) {
			throw error(400, 'before must be a valid ISO timestamp');
		}
		before = parsed;
	}

	const occurrences = await listAgentOccurrences({ issueId: issue.issueId, limit, before });

	return {
		response: json({ occurrences }),
	};
});
