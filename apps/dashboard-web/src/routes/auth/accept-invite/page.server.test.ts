import { describe, it, expect, vi, beforeEach } from 'vitest';

const orgQueries = {
	getInvitationByToken: vi.fn(),
	claimInvitation: vi.fn(),
	updateUserLastActiveOrg: vi.fn(),
};
vi.mock('$lib/db/queries/organizations', () => orgQueries);

function makeDbMock() {
	const dbMock: any = { select: vi.fn(), from: vi.fn(), where: vi.fn(), limit: vi.fn(), then: vi.fn() };
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.limit.mockReturnValue(dbMock);
	dbMock.then.mockImplementation((resolve: any) => resolve([{ id: 'user-1' }]));
	return dbMock;
}
const dbMock = makeDbMock();
vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	organizationMembers: { id: 'id', organizationId: 'organizationId', userId: 'userId', role: 'role' },
	users: { id: 'id', email: 'email' },
}));

vi.mock('$env/dynamic/private', () => ({ env: {} }));

const signInMock = vi.fn().mockResolvedValue(undefined);
vi.mock('$lib/server/auth-config', () => ({ signIn: (...args: any[]) => signInMock(...args) }));

const { load, actions } = await import('./+page.server');

function makeCookies(initial?: string) {
	let value: string | undefined = initial;
	return {
		get: vi.fn(() => value),
		set: vi.fn((_n: string, v: string) => {
			value = v;
		}),
		delete: vi.fn(() => {
			value = undefined;
		}),
	};
}

beforeEach(() => {
	vi.clearAllMocks();
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.limit.mockReturnValue(dbMock);
	dbMock.then.mockImplementation((resolve: any) => resolve([{ id: 'user-1' }]));
});

describe('/auth/accept-invite load (D06)', () => {
	it('reads the token from the cookie, not the URL', async () => {
		const cookies = makeCookies('raw-token');
		orgQueries.getInvitationByToken.mockResolvedValueOnce(null);

		const data = await load({ cookies, locals: { auth: async () => null } } as any);

		expect(cookies.get).toHaveBeenCalledWith('sentinel_invite_token');
		expect(orgQueries.getInvitationByToken).toHaveBeenCalledWith('raw-token');
		expect(data).toEqual({ status: 'expired_or_invalid' });
	});

	it('reports invalid_token when no cookie is present, without ever querying the DB', async () => {
		const cookies = makeCookies(undefined);
		const data = await load({ cookies, locals: { auth: async () => null } } as any);
		expect(data).toEqual({ status: 'invalid_token' });
		expect(orgQueries.getInvitationByToken).not.toHaveBeenCalled();
	});
});

describe('/auth/accept-invite actions.accept (D06, D07)', () => {
	it('reads the token from the cookie and never trusts a client-supplied form field', async () => {
		const cookies = makeCookies('raw-token');
		orgQueries.getInvitationByToken.mockResolvedValueOnce({
			invitation: { email: 'user@example.com' },
			organization: { id: 'org-1', slug: 'acme' },
		});
		orgQueries.claimInvitation.mockResolvedValueOnce({
			ok: true,
			organization: { id: 'org-1', slug: 'acme' },
		});

		let caught: any;
		try {
			await (actions as any).accept({
				locals: { auth: async () => ({ user: { email: 'user@example.com' } }) },
				cookies,
			});
		} catch (e) {
			caught = e;
		}

		expect(orgQueries.claimInvitation).toHaveBeenCalledWith('raw-token', 'user-1');
		// The redirect Location is just the org slug path -- never a query string, never the token.
		expect(caught).toBeDefined();
		expect(caught.status).toBe(303);
		expect(caught.location).toBe('/acme');
		expect(caught.location).not.toContain('token');
	});

	it('400s when the cookie is missing', async () => {
		const cookies = makeCookies(undefined);
		const result = await (actions as any).accept({
			locals: { auth: async () => ({ user: { email: 'user@example.com' } }) },
			cookies,
		});
		expect(result.status).toBe(400);
		expect(orgQueries.claimInvitation).not.toHaveBeenCalled();
	});

	it('surfaces already_used as a 409, not a silent success', async () => {
		const cookies = makeCookies('raw-token');
		orgQueries.getInvitationByToken.mockResolvedValueOnce({
			invitation: { email: 'user@example.com' },
			organization: { id: 'org-1', slug: 'acme' },
		});
		orgQueries.claimInvitation.mockResolvedValueOnce({ ok: false, reason: 'already_used' });

		const result = await (actions as any).accept({
			locals: { auth: async () => ({ user: { email: 'user@example.com' } }) },
			cookies,
		});

		expect(result.status).toBe(409);
	});
});

describe('/auth/accept-invite actions.google / actions.magiclink (D06)', () => {
	it('google action never puts the token in redirectTo', async () => {
		await (actions as any).google({});
		expect(signInMock).toHaveBeenCalledWith('google', { redirectTo: '/auth/accept-invite' });
		const [, options] = signInMock.mock.calls[0];
		expect(JSON.stringify(options)).not.toContain('token');
	});

	it('magiclink action never puts the token in callbackUrl', async () => {
		const request = new Request('http://x', {
			method: 'POST',
			headers: { 'content-type': 'application/x-www-form-urlencoded' },
			body: 'email=user%40example.com',
		});
		await (actions as any).magiclink({ request });
		expect(signInMock).toHaveBeenCalledWith('email', { email: 'user@example.com', callbackUrl: '/auth/accept-invite' });
	});
});
