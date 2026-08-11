import { json } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { listAgentIssues } from '$lib/db/queries/agent-work';

// Manual Issues M5 stage 2 (design §7 step 1). GET /api/agent/issues?type=&claimed=&project=&waiting=
// -- spans both issue types, scoped to the key's own org (B7). Manual-validation style, matching
// every other route in this feature.

const VALID_TYPES = ['user_report', 'system_error'] as const;

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

	const claimedParam = url.searchParams.get('claimed');
	let claimed: boolean | undefined;
	if (claimedParam === 'true') claimed = true;
	else if (claimedParam === 'false') claimed = false;
	else if (claimedParam !== null) {
		return json({ error: 'claimed must be "true" or "false"' }, { status: 400 });
	}

	const projectId = url.searchParams.get('project') || undefined;

	const waitingParam = url.searchParams.get('waiting');
	let waiting: boolean | undefined;
	if (waitingParam === 'true') waiting = true;
	else if (waitingParam !== null && waitingParam !== 'false') {
		return json({ error: 'waiting must be "true" or "false"' }, { status: 400 });
	}

	const issues = await listAgentIssues({
		organizationId: ctx.organizationId,
		type,
		claimed,
		projectId,
		waiting,
	});

	return json({ issues });
};
