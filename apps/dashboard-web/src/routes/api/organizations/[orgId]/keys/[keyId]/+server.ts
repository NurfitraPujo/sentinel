import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { getApiKeyById, revokeApiKey, createNatsPublisher } from '$lib/db/queries/apikeys';
import { hasPermission } from '$lib/rbac';
import { requireOrgMembership } from '../_shared';

export const DELETE: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId, keyId } = params;
	const membership = await requireOrgMembership(session.user.id, orgId!);
	if (!membership) {
		throw error(403, 'Forbidden: not a member of this organization');
	}

	// Fast-path denial: a role with NEITHER key-management permission can never revoke ANY key
	// regardless of its scope, so this is checked before fetching the key at all -- same
	// external behaviour (403, no lookup performed) as before this route knew about agent
	// scope. A role with at least one of the two permissions falls through to the scope-specific
	// check below, once the key (and therefore its scope) is known.
	if (!hasPermission(membership.role, 'manage_keys') && !hasPermission(membership.role, 'manage_agents')) {
		throw error(403, 'Forbidden: only owners, admins, and engineers can revoke API keys');
	}

	const existingKey = await getApiKeyById(keyId!);
	// A key belonging to a different organization must 404 exactly like a key that does not exist
	// at all (VERIFIED_STATE.md S6's class of bug: the URL's orgId is never the authority — only
	// this membership-scoped comparison is). Do not let a member of org A discover, via a
	// distinguishable error, that keyId belongs to org B.
	if (!existingKey || existingKey.organizationId !== orgId) {
		throw error(404, 'API key not found');
	}

	// M5 §7/§9: revoking an agent's key is agent management, gated the same way agent-key
	// issuance is (owner/admin via 'manage_agents'), not the broader 'manage_keys' FR-007 gate —
	// mirrors the create-side split in ../+server.ts's POST handler.
	const requiredPermission = existingKey.scope === 'agent' ? 'manage_agents' : 'manage_keys';
	if (!hasPermission(membership.role, requiredPermission)) {
		throw error(
			403,
			existingKey.scope === 'agent'
				? 'Forbidden: only owners and admins can revoke agent keys'
				: 'Forbidden: only owners, admins, and engineers can revoke API keys'
		);
	}

	const revokedKey = await revokeApiKey(session.user.id, keyId!, createNatsPublisher());

	return json({
		success: true,
		message: 'Key revoked successfully',
		keyId: revokedKey.id,
	});
};
