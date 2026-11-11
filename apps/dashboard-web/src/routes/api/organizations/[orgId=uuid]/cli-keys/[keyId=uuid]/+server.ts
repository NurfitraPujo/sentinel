import { error, json } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { hasPermission } from '$lib/rbac';
import { requireOrgMembership } from '../../keys/_shared';
import { getPersonalKey, revokePersonalKey } from '$lib/server/cli-personal-keys';
import { isUuid } from '$lib/uuid';

// sentinel-cli-browser-login (docs/plans/CLI_BROWSER_LOGIN.md). DELETE /api/organizations/{orgId}/cli-keys/{keyId} -- an org
// owner/admin ('manage_agents') revokes a member's personal CLI key from the Agents page. Session-authenticated; a JSON-less
// DELETE is not something a cross-site <form> can send. The key must be a PERSONAL key of THIS organization -- anything
// else (another org's key, an org agent's key, an ingest key) is the same 404. Audited as `cli.key.revoked` (agents_page).
export const DELETE: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const membership = await requireOrgMembership(session.user.id, params.orgId);
	if (!membership) {
		throw error(403, 'Forbidden: not a member of this organization');
	}
	if (!hasPermission(membership.role, 'manage_agents')) {
		throw error(403, "Forbidden: only owners and admins can revoke other members' CLI keys");
	}

	const key = isUuid(params.keyId) ? await getPersonalKey(params.keyId) : undefined;
	if (!key || key.organizationId !== params.orgId) {
		throw error(404, 'CLI key not found');
	}

	const revoked = await revokePersonalKey({
		keyId: key.keyId,
		actorId: session.user.id,
		via: 'agents_page',
		organizationId: key.organizationId,
		ownerUserId: key.ownerUserId,
	});

	return json({ success: true, keyId: key.keyId, alreadyRevoked: !revoked });
};
