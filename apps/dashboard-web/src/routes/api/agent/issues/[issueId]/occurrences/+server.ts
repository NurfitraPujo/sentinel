import { json, error } from '@sveltejs/kit';
import { withAgentIssue } from '$lib/server/agent-route';
import { getAgentOccurrencesByIds, listAgentOccurrences } from '$lib/db/queries/agent-reads';
import { annotateSpans } from '$lib/server/agent-spans';
import { walkOccurrencesWithSpans } from '$lib/server/issue-spans';
import { resolveAgentSpanScope } from '$lib/server/span-scope';

// N1c (agent read endpoints). GET /api/agent/issues/[issueId]/occurrences?limit=&before=&hasSpans=
// Newest-first page of occurrences. `limit` clamps to [1, 50] (default 20) inside
// listAgentOccurrences; `before` is an ISO timestamp cursor for paging further back. Read-only.
//
// sentinel-occurrence-spans TASK-01: every occurrence carries `spansStatus` (available | pending | none, null with no
// canonical trace id) and `hasSpans`. `hasSpans=true` keeps only occurrences with spans: a walk bounded to the issue's
// newest 2,000 occurrences (see issue-spans.ts), so the response says what it looked at (`spansScan`) and, when older
// occurrences may remain, where to continue (`nextBefore` -> `before`).

export const GET = withAgentIssue(async (ctx, issue, event) => {
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

	const hasSpansParam = event.url.searchParams.get('hasSpans');
	if (hasSpansParam !== null && hasSpansParam !== 'true' && hasSpansParam !== 'false') {
		throw error(400, 'hasSpans must be true or false');
	}

	const scope = await resolveAgentSpanScope(ctx.organizationId);

	if (hasSpansParam === 'true') {
		const want = Math.min(Math.max(limit ?? 20, 1), 50);
		const walk = await walkOccurrencesWithSpans({ issueId: issue.issueId, scope, want, before });
		const rows = await getAgentOccurrencesByIds(issue.issueId, walk.ids);
		const occurrences = rows.map((r) => ({ ...r, spansStatus: 'available' as const, hasSpans: true }));
		return {
			response: json({
				occurrences,
				spansScan: { scanned: walk.scanned, exhausted: walk.exhausted },
				...(walk.continueBefore ? { nextBefore: walk.continueBefore.toISOString() } : {}),
			}),
		};
	}

	const occurrences = await annotateSpans(await listAgentOccurrences({ issueId: issue.issueId, limit, before }), scope);

	return {
		response: json({ occurrences }),
	};
});
