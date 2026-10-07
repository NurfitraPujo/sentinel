import { json } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { cliProfileName } from '$lib/server/cli-dist';

/**
 * R1a (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7f): closes the "no dedicated identity
 * endpoint" gap `sentinel whoami` used to work around by probing GET /api/agent/issues and
 * reporting only reachability (tools/sentinel/commands.go's old `cmdWhoami`). Returns exactly
 * what `authenticateAgentRequest` already resolved for THIS request -- no second query, no
 * `lastUsedAt` (project_api_keys tracks no such column; adding one is out of scope for this
 * phase, see the plan's R1 note).
 *
 * N9 (docs/plans/AGENT_WORKER_PLAN.md, C6): `createdAt` is now included so a client can do
 * age-based rotation even for a key with no `expiresAt`.
 *
 * sentinel-cli-browser-login: `kind` ('ai' | 'bot' | 'personal') and `owner` -- the person a personal key acts for, with
 * their CURRENT role and whether that role may make changes (`canWrite`), as resolved for this very request. `owner` is
 * null for an org agent. `sentinel whoami` prints it; the key itself is never returned.
 */
export const GET: RequestHandler = async ({ request }) => {
	const ctx = await authenticateAgentRequest(request);

	// Suggested CLI profile name for this deployment (SENTINEL_CLI_PROFILE), so `sentinel login` on a fresh
	// machine names the profile after the environment. Omitted when unset.
	const cliProfile = cliProfileName();

	return json({
		...(cliProfile ? { cliProfile } : {}),
		agentId: ctx.agentId,
		name: ctx.agentName,
		organizationId: ctx.organizationId,
		kind: ctx.agentKind,
		owner: ctx.owner
			? {
					userId: ctx.owner.userId,
					name: ctx.owner.name,
					email: ctx.owner.email,
					role: ctx.owner.role,
					canWrite: ctx.owner.canWrite,
				}
			: null,
		key: {
			id: ctx.keyId,
			prefix: ctx.keyPrefix,
			createdAt: ctx.keyCreatedAt ? ctx.keyCreatedAt.toISOString() : null,
			expiresAt: ctx.keyExpiresAt ? ctx.keyExpiresAt.toISOString() : null,
			lastUsedAt: null,
		},
	});
};
