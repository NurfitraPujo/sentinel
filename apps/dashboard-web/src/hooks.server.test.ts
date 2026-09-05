import { describe, it, expect, vi, beforeEach } from 'vitest';

// D01/P2-1: hooks.server.ts:77's reservedRoutes used to be missing 'invitations'. orgHandle runs
// BEFORE route resolution, so an authenticated request to /invitations/<token> was parsed as an org
// slug, found no matching org, and threw a 403 — the redirect in
// routes/invitations/[token]/+page.server.ts never got a chance to run. Anonymous users escaped only
// via orgHandle's early return on no session. This test drives orgHandle directly (exported for this
// purpose) with a fake authenticated session and asserts it calls through to `resolve` instead of
// throwing, for exactly that path.

function makeDbMock() {
	const dbMock: any = {
		select: vi.fn(),
		from: vi.fn(),
		where: vi.fn(),
		limit: vi.fn(),
		leftJoin: vi.fn(),
		innerJoin: vi.fn(),
		then: vi.fn(),
	};
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.limit.mockReturnValue(dbMock);
	dbMock.leftJoin.mockReturnValue(dbMock);
	dbMock.innerJoin.mockReturnValue(dbMock);
	// No org memberships at all for this user — irrelevant to the assertion (we only care whether
	// orgSlugFromUrl is even computed from the path), but keeps every query resolving to an empty set.
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
	return dbMock;
}

const dbMock = makeDbMock();

vi.mock('$lib/server/db', () => ({ db: dbMock }));
vi.mock('$lib/db/schema', () => ({
	organizations: { id: 'id', slug: 'slug' },
	organizationMembers: { id: 'id', organizationId: 'organizationId', userId: 'userId', role: 'role' },
	userSessionPreferences: { userId: 'userId', lastActiveOrganizationId: 'lastActiveOrganizationId' },
	users: { id: 'id', email: 'email' },
}));

// `dev` is true under vitest (the SvelteKit vite plugin resolves `$app/environment` in dev mode),
// and `csrfHandle` matches upstream by only guarding production -- so the mock is what makes the
// production behaviour testable at all. `dev: true` is asserted separately below.
const environment = { dev: false, browser: false, building: false, version: 'test' };
vi.mock('$app/environment', () => environment);

const { orgHandle, csrfHandle } = await import('./hooks.server');

function makeEvent(pathname: string, authenticated: boolean) {
	return {
		url: { pathname },
		locals: {
			auth: async () =>
				authenticated ? { user: { email: 'colleague@example.com' } } : null,
		},
	} as any;
}

describe('orgHandle reservedRoutes (D01)', () => {
	beforeEach(() => {
		vi.resetAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.limit.mockReturnValue(dbMock);
		dbMock.leftJoin.mockReturnValue(dbMock);
		dbMock.innerJoin.mockReturnValue(dbMock);
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
		// First select() call in orgHandle looks up the user by email; return a fake user row so the
		// function proceeds past that guard and reaches the reservedRoutes check.
		let callCount = 0;
		dbMock.then.mockImplementation((resolve: any) => {
			callCount += 1;
			// The very first awaited query in orgHandle is `users` lookup -> needs one row with `id`.
			if (callCount === 1) return resolve([{ id: 'user-1' }]);
			return resolve([]);
		});
	});

	it('reaches resolve() (not a 403) for an authenticated request to /invitations/<token>', async () => {
		const resolve = vi.fn().mockResolvedValue(new Response('ok'));
		const event = makeEvent('/invitations/abc123token', true);

		const response = await orgHandle({ event, resolve });

		expect(resolve).toHaveBeenCalledWith(event);
		expect(response).toBeInstanceOf(Response);
	});

	it('still 403s an authenticated request to an unknown top-level path treated as an org slug', async () => {
		const resolve = vi.fn().mockResolvedValue(new Response('ok'));
		const event = makeEvent('/some-nonexistent-org-slug', true);

		await expect(orgHandle({ event, resolve })).rejects.toMatchObject({ status: 403 });
	});

	it('anonymous requests to /invitations/<token> pass straight through regardless', async () => {
		const resolve = vi.fn().mockResolvedValue(new Response('ok'));
		const event = makeEvent('/invitations/abc123token', false);

		await orgHandle({ event, resolve });

		expect(resolve).toHaveBeenCalledWith(event);
	});
});

