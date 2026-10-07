import { describe, it, expect, vi, beforeEach } from 'vitest';
import { resetRateLimitKey } from '$lib/rate-limit';

/**
 * sentinel-cli-browser-login: the /cli/approve page. Session required for the load AND both actions; each action refuses a
 * request whose Origin is not this server (CSRF, enforced even in dev); the org must come from the form and is re-checked
 * by decideDeviceAuthorization (mocked here; its real-Postgres suite proves the re-check); lookups are rate-limited.
 */

const findDeviceAuthorization = vi.fn();
const decideDeviceAuthorization = vi.fn();
vi.mock('$lib/server/cli-device', async (orig) => ({
	...(await orig<typeof import('$lib/server/cli-device')>()),
	findDeviceAuthorization,
	decideDeviceAuthorization,
}));
const getUserOrganizations = vi.fn();
vi.mock('$lib/db/queries/organizations', () => ({ getUserOrganizations }));
const liveKeysReplacedByDevice = vi.fn();
vi.mock('$lib/server/cli-personal-keys', () => ({ liveKeysReplacedByDevice }));

const { load, actions } = await import('./+page.server');

const session = (id: string | null) => ({ auth: async () => (id ? { user: { id, email: `${id}@example.test` } } : null) });

function loadEvent(userId: string | null, search = '') {
	return { locals: { ...session(userId), currentOrg: { id: 'org-2' } }, url: new URL(`http://dash/cli/approve${search}`) } as any;
}

function actionEvent(userId: string | null, fields: Record<string, string>, origin: string | null = 'http://dash') {
	const headers: Record<string, string> = { 'content-type': 'application/x-www-form-urlencoded' };
	if (origin) headers.origin = origin;
	const url = new URL('http://dash/cli/approve?/approve');
	return {
		request: new Request(url, { method: 'POST', headers, body: new URLSearchParams(fields) }),
		url,
		locals: session(userId),
		getClientAddress: () => '198.51.100.1',
	} as any;
}

const ORG_1 = '11111111-1111-4111-8111-111111111111';
const ORG_9 = '99999999-9999-4999-8999-999999999999';

const PENDING = {
	id: 'dev-1',
	userCode: 'BCDFGHJK',
	state: 'pending',
	client: 'sentinel-cli',
	clientVersion: 'v1.2.3',
	deviceName: 'ada-laptop',
	requestedProfile: 'production',
	requestedIp: '203.0.113.7',
	createdAt: new Date('2026-10-07T00:00:00Z'),
	expiresAt: new Date('2026-10-07T00:10:00Z'),
};

beforeEach(() => {
	vi.clearAllMocks();
	for (const u of ['u1', 'u2', 'u3']) resetRateLimitKey(`cli-approve:user:${u}`);
	getUserOrganizations.mockResolvedValue([
		{ id: 'org-1', name: 'Acme', slug: 'acme', role: 'engineer' },
		{ id: 'org-2', name: 'Beta', slug: 'beta', role: 'viewer' },
	]);
	findDeviceAuthorization.mockResolvedValue(PENDING);
	liveKeysReplacedByDevice.mockResolvedValue([]);
});

describe('/cli/approve load', () => {
	it('signed out: offers sign-in that comes back to this code, and reads nothing', async () => {
		const data: any = await load(loadEvent(null, '?code=bcdf-ghjk'));
		expect(data).toEqual({ signedIn: false, signInUrl: '/signin?next=%2Fcli%2Fapprove%3Fcode%3DBCDF-GHJK' });
		expect(findDeviceAuthorization).not.toHaveBeenCalled();
	});

	it('signed in: shows the request details and only the user\'s own orgs, defaulting to the active one', async () => {
		const data: any = await load(loadEvent('u1', '?code=BCDF-GHJK'));
		expect(findDeviceAuthorization).toHaveBeenCalledWith('BCDFGHJK');
		expect(data).toMatchObject({
			signedIn: true,
			code: 'BCDF-GHJK',
			codeError: null,
			defaultOrganizationId: 'org-2',
			request: { state: 'pending', deviceName: 'ada-laptop', requestedIp: '203.0.113.7', clientVersion: 'v1.2.3' },
		});
		expect(data.organizations.map((o: any) => o.id)).toEqual(['org-1', 'org-2']);
		expect(getUserOrganizations).toHaveBeenCalledWith('u1');
	});

	// Security review, repair round 1: approving a device name that already has a live key REPLACES it -- the page says
	// so, per org, because a phisher who copies the victim's hostname otherwise looks like the victim's own re-login.
	it('reports which live key of the same device name approving would replace, only for a pending request', async () => {
		liveKeysReplacedByDevice.mockResolvedValue([{ organizationId: 'org-2', createdAt: '2026-09-01T00:00:00.000Z' }]);
		const data: any = await load(loadEvent('u1', '?code=BCDF-GHJK'));
		expect(liveKeysReplacedByDevice).toHaveBeenCalledWith('u1', 'ada-laptop');
		expect(data.replaces).toEqual([{ organizationId: 'org-2', createdAt: '2026-09-01T00:00:00.000Z' }]);

		liveKeysReplacedByDevice.mockClear();
		findDeviceAuthorization.mockResolvedValueOnce({ ...PENDING, state: 'delivered' });
		const used: any = await load(loadEvent('u1', '?code=BCDF-GHJK'));
		expect(used.replaces).toEqual([]);
		expect(liveKeysReplacedByDevice).not.toHaveBeenCalled();
	});

	it('a malformed code is refused before any lookup; an unknown code says so', async () => {
		const bad: any = await load(loadEvent('u1', '?code=AAAA-1111'));
		expect(bad.codeError).toMatch(/not a valid code/);
		expect(findDeviceAuthorization).not.toHaveBeenCalled();
		findDeviceAuthorization.mockResolvedValueOnce(null);
		const unknown: any = await load(loadEvent('u1', '?code=BCDF-GHJK'));
		expect(unknown.codeError).toMatch(/No login request/);
	});

	it('limits code lookups per user (20 per 10 minutes)', async () => {
		for (let i = 0; i < 20; i++) await load(loadEvent('u2', '?code=BCDF-GHJK'));
		const limited: any = await load(loadEvent('u2', '?code=BCDF-GHJK'));
		expect(limited.codeError).toMatch(/Too many/);
		expect(findDeviceAuthorization).toHaveBeenCalledTimes(20);
	});
});

