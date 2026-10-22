// @vitest-environment node
import { describe, it, expect, vi, beforeEach } from 'vitest';

/** sentinel-cli-browser-login: /settings/cli -- a user sees and revokes only their OWN personal keys. */

const listPersonalKeysForUser = vi.fn();
const getPersonalKey = vi.fn();
const revokePersonalKey = vi.fn();
vi.mock('$lib/server/cli-personal-keys', async (orig) => ({
	...(await orig<typeof import('$lib/server/cli-personal-keys')>()),
	listPersonalKeysForUser,
	getPersonalKey,
	revokePersonalKey,
}));

const { load, actions } = await import('./+page.server');
const KEY = '11111111-1111-4111-8111-111111111111';
const locals = (id: string | null) => ({ auth: async () => (id ? { user: { id } } : null) });

function revokeEvent(userId: string | null, keyId: string, origin = 'http://dash') {
	const url = new URL('http://dash/settings/cli?/revoke');
	return {
		request: new Request(url, {
			method: 'POST',
			headers: { origin, 'content-type': 'application/x-www-form-urlencoded' },
			body: new URLSearchParams({ keyId }),
		}),
		url,
		locals: locals(userId),
	} as any;
}

beforeEach(() => vi.clearAllMocks());

describe('/settings/cli', () => {
	it('lists the user\'s sessions with a derived state (no "last used" -- keys do not track it)', async () => {
		const future = new Date(Date.now() + 86_400_000);
		listPersonalKeysForUser.mockResolvedValue([
			{ keyId: 'k1', deviceName: 'laptop', keyPrefix: 'sent_agent_', status: 'active', revokedAt: null, createdAt: new Date(), expiresAt: future, organizationName: 'Acme', organizationSlug: 'acme' },
			{ keyId: 'k2', deviceName: 'old', keyPrefix: 'sent_agent_', status: 'active', revokedAt: null, createdAt: new Date(), expiresAt: new Date(Date.now() - 1000), organizationName: 'Acme', organizationSlug: 'acme' },
			{ keyId: 'k3', deviceName: 'gone', keyPrefix: 'sent_agent_', status: 'revoked', revokedAt: new Date(), createdAt: new Date(), expiresAt: new Date(), organizationName: 'Acme', organizationSlug: 'acme' },
		]);
		const data: any = await load({ locals: locals('u1') } as any);
		expect(listPersonalKeysForUser).toHaveBeenCalledWith('u1');
		expect(data.sessions.map((s: any) => s.state)).toEqual(['active', 'expired', 'revoked']);
		expect(Object.keys(data.sessions[0])).not.toContain('lastUsedAt');
	});

	it('revokes the caller\'s own key', async () => {
		getPersonalKey.mockResolvedValue({ keyId: KEY, ownerUserId: 'u1', organizationId: 'org-1' });
		expect(await actions.revoke(revokeEvent('u1', KEY))).toEqual({ revoked: KEY });
		expect(revokePersonalKey).toHaveBeenCalledWith({ keyId: KEY, actorId: 'u1', via: 'settings', organizationId: 'org-1', ownerUserId: 'u1' });
	});

	it('someone else\'s key is a 404 and is not touched', async () => {
		getPersonalKey.mockResolvedValue({ keyId: KEY, ownerUserId: 'u2', organizationId: 'org-1' });
		const res: any = await actions.revoke(revokeEvent('u1', KEY));
		expect(res.status).toBe(404);
		expect(revokePersonalKey).not.toHaveBeenCalled();
	});

	it('refuses a cross-site POST and a missing session', async () => {
		expect(((await actions.revoke(revokeEvent('u1', KEY, 'http://evil'))) as any).status).toBe(403);
		expect(((await actions.revoke(revokeEvent(null, KEY))) as any).status).toBe(401);
		expect(getPersonalKey).not.toHaveBeenCalled();
	});
});
