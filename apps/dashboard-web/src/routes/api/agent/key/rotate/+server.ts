import { error, json } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { env } from '$env/dynamic/private';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { rotateAgentKeyWithGrace, AgentKeyRotationError } from '$lib/db/queries/apikeys';
import { writeAgentAuditLog } from '$lib/server/agent-audit';

// R1b (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7f): rotates the CALLING key only --
// `ctx.keyId` comes from `authenticateAgentRequest`'s own lookup, never anything the request
// supplies (B7). `AGENT_KEY_ROTATION_GRACE_HOURS` (default 24, 0 = immediate) controls how long
// the old key stays valid after rotation; see rotateAgentKeyWithGrace's doc comment for why this
// is deliberately NOT the same immediate-revoke behavior as the human `rotateApiKey`.
export const POST: RequestHandler = async ({ request }) => {
	const ctx = await authenticateAgentRequest(request);

	const graceHours = parseInt(env.AGENT_KEY_ROTATION_GRACE_HOURS ?? '24', 10);
	if (Number.isNaN(graceHours) || graceHours < 0) {
		throw error(500, 'AGENT_KEY_ROTATION_GRACE_HOURS is misconfigured');
	}

	let result;
	try {
		result = await rotateAgentKeyWithGrace(ctx.keyId, graceHours);
	} catch (err) {
		if (err instanceof AgentKeyRotationError) {
			throw error(400, err.message);
		}
		throw err;
	}

	await writeAgentAuditLog(ctx, 'agent.key.rotated', 'api_key', ctx.keyId, {
		newKeyId: result.newKey.id,
		graceHours,
	});

	return json({
		success: true,
		oldKey: {
			id: ctx.keyId,
			expiresAt: result.oldKey?.expiresAt ? result.oldKey.expiresAt.toISOString() : null,
		},
		newKey: {
			id: result.newKey.id,
			prefix: result.newKey.keyPrefix,
			secret: result.secretToken,
		},
	});
};
