import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { getApiKeyById, rotateApiKey, createNatsPublisher, toPublicKey } from '$lib/db/queries/apikeys';
import { hasPermission } from '$lib/rbac';
import { requireOrgMembership } from '../../_shared';
import { getAgentById } from '$lib/db/queries/agents';

export const POST: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId, keyId } = params;
	const membership = await requireOrgMembership(session.user.id, orgId!);
	if (!membership) {
		throw error(403, 'Forbidden: not a member of this organization');
	}
	if (!hasPermission(membership.role, 'manage_keys')) {
		throw error(403, 'Forbidden: only owners, admins, and engineers can rotate API keys');
	}

	const existingKey = await getApiKeyById(keyId!);
	if (!existingKey || existingKey.organizationId !== orgId) {
		throw error(404, 'API key not found');
	}

	// sentinel-cli-browser-login: this route hands the NEW SECRET to the caller, so for an agent key it is a credential
	// grant, not key hygiene. An agent key needs 'manage_agents' (owner/admin), exactly like issuing one -- before this an
	// engineer ('manage_keys') could rotate an org agent's key and walk away with it. A PERSONAL key is never rotated here
	// by anyone: the new secret would let the caller act as another person. Its owner rotates it with `sentinel key
	// rotate`; an admin may only revoke it.
	if (existingKey.scope === 'agent') {
		if (!hasPermission(membership.role, 'manage_agents')) {
			throw error(403, 'Forbidden: only owners and admins can rotate agent keys');
		}
		const agentRow = existingKey.agentId ? await getAgentById(existingKey.agentId) : undefined;
		if (agentRow?.kind === 'personal') {
			throw error(403, "Forbidden: a personal CLI key can only be rotated by its owner's CLI; revoke it instead");
		}
	}

	// NOTE on "grace period": spec 008 User Story 1 Scenario 4 originally described a 24h
	// dual-active window for the old key. rotateApiKey (src/lib/db/queries/apikeys.ts) revokes the
	// old key IMMEDIATELY instead — see the DECISION comment on rotateApiKey for the full
	// rationale (rotation usually means "this secret may be leaked", so a multi-hour window where
	// it still authenticates defeats the point). specs/008-api-key-management/spec.md has been
	// amended to match; this is not a bug to fix here.
	const { apiKey, secretToken } = await rotateApiKey(session.user.id, existingKey.id, '24h', createNatsPublisher());

	return json({
		success: true,
		message: 'Key rotated successfully. The previous key was revoked immediately (no grace period) — see specs/008-api-key-management/spec.md.',
		key: toPublicKey(apiKey),
		token: secretToken,
	});
};