describe('/cli/approve actions', () => {
	it('approve: decides with the session user, the form org and the normalized code', async () => {
		decideDeviceAuthorization.mockResolvedValue({ ok: true, state: 'approved' });
		const res = await actions.approve(actionEvent('u1', { code: 'bcdf-ghjk', organizationId: ORG_1 }));
		expect(res).toEqual({ decided: 'approved', code: 'BCDF-GHJK' });
		expect(decideDeviceAuthorization).toHaveBeenCalledWith({
			userCode: 'BCDFGHJK',
			userId: 'u1',
			organizationId: ORG_1,
			decision: 'approve',
			ip: '198.51.100.1',
		});
	});

	it('deny: never binds an org', async () => {
		decideDeviceAuthorization.mockResolvedValue({ ok: true, state: 'denied' });
		const res = await actions.deny(actionEvent('u1', { code: 'BCDF-GHJK', organizationId: ORG_1 }));
		expect(res).toEqual({ decided: 'denied', code: 'BCDF-GHJK' });
		expect(decideDeviceAuthorization.mock.calls[0][0]).toMatchObject({ decision: 'deny', organizationId: null });
	});

	it.each([
		['a foreign Origin', 'http://evil.example'],
		['no Origin at all', null],
	])('CSRF: %s is refused with 403 and nothing is decided', async (_label, origin) => {
		const res: any = await actions.approve(actionEvent('u1', { code: 'BCDF-GHJK', organizationId: ORG_1 }, origin));
		expect(res.status).toBe(403);
		expect(decideDeviceAuthorization).not.toHaveBeenCalled();
	});

	it('requires a session', async () => {
		const res: any = await actions.approve(actionEvent(null, { code: 'BCDF-GHJK', organizationId: ORG_1 }));
		expect(res.status).toBe(401);
		expect(decideDeviceAuthorization).not.toHaveBeenCalled();
	});

	it.each([
		['not_a_member', 403],
		['expired', 400],
		['already_decided', 400],
		['not_found', 400],
	] as const)('%s -> %i with a readable message', async (reason, status) => {
		decideDeviceAuthorization.mockResolvedValue({ ok: false, reason });
		const res: any = await actions.approve(actionEvent('u3', { code: 'BCDF-GHJK', organizationId: ORG_9 }));
		expect(res.status).toBe(status);
		expect(typeof res.data.error).toBe('string');
	});

	// Security review, repair round 1: a malformed organizationId used to reach the uuid column and 500.
	it.each([['not-a-uuid'], [''], ["' or 1=1 --"], ['00000000-0000-4000-8000-00000000000g']])(
		'approve with organizationId %j is a 400 and never reaches the database',
		async (organizationId) => {
			const res: any = await actions.approve(actionEvent('u1', { code: 'BCDF-GHJK', organizationId }));
			expect(res.status).toBe(400);
			expect(typeof res.data.error).toBe('string');
			expect(decideDeviceAuthorization).not.toHaveBeenCalled();
		}
	);

	it('approve with no organizationId at all is a 400', async () => {
		const res: any = await actions.approve(actionEvent('u1', { code: 'BCDF-GHJK' }));
		expect(res.status).toBe(400);
		expect(decideDeviceAuthorization).not.toHaveBeenCalled();
	});

	it('deny needs no organization, so a junk organizationId does not block it', async () => {
		decideDeviceAuthorization.mockResolvedValue({ ok: true, state: 'denied' });
		const res = await actions.deny(actionEvent('u1', { code: 'BCDF-GHJK', organizationId: 'junk' }));
		expect(res).toEqual({ decided: 'denied', code: 'BCDF-GHJK' });
		expect(decideDeviceAuthorization.mock.calls[0][0]).toMatchObject({ organizationId: null });
	});
});
