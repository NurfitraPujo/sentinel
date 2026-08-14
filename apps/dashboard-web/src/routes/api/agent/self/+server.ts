import { json } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';

/**
 * R1a (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7f): closes the "no dedicated identity
 * endpoint" gap `sentinel whoami` used to work around by probing GET /api/agent/issues and
 * reporting only reachability (tools/sentinel-cli/commands.go's old `cmdWhoami`). Returns exactly
 * what `authenticateAgentRequest` already resolved for THIS request -- no second query, no
 * `lastUsedAt` (project_api_keys tracks no such column; adding one is out of scope for this
 * phase, see the plan's R1 note).
 */
export const GET: RequestHandler = async ({ request }) => {
	const ctx = await authenticateAgentRequest(request);

	return json({
		agentId: ctx.agentId,
		name: ctx.agentName,
		organizationId: ctx.organizationId,
		key: {
			id: ctx.keyId,
			prefix: ctx.keyPrefix,
			expiresAt: ctx.keyExpiresAt ? ctx.keyExpiresAt.toISOString() : null,
			lastUsedAt: null,
		},
	});
};
