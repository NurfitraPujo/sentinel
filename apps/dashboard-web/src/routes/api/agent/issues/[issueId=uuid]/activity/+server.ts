import { json, error } from '@sveltejs/kit';
import { withAgentIssue } from '$lib/server/agent-route';
import { InvalidActivityCursorError, listIssueActivityPage } from '$lib/db/queries/issue-activity';
import { parseActivityLimit } from '$lib/issue-activity';
import { redactLinkedEventTargets } from '$lib/server/agent-relations';

// sentinel-issue-activity TASK-01. GET /api/agent/issues/[issueId]/activity?limit=&before= -- one issue's activity history,
// newest first, `occurrence_burst` runs collapsed into one entry (issue-activity.ts: order, cursor, collapse rule). The
// per-issue counterpart of GET /api/agent/events: each entry carries the feed item's fields (seq, eventType, actorType,
// actorId, oldValue, newValue, createdAt) plus `actor` (resolved name) and, by type, `comment` / `assignee` / `burst`.
//
// Authorized exactly like GET /api/agent/issues/[issueId] (withAgentIssue 'read'): an issue outside the key's
// organization -- or, for a personal key, outside its owner's live projects/environment grants -- is a 404. A `linked`
// entry's target that the personal key cannot see is redacted as on the events feed. Read-only, so unaudited.

export const GET = withAgentIssue('read', async (ctx, issue, event) => {
	const limit = parseActivityLimit(event.url.searchParams.get('limit'));
	if (limit === null) {
		throw error(400, 'limit must be a positive integer');
	}

	let page;
	try {
		page = await listIssueActivityPage({
			issueId: issue.issueId,
			organizationId: ctx.organizationId,
			limit,
			before: event.url.searchParams.get('before'),
		});
	} catch (err) {
		if (err instanceof InvalidActivityCursorError) throw error(400, err.message);
		throw err;
	}

	return {
		response: json({ ...page, entries: await redactLinkedEventTargets(ctx, page.entries) }),
	};
});
