import { json } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import {
	listAgentIssues,
	decodeAgentIssuesCursor,
	AGENT_ISSUES_MAX_LIMIT,
	type AgentIssuesSort,
} from '$lib/db/queries/agent-work';

// Manual Issues M5 stage 2 (design §7 step 1). GET /api/agent/issues?type=&claimed=&project=&waiting=
// -- spans both issue types, scoped to the key's own org (B7). Manual-validation style, matching
// every other route in this feature.
//
// N7b (A02): adds since=&sort=&limit=&cursor= for bootstrap-friendly pagination. All four are
// OPTIONAL and, when entirely absent, the response is byte-identical to pre-N7b (no `nextCursor`
// field, no `.limit()` applied -- see agent-work.ts's listAgentIssues header).

const VALID_TYPES = ['user_report', 'system_error'] as const;
const VALID_SORTS: readonly AgentIssuesSort[] = ['firstSeen', 'lastSeen'];

export const GET: RequestHandler = async ({ request, url }) => {
	const ctx = await authenticateAgentRequest(request);

	const typeParam = url.searchParams.get('type');
	let type: 'user_report' | 'system_error' | undefined;
	if (typeParam !== null && typeParam !== 'any') {
		if (!(VALID_TYPES as readonly string[]).includes(typeParam)) {
			return json({ error: `type must be one of: any, ${VALID_TYPES.join(', ')}` }, { status: 400 });
		}
		type = typeParam as 'user_report' | 'system_error';
	}

	// N9 (AGENT_WORKER_PLAN C12): `claimed=me` restricts to THIS agent's own claims, resolved from
	// the credential (B7 -- never a request param), mirroring the events feed's claimed=me.
	const claimedParam = url.searchParams.get('claimed');
	let claimed: boolean | undefined;
	let claimedByAgentId: string | undefined;
	if (claimedParam === 'true') claimed = true;
	else if (claimedParam === 'false') claimed = false;
	else if (claimedParam === 'me') claimedByAgentId = ctx.agentId;
	else if (claimedParam !== null) {
		return json({ error: 'claimed must be "true", "false", or "me"' }, { status: 400 });
	}

	const projectId = url.searchParams.get('project') || undefined;

	const waitingParam = url.searchParams.get('waiting');
	let waiting: boolean | undefined;
	if (waitingParam === 'true') waiting = true;
	else if (waitingParam !== null && waitingParam !== 'false') {
		return json({ error: 'waiting must be "true" or "false"' }, { status: 400 });
	}

	const sinceParam = url.searchParams.get('since');
	let since: Date | undefined;
	if (sinceParam !== null) {
		const parsed = new Date(sinceParam);
		if (Number.isNaN(parsed.getTime())) {
			return json({ error: 'since must be a valid ISO timestamp' }, { status: 400 });
		}
		since = parsed;
	}

	const sortParam = url.searchParams.get('sort');
	let sort: AgentIssuesSort | undefined;
	if (sortParam !== null) {
		if (!(VALID_SORTS as readonly string[]).includes(sortParam)) {
			return json({ error: `sort must be one of: ${VALID_SORTS.join(', ')}` }, { status: 400 });
		}
		sort = sortParam as AgentIssuesSort;
	}

	const limitParam = url.searchParams.get('limit');
	let limit: number | undefined;
	if (limitParam !== null) {
		const parsed = Number(limitParam);
		if (!Number.isFinite(parsed) || !Number.isInteger(parsed) || parsed < 1) {
			return json({ error: `limit must be a positive integer (max ${AGENT_ISSUES_MAX_LIMIT})` }, { status: 400 });
		}
		limit = parsed;
	}

	const cursorParam = url.searchParams.get('cursor');
	let cursor: ReturnType<typeof decodeAgentIssuesCursor> | undefined;
	if (cursorParam !== null) {
		try {
			cursor = decodeAgentIssuesCursor(cursorParam);
		} catch {
			return json({ error: 'cursor is invalid or malformed' }, { status: 400 });
		}
	}

	const result = await listAgentIssues({
		organizationId: ctx.organizationId,
		type,
		claimed,
		claimedByAgentId,
		projectId,
		waiting,
		since,
		sort,
		limit,
		cursor,
	});

	return json(
		result.nextCursor !== undefined
			? { issues: result.issues, nextCursor: result.nextCursor }
			: { issues: result.issues }
	);
};
