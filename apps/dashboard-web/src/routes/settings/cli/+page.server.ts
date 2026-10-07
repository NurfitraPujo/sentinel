import { error, fail } from '@sveltejs/kit';
import type { Actions, PageServerLoad } from './$types';
import { getPersonalKey, isLivePersonalKey, listPersonalKeysForUser, revokePersonalKey } from '$lib/server/cli-personal-keys';
import { approveRequestIsSameOrigin } from '$lib/server/cli-approve';

// sentinel-cli-browser-login (docs/plans/CLI_BROWSER_LOGIN.md). /settings/cli -- "CLI sessions": the signed-in user's own
// personal keys (one per device per organization), with Revoke. The parent +layout.server.ts enforces a session; this
// file re-reads it for the user id. There is no "last used" column: keys do not track one, and none is invented here.

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export const load: PageServerLoad = async ({ locals }) => {
	const session = await locals.auth();
	const userId = session?.user?.id;
	if (!userId) throw error(401, 'Unauthorized');

	const rows = await listPersonalKeysForUser(userId);
	const now = Date.now();
	return {
		sessions: rows.map((r) => ({
			keyId: r.keyId,
			deviceName: r.deviceName,
			keyPrefix: r.keyPrefix,
			organizationName: r.organizationName,
			organizationSlug: r.organizationSlug,
			createdAt: r.createdAt ? r.createdAt.toISOString() : null,
			expiresAt: r.expiresAt ? r.expiresAt.toISOString() : null,
			state: isLivePersonalKey(r, now) ? 'active' : r.status === 'revoked' ? 'revoked' : 'expired',
		})),
	};
};

export const actions: Actions = {
	revoke: async ({ request, locals, url }) => {
		if (!approveRequestIsSameOrigin(request, url.origin)) {
			return fail(403, { error: 'Cross-site request refused.' });
		}
		const session = await locals.auth();
		const userId = session?.user?.id;
		if (!userId) return fail(401, { error: 'Sign in first.' });

		const keyId = (await request.formData()).get('keyId');
		const key = typeof keyId === 'string' && UUID_RE.test(keyId) ? await getPersonalKey(keyId) : undefined;
		// Only the caller's OWN key; anyone else's is indistinguishable from a missing one.
		if (!key || key.ownerUserId !== userId) {
			return fail(404, { error: 'CLI session not found.' });
		}
		await revokePersonalKey({
			keyId: key.keyId,
			actorId: userId,
			via: 'settings',
			organizationId: key.organizationId,
			ownerUserId: userId,
		});
		return { revoked: key.keyId };
	},
};
