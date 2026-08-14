import { json } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { listOrgActivity } from '$lib/db/queries/events';
import {
	AGENT_EVENT_TYPES,
	EVENTS_DEFAULT_LIMIT,
	EVENTS_MAX_LIMIT,
	EVENTS_MIN_LIMIT,
	type AgentEventType,
} from '$lib/server/agent-events';

// N1b (events feed). GET /api/agent/events?after=&limit=&type=&project=&claimed=me -- org-scoped
// (B7: organizationId comes ONLY from the authenticated credential, never a request param),
// seq-cursored feed over issue_activity. Mirrors /api/agent/issues's manual-validation style.

export const GET: RequestHandler = async ({ request, url }) => {
	const ctx = await authenticateAgentRequest(request);

	const afterParam = url.searchParams.get('after');
	let after = 0;
	if (afterParam !== null) {
		if (!/^\d+$/.test(afterParam)) {
			return json({ error: 'after must be a non-negative integer' }, { status: 400 });
		}
		after = Number(afterParam);
	}

	const limitParam = url.searchParams.get('limit');
	let limit = EVENTS_DEFAULT_LIMIT;
	if (limitParam !== null) {
		if (!/^\d+$/.test(limitParam)) {
			return json({ error: 'limit must be an integer' }, { status: 400 });
		}
		limit = Math.min(EVENTS_MAX_LIMIT, Math.max(EVENTS_MIN_LIMIT, Number(limitParam)));
	}

	const typeParam = url.searchParams.get('type');
	let eventTypes: AgentEventType[] | undefined;
	if (typeParam !== null) {
		const requested = typeParam
			.split(',')
			.map((t) => t.trim())
			.filter((t) => t.length > 0);
		const invalid = requested.filter((t) => !(AGENT_EVENT_TYPES as readonly string[]).includes(t));
		if (invalid.length > 0) {
			return json(
				{ error: `type contains invalid event type(s): ${invalid.join(', ')}. Valid: ${AGENT_EVENT_TYPES.join(', ')}` },
				{ status: 400 }
			);
		}
		eventTypes = requested as AgentEventType[];
	}

	const projectId = url.searchParams.get('project') || undefined;

	const claimedParam = url.searchParams.get('claimed');
	let claimedByAgentId: string | undefined;
	if (claimedParam !== null) {
		if (claimedParam !== 'me') {
			return json({ error: 'claimed must be "me"' }, { status: 400 });
		}
		claimedByAgentId = ctx.agentId;
	}

	const result = await listOrgActivity({
		organizationId: ctx.organizationId,
		after,
		limit,
		eventTypes,
		projectId,
		claimedByAgentId,
	});

	return json(result);
};
