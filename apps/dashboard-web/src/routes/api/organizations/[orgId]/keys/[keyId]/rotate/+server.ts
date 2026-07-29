import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { getApiKeyById, rotateApiKey, createNatsPublisher } from '$lib/db/queries/apikeys';
import { hasPermission } from '$lib/rbac';
import { requireOrgMembership } from '../../_shared';

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
		key: apiKey,
		token: secretToken,
	});
};