/**
 * `svelte.config.js` sets `kit.csrf.checkOrigin: false` because SvelteKit's built-in check runs
 * before `handle`, has no per-route escape hatch, and answers `403 "Cross-site POST form
 * submissions are forbidden"` to any `Origin`-less multipart POST -- which is exactly what
 * sentinel-user-report-issue §4.5's service caller sends. `csrfHandle` is the replacement. The
 * predicate itself is covered in `src/lib/server/csrf.test.ts`; what is asserted HERE is that the
 * hook is wired to it, that the exemption is applied, and that `dev` is honoured.
 */
describe('csrfHandle (§4.5 -- the intake exemption, and only that)', () => {
	function csrfEvent(pathname: string, request: Request) {
		return { url: new URL(request.url), request } as any;
	}

	function multipartPost(pathname: string, origin?: string): Request {
		const headers = new Headers({ 'content-type': 'multipart/form-data; boundary=x' });
		if (origin) headers.set('origin', origin);
		return new Request(`https://sentinel.example.com${pathname}`, { method: 'POST', headers });
	}

	beforeEach(() => {
		environment.dev = false;
	});

	it('lets an Origin-less multipart POST through to /api/report/attachments', async () => {
		const resolve = vi.fn().mockResolvedValue(new Response('ok'));
		const request = multipartPost('/api/report/attachments');

		const response = await csrfHandle({ event: csrfEvent('/api/report/attachments', request), resolve } as any);

		expect(resolve).toHaveBeenCalledTimes(1);
		expect(response.status).toBe(200);
	});

	it('lets an Origin-less multipart POST through to /api/report', async () => {
		const resolve = vi.fn().mockResolvedValue(new Response('ok'));
		const request = multipartPost('/api/report');

		await csrfHandle({ event: csrfEvent('/api/report', request), resolve } as any);

		expect(resolve).toHaveBeenCalledTimes(1);
	});

	it('still 403s an Origin-less multipart POST to /api/uploads, without calling the route', async () => {
		const resolve = vi.fn().mockResolvedValue(new Response('ok'));
		const request = multipartPost('/api/uploads');

		const response = await csrfHandle({ event: csrfEvent('/api/uploads', request), resolve } as any);

		expect(response.status).toBe(403);
		expect(await response.text()).toBe('Cross-site POST form submissions are forbidden');
		expect(resolve).not.toHaveBeenCalled();
	});

	it('still 403s a foreign-origin multipart POST to a non-exempt path', async () => {
		const resolve = vi.fn().mockResolvedValue(new Response('ok'));
		const request = multipartPost('/api/uploads', 'https://evil.example');

		const response = await csrfHandle({ event: csrfEvent('/api/uploads', request), resolve } as any);

		expect(response.status).toBe(403);
		expect(resolve).not.toHaveBeenCalled();
	});

	it('lets a same-origin multipart POST through to a non-exempt path', async () => {
		const resolve = vi.fn().mockResolvedValue(new Response('ok'));
		const request = multipartPost('/api/uploads', 'https://sentinel.example.com');

		await csrfHandle({ event: csrfEvent('/api/uploads', request), resolve } as any);

		expect(resolve).toHaveBeenCalledTimes(1);
	});

	it('does nothing in dev, matching upstream\'s `if (!DEV)` guard', async () => {
		environment.dev = true;
		const resolve = vi.fn().mockResolvedValue(new Response('ok'));
		const request = multipartPost('/api/uploads');

		await csrfHandle({ event: csrfEvent('/api/uploads', request), resolve } as any);

		expect(resolve).toHaveBeenCalledTimes(1);
	});
});
