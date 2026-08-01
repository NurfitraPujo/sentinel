import { describe, it, expect, vi, beforeEach } from 'vitest';
import { redirect } from '@sveltejs/kit';

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
	// `clearAllMocks` clears call records but NOT queued `mockImplementationOnce` entries, so an
	// un-consumed queued resolution leaks into the NEXT test and answers the wrong query, making
	// results order-dependent. `mockReset` on the queue-bearing mock drops the queue; the base
	// implementation is re-established on the next line.
	dbMock.then.mockReset();
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
	// signInMock's implementation is set at module scope, and tests below replace it with a
	// persistent rejection. `clearAllMocks` wipes call records but NOT implementations, so that
	// rejection leaked into whichever test ran next — an unrelated test then failed with the
	// previous test's error ("SMTP unreachable") under --sequence.shuffle. Re-establish the default.
	signInMock.mockReset();
	signInMock.mockResolvedValue(undefined);
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.limit.mockReturnValue(dbMock);
	dbMock.then.mockReset();
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

	// This assertion previously required `callbackUrl` — pinning the D29 defect in place, since
	// @auth/sveltekit v5 ignores that option entirely. Corrected to `redirectTo`, the option this
	// project's Auth.js version actually reads.
	it('magiclink action never puts the token in redirectTo', async () => {
		const request = new Request('http://x', {
			method: 'POST',
			headers: { 'content-type': 'application/x-www-form-urlencoded' },
			body: 'email=user%40example.com',
		});
		await (actions as any).magiclink({ request });
		expect(signInMock).toHaveBeenCalledWith('email', { email: 'user@example.com', redirectTo: '/auth/accept-invite' });
	});
});

// D29: the magic-link action had two independent bugs that each broke the flow on their own.
describe('accept-invite magiclink action (D29)', () => {
	beforeEach(() => {
		vi.resetAllMocks();
		signInMock.mockResolvedValue(undefined);
	});

	function formRequest(fields: Record<string, string>) {
		const body = new URLSearchParams(fields);
		return new Request('http://x', {
			method: 'POST',
			body,
			headers: { 'content-type': 'application/x-www-form-urlencoded' },
		});
	}

	it('passes redirectTo, not the Auth.js v4 callbackUrl that v5 ignores', async () => {
		await actions.magiclink({ request: formRequest({ email: 'a@b.com' }) } as any);

		expect(signInMock).toHaveBeenCalledTimes(1);
		const [provider, options] = signInMock.mock.calls[0];
		expect(provider).toBe('email');
		expect(options).toHaveProperty('redirectTo', '/auth/accept-invite');
		// The whole defect: v5 silently ignores `callbackUrl`, so the user never came back here.
		expect(options).not.toHaveProperty('callbackUrl');
	});

	it('never puts the invite token in the redirect URL (D06)', async () => {
		await actions.magiclink({ request: formRequest({ email: 'a@b.com' }) } as any);

		const [, options] = signInMock.mock.calls[0];
		expect(String(options.redirectTo)).not.toContain('token');
	});

	it('rethrows the redirect signIn throws on success instead of turning it into a 500', async () => {
		// @auth/sveltekit signals a successful sign-in by THROWING a redirect. The old catch-all
		// swallowed it and returned fail(500), so the flow could not complete even once the option
		// name was correct.
		// Construct a real SvelteKit Redirect: isRedirect() brand-checks the instance, so a
		// plain { status, location } object would not be recognised.
		let redirectSignal: unknown;
		try {
			redirect(303, '/verify-request');
		} catch (e) {
			redirectSignal = e;
		}
		signInMock.mockRejectedValueOnce(redirectSignal);

		await expect(
			actions.magiclink({ request: formRequest({ email: 'a@b.com' }) } as any)
		).rejects.toBe(redirectSignal);
	});

	it('still returns a 500 for a genuine send failure', async () => {
		signInMock.mockRejectedValueOnce(new Error('SMTP unreachable'));

		const result: any = await actions.magiclink({
			request: formRequest({ email: 'a@b.com' }),
		} as any);
		expect(result.status).toBe(500);
	});

	it('400s a missing email without calling signIn', async () => {
		const result: any = await actions.magiclink({ request: formRequest({ email: '' }) } as any);
		expect(result.status).toBe(400);
		expect(signInMock).not.toHaveBeenCalled();
	});
});
