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

const { orgHandle, csrfHandle, frameGuardHandle, unmatchedApiHandle } = await import('./hooks.server');

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

	it.each(['/cli/download/linux/amd64', '/cli/checksums.txt', '/install.sh'])(
		'CLI distribution path %s is not parsed as an org slug for a signed-in user',
		async (pathname) => {
			const resolve = vi.fn().mockResolvedValue(new Response('ok'));
			const event = makeEvent(pathname, true);

			await orgHandle({ event, resolve });

			expect(resolve).toHaveBeenCalledWith(event);
		}
	);

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

	it('reaches resolve() (not a 403) for static file requests like /favicon.png even when authenticated', async () => {
		const resolve = vi.fn().mockResolvedValue(new Response('ok'));
		const event = makeEvent('/favicon.png', true);

		const response = await orgHandle({ event, resolve });

		expect(resolve).toHaveBeenCalledWith(event);
		expect(response).toBeInstanceOf(Response);
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

	// sentinel-sourcemaps §2.1, flow U17: outside `dev`, a CI curl's Origin-less multipart POST
	// reaches the route (whatever the route then answers -- 401/201 -- is the route's), never
	// csrfHandle's 403. Without `/api/sourcemaps` in CSRF_EXEMPT_PATHS the upload is unreachable by
	// its only caller in every non-dev deployment.
	it('lets an Origin-less multipart POST through to /api/sourcemaps outside dev (U17)', async () => {
		const resolve = vi.fn().mockResolvedValue(new Response('{"error":{"code":"unauthenticated"}}', { status: 401 }));
		const request = multipartPost('/api/sourcemaps');

		const response = await csrfHandle({ event: csrfEvent('/api/sourcemaps', request), resolve } as any);

		expect(resolve).toHaveBeenCalledTimes(1);
		expect(response.status).toBe(401);
		expect(await response.text()).not.toBe('Cross-site POST form submissions are forbidden');
	});

	it('still 403s an Origin-less multipart POST to /api/sourcemaps/anything -- exact path, never a prefix', async () => {
		const resolve = vi.fn().mockResolvedValue(new Response('ok'));
		const request = multipartPost('/api/sourcemaps/anything');

		const response = await csrfHandle({ event: csrfEvent('/api/sourcemaps/anything', request), resolve } as any);

		expect(response.status).toBe(403);
		expect(resolve).not.toHaveBeenCalled();
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

// sentinel-cli-browser-login (security review, repair round 1): the approval page and its sign-in hop refuse framing,
// on every response they produce (page, __data.json, action, error), and nothing else changes.
describe('frameGuardHandle', () => {
	const run = async (pathname: string, headers: Record<string, string> = {}) =>
		(await frameGuardHandle({
			event: { url: new URL(`http://dash${pathname}`) } as any,
			resolve: async () => new Response('ok', { headers }),
		})) as Response;

	it.each(['/cli/approve', '/cli/approve/', '/cli/approve/__data.json', '/signin', '/signin/__data.json'])(
		'%s gets frame-ancestors none and X-Frame-Options DENY',
		async (pathname) => {
			const res = await run(pathname);
			expect(res.headers.get('content-security-policy')).toBe("frame-ancestors 'none'");
			expect(res.headers.get('x-frame-options')).toBe('DENY');
		}
	);

	it('merges into an existing CSP instead of replacing it', async () => {
		const res = await run('/cli/approve', { 'content-security-policy': "default-src 'self'" });
		expect(res.headers.get('content-security-policy')).toBe("default-src 'self'; frame-ancestors 'none'");
	});

	it.each(['/cli/approvex', '/cli/download/linux/amd64', '/install.sh', '/acme/issues', '/signinx'])('%s is untouched', async (pathname) => {
		const res = await run(pathname);
		expect(res.headers.get('content-security-policy')).toBeNull();
		expect(res.headers.get('x-frame-options')).toBeNull();
	});
});

// sentinel-issue-activity TASK-01: a uuid path parameter carries the `uuid` matcher, so `/api/agent/issues/--help` matches
// no route (`event.route.id === null`). unmatchedApiHandle answers that with SvelteKit's `{ message }` 404 as JSON, before
// any session/org read -- and leaves every routed request, and every non-API path, to the rest of the chain.
describe('unmatchedApiHandle', () => {
	const call = async (pathname: string, routeId: string | null) => {
		const resolve = vi.fn(async () => new Response('routed'));
		const event: any = { url: new URL(`http://localhost${pathname}`), route: { id: routeId } };
		const response = await unmatchedApiHandle({ event, resolve });
		return { response, resolve };
	};

	beforeEach(() => {
		dbMock.select.mockClear();
	});

	it.each(['/api/agent/issues/--help', '/api/issues/42a26f64-/activity', '/api/no-such-thing', '/api'])(
		'%s, matching no route, is a JSON 404 without resolving or reading the database',
		async (pathname) => {
			const { response, resolve } = await call(pathname, null);
			expect(response.status).toBe(404);
			expect(response.headers.get('content-type')).toContain('application/json');
			expect(await response.json()).toEqual({ message: 'Not found' });
			expect(resolve).not.toHaveBeenCalled();
			expect(dbMock.select).not.toHaveBeenCalled();
		}
	);

	it('passes a routed API request through', async () => {
		const { response, resolve } = await call('/api/agent/issues/42a26f64-1b2c-4d5e-8f90-123456789012', '/api/agent/issues/[issueId=uuid]');
		expect(resolve).toHaveBeenCalledTimes(1);
		expect(await response.text()).toBe('routed');
	});

	it('leaves an unmatched PAGE path to SvelteKit (its not-found page), and does not treat /apiary as /api', async () => {
		for (const pathname of ['/issues/42a26f64-', '/apiary']) {
			const { resolve } = await call(pathname, null);
			expect(resolve).toHaveBeenCalledTimes(1);
		}
	});
});
