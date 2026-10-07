import { error, json } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { revokePersonalKey } from '$lib/server/cli-personal-keys';

// sentinel-cli-browser-login (docs/plans/CLI_BROWSER_LOGIN.md). POST /api/agent/key/revoke -- `sentinel logout`: revokes
// the CALLING key (ctx.keyId from authenticateAgentRequest's own lookup, never anything the request names -- B7), so it can
// only ever end its own session. Personal keys only: an org agent's key is an admin's to retire (and a CI job that runs
// `sentinel logout` by mistake must not take a shared integration down), so it gets a 403 and stays valid.
// Audited as `cli.key.revoked` (via: cli_logout) with the key's own agent as the actor.
export const POST: RequestHandler = async ({ request }) => {
	const ctx = await authenticateAgentRequest(request);
	if (!ctx.owner) {
		throw error(403, 'Forbidden: org agent keys are revoked by an org owner or admin in the dashboard, not by the key itself');
	}

	await revokePersonalKey({
		keyId: ctx.keyId,
		actorId: ctx.agentId,
		via: 'cli_logout',
		organizationId: ctx.organizationId,
		ownerUserId: ctx.owner.userId,
	});

	return json({ success: true, revokedKeyId: ctx.keyId });
};
