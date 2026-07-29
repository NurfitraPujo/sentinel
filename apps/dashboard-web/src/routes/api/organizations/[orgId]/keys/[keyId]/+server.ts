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
	if (!hasPermission(membership.role, 'manage_keys')) {
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

	const revokedKey = await revokeApiKey(session.user.id, keyId!, createNatsPublisher());

	return json({
		success: true,
		message: 'Key revoked successfully',
		keyId: revokedKey.id,
	});
};
