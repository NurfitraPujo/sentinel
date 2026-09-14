import { describe, it, expect, vi, beforeEach } from 'vitest';
import crypto from 'crypto';

/**
 * sentinel-user-report-issue contract §2 / OQ-3 -- the `report`-scope X-API-Key credential.
 *
 * The load-bearing assertion here is the SCOPE half: an `ingest`-scoped key must get 403
 * `forbidden_scope` in §7's envelope, which is the dashboard-side mirror of the ingestor refusing
 * a `report` key on /ingest (apps/ingestor-go/auth/apikey_test.go). Uses the REAL sha256 so the
 * "hashed exactly like createApiKey" claim is exercised rather than asserted in a comment.
 */

function makeChainable() {
	const m: any = {};
	for (const name of ['select', 'from', 'where']) {
		m[name] = vi.fn(() => m);
	}
	m.then = vi.fn((resolve: any) => resolve([]));
	return m;
}

const dbMock: any = makeChainable();
vi.mock('$lib/server/db', () => ({ db: dbMock }));
vi.mock('$lib/db/schema', () => ({
	projectApiKeys: {
		id: 'id',
		organizationId: 'organizationId',
		projectId: 'projectId',
		scope: 'scope',
		status: 'status',
		expiresAt: 'expiresAt',
		revokedAt: 'revokedAt',
		rateLimitRpm: 'rateLimitRpm',
		keyHash: 'keyHash',
	},
	projects: { id: 'id', name: 'name', organizationId: 'organizationId' },
}));

const { authenticateReportRequest, authenticateSourcemapRequest, REPORT_SCOPES, SOURCEMAP_SCOPES, reportErrorResponse } =
	await import('./report-auth');
const { resetRateLimitKey } = await import('$lib/rate-limit');
const { toReportError } = await import('./report-auth');

function hashKey(raw: string): string {
	return crypto.createHash('sha256').update(raw).digest('hex');
}

/** Queues one result array per db select in call order: key row, then project row. */
function queueResults(...results: unknown[][]) {
	let call = 0;
	dbMock.then = vi.fn((resolve: any) => resolve(results[call++] ?? []));
}

function keyRow(overrides: Record<string, unknown> = {}) {
	return {
		id: 'key-1',
		organizationId: 'org-1',
		projectId: 'proj-1',
		scope: 'report',
		status: 'active',
		expiresAt: null,
		revokedAt: null,
		rateLimitRpm: 5000,
		...overrides,
	};
}

const projectRow = { id: 'proj-1', name: 'checkout' };

function request(headers: Record<string, string> = {}): Request {
	return new Request('http://localhost/api/report/attachments', { method: 'POST', headers });
}

async function envelope(promise: Promise<unknown>) {
	try {
		await promise;
		throw new Error('expected a rejection');
	} catch (err) {
		if (!(err instanceof Response)) throw err;
		return { status: err.status, retryAfter: err.headers.get('Retry-After'), body: await err.json() };
	}
}

beforeEach(() => {
	vi.clearAllMocks();
	// The limiter is module-level state shared across tests in this file.
	for (const id of ['key-1', 'key-2']) {
		resetRateLimitKey(`report-key:${id}`);
		resetRateLimitKey(`sourcemap-key:${id}`);
	}
});

describe('authenticateReportRequest', () => {
	it('admits exactly report and admin', () => {
		expect([...REPORT_SCOPES]).toEqual(['report', 'admin']);
	});

	it('401 unauthenticated when X-API-Key is missing', async () => {
		const res = await envelope(authenticateReportRequest(request()));
		expect(res.status).toBe(401);
		expect(res.body).toEqual({
			error: { code: 'unauthenticated', message: expect.any(String), details: {} },
		});
	});

	it('401 unauthenticated when the key hash matches no row', async () => {
		queueResults([]);
		const res = await envelope(authenticateReportRequest(request({ 'X-API-Key': 'sent_nope' })));
		expect(res.status).toBe(401);
		expect(res.body.error.code).toBe('unauthenticated');
	});

	it('401 for a revoked key, and for an expired one', async () => {
		queueResults([keyRow({ revokedAt: new Date('2026-01-01T00:00:00Z') })]);
		expect((await envelope(authenticateReportRequest(request({ 'X-API-Key': 'k' })))).status).toBe(401);

		queueResults([keyRow({ expiresAt: new Date(Date.now() - 1000) })]);
		expect((await envelope(authenticateReportRequest(request({ 'X-API-Key': 'k' })))).status).toBe(401);
	});

	// The criterion-1 assertion: a key that may POST /ingest may NOT post a report.
	it.each(['ingest', 'read', 'agent'])(
		'403 forbidden_scope for a %s-scoped key, echoing the scope in details',
		async (scope) => {
			queueResults([keyRow({ scope })]);
			const res = await envelope(authenticateReportRequest(request({ 'X-API-Key': 'k' })));
			expect(res.status).toBe(403);
			expect(res.body.error.code).toBe('forbidden_scope');
			expect(res.body.error.details).toEqual({ scope });
		}
	);

	it.each(['report', 'admin'])('admits a %s-scoped project key and fixes the project from the row', async (scope) => {
		queueResults([keyRow({ scope })], [projectRow]);
		const ctx = await authenticateReportRequest(request({ 'X-API-Key': 'k' }));
		expect(ctx).toMatchObject({
			keyId: 'key-1',
			organizationId: 'org-1',
			projectId: 'proj-1',
			projectName: 'checkout',
			scope,
			orgWide: false,
		});
	});

	it('hashes the presented secret with sha256 hex, and looks the row up by that hash', async () => {
		queueResults([keyRow()], [projectRow]);
		await authenticateReportRequest(request({ 'X-API-Key': 'sent_report_secret' }));
		// `where(eq(keyHash, <hash>))` -- the mocked eq() is drizzle's real one over string columns,
		// so assert on the argument the chain received.
		const whereArg = JSON.stringify(dbMock.where.mock.calls[0]?.[0] ?? {});
		expect(whereArg).toContain(hashKey('sent_report_secret'));
	});

	it('400 invalid_request when an org-wide key names no project', async () => {
		queueResults([keyRow({ projectId: null })]);
		const res = await envelope(authenticateReportRequest(request({ 'X-API-Key': 'k' })));
		expect(res.status).toBe(400);
		expect(res.body.error).toMatchObject({ code: 'invalid_request', details: { field: 'X-Project-Key' } });
	});

	it('403 project_not_found — not 404 — when X-Project-Key is outside the key\'s organization', async () => {
		queueResults([keyRow({ projectId: null })], []);
		const res = await envelope(
			authenticateReportRequest(request({ 'X-API-Key': 'k', 'X-Project-Key': 'someone-elses' }))
		);
		expect(res.status).toBe(403);
		expect(res.body.error.code).toBe('project_not_found');
	});

	it('resolves an org-wide key through X-Project-Key within its own organization', async () => {
		queueResults([keyRow({ projectId: null })], [projectRow]);
		const ctx = await authenticateReportRequest(
			request({ 'X-API-Key': 'k', 'X-Project-Key': 'checkout' })
		);
		expect(ctx).toMatchObject({ projectId: 'proj-1', projectName: 'checkout', orgWide: true });
	});

	it("429 rate_limited with Retry-After once the key's own rate_limit_rpm is spent", async () => {
		for (let i = 0; i < 2; i++) {
			queueResults([keyRow({ id: 'key-2', rateLimitRpm: 2 })], [projectRow]);
			await authenticateReportRequest(request({ 'X-API-Key': 'k' }));
		}
		queueResults([keyRow({ id: 'key-2', rateLimitRpm: 2 })], [projectRow]);
		const res = await envelope(authenticateReportRequest(request({ 'X-API-Key': 'k' })));
		expect(res.status).toBe(429);
		expect(res.body.error.code).toBe('rate_limited');
		expect(Number(res.retryAfter)).toBeGreaterThan(0);
	});

	it('checks scope BEFORE the limiter, so a wrong-scope key cannot be masked by a 429', async () => {
		queueResults([keyRow({ id: 'key-2', scope: 'ingest', rateLimitRpm: 0 })]);
		const res = await envelope(authenticateReportRequest(request({ 'X-API-Key': 'k' })));
		expect(res.status).toBe(403);
	});
});

/**
 * sentinel-sourcemaps contract §2.1 (decision D2, be.md task 7): the SAME authenticator under the
 * `{ingest, admin}` allowlist -- `apikey.go: ScopesIngest`'s set -- for `POST /api/sourcemaps`.
 * What is load-bearing: a `report` key (the intake's own credential) is 403 here, the mirror image
 * of the `ingest` case above; `resolveReportProject` is reused as it is (decision 24), so the three
 * project answers are byte-identical to the intake's; and the limiter bucket is the route's own.
 */
describe('authenticateSourcemapRequest (sentinel-sourcemaps §2.1)', () => {
	it('admits exactly ingest and admin -- apikey.go ScopesIngest, one rule', () => {
		expect([...SOURCEMAP_SCOPES]).toEqual(['ingest', 'admin']);
	});

	it('401 unauthenticated when X-API-Key is missing (U5)', async () => {
		const res = await envelope(authenticateSourcemapRequest(request()));
		expect(res.status).toBe(401);
		expect(res.body.error.code).toBe('unauthenticated');
	});

	it('401 for an unknown, a revoked and an expired key (U5)', async () => {
		queueResults([]);
		expect((await envelope(authenticateSourcemapRequest(request({ 'X-API-Key': 'nope' })))).status).toBe(401);
		queueResults([keyRow({ scope: 'ingest', revokedAt: new Date('2026-01-01T00:00:00Z') })]);
		expect((await envelope(authenticateSourcemapRequest(request({ 'X-API-Key': 'k' })))).status).toBe(401);
		queueResults([keyRow({ scope: 'ingest', expiresAt: new Date(Date.now() - 1000) })]);
		expect((await envelope(authenticateSourcemapRequest(request({ 'X-API-Key': 'k' })))).status).toBe(401);
	});

	// U4: the intake's own credential may NOT upload maps -- the allowlist is per route.
	it.each(['report', 'read', 'agent'])('403 forbidden_scope for a %s-scoped key (U4)', async (scope) => {
		queueResults([keyRow({ scope })]);
		const res = await envelope(authenticateSourcemapRequest(request({ 'X-API-Key': 'k' })));
		expect(res.status).toBe(403);
		expect(res.body.error).toMatchObject({ code: 'forbidden_scope', details: { scope } });
	});

	it.each(['ingest', 'admin'])('admits an %s-scoped project key and fixes the project from the row', async (scope) => {
		queueResults([keyRow({ scope })], [projectRow]);
		const ctx = await authenticateSourcemapRequest(request({ 'X-API-Key': 'k' }));
		expect(ctx).toMatchObject({ keyId: 'key-1', organizationId: 'org-1', projectId: 'proj-1', scope, orgWide: false });
	});

	it('400 invalid_request { field: X-Project-Key } for an org-wide key with no header (U6, decision 24)', async () => {
		queueResults([keyRow({ scope: 'ingest', projectId: null })]);
		const res = await envelope(authenticateSourcemapRequest(request({ 'X-API-Key': 'k' })));
		expect(res.status).toBe(400);
		expect(res.body.error).toMatchObject({ code: 'invalid_request', details: { field: 'X-Project-Key' } });
	});

	it('403 project_not_found for an org-wide key naming a project outside its organization (U6b)', async () => {
		queueResults([keyRow({ scope: 'ingest', projectId: null })], []);
		const res = await envelope(
			authenticateSourcemapRequest(request({ 'X-API-Key': 'k', 'X-Project-Key': 'someone-elses' }))
		);
		expect(res.status).toBe(403);
		expect(res.body.error).toEqual({ code: 'project_not_found', message: expect.any(String), details: {} });
	});

	it('403 project_not_found for a PROJECT-SCOPED key naming a project outside its organization (U6b)', async () => {
		queueResults([keyRow({ scope: 'ingest' })], []);
		const res = await envelope(
			authenticateSourcemapRequest(request({ 'X-API-Key': 'k', 'X-Project-Key': 'someone-elses' }))
		);
		expect(res.status).toBe(403);
		expect(res.body.error.code).toBe('project_not_found');
	});

	it('resolves an org-wide key through X-Project-Key within its own organization', async () => {
		queueResults([keyRow({ scope: 'ingest', projectId: null })], [projectRow]);
		const ctx = await authenticateSourcemapRequest(request({ 'X-API-Key': 'k', 'X-Project-Key': 'checkout' }));
		expect(ctx).toMatchObject({ projectId: 'proj-1', projectName: 'checkout', orgWide: true });
	});

	it("429 rate_limited with Retry-After once the key's rate_limit_rpm is spent (U14)", async () => {
		for (let i = 0; i < 2; i++) {
			queueResults([keyRow({ id: 'key-2', scope: 'ingest', rateLimitRpm: 2 })], [projectRow]);
			await authenticateSourcemapRequest(request({ 'X-API-Key': 'k' }));
		}
		queueResults([keyRow({ id: 'key-2', scope: 'ingest', rateLimitRpm: 2 })], [projectRow]);
		const res = await envelope(authenticateSourcemapRequest(request({ 'X-API-Key': 'k' })));
		expect(res.status).toBe(429);
		expect(res.body.error.code).toBe('rate_limited');
		expect(Number(res.retryAfter)).toBeGreaterThan(0);
	});

	it("keeps its own limiter bucket: spending the sourcemap budget does not spend the report one", async () => {
		for (let i = 0; i < 2; i++) {
			queueResults([keyRow({ id: 'key-2', scope: 'admin', rateLimitRpm: 2 })], [projectRow]);
			await authenticateSourcemapRequest(request({ 'X-API-Key': 'k' }));
		}
		queueResults([keyRow({ id: 'key-2', scope: 'admin', rateLimitRpm: 2 })], [projectRow]);
		expect((await envelope(authenticateSourcemapRequest(request({ 'X-API-Key': 'k' })))).status).toBe(429);
		// The same admin key, on the intake route, still has its two.
		queueResults([keyRow({ id: 'key-2', scope: 'admin', rateLimitRpm: 2 })], [projectRow]);
		await expect(authenticateReportRequest(request({ 'X-API-Key': 'k' }))).resolves.toMatchObject({ keyId: 'key-2' });
	});

	it('checks scope BEFORE the limiter, so a wrong-scope key cannot be masked by a 429', async () => {
		queueResults([keyRow({ id: 'key-2', scope: 'report', rateLimitRpm: 0 })]);
		const res = await envelope(authenticateSourcemapRequest(request({ 'X-API-Key': 'k' })));
		expect(res.status).toBe(403);
	});
});

/**
 * `toReportError` wraps two shapes of thrown status into §7's envelope: SvelteKit's `error()`
 * (`body.message`) and the Node adapter's own `SvelteKitError` (an `Error` with `status` and the
 * text in `message`) -- the BODY_SIZE_LIMIT 413 sentinel-sourcemaps §4.1 / flow U10 must wrap.
 */
describe('toReportError', () => {
	it('keeps body.message for a SvelteKit error()', async () => {
		const res = toReportError({ status: 413, body: { message: 'File exceeds the cap' } });
		expect(res.status).toBe(413);
		expect(await res.json()).toEqual({ error: { code: 'payload_too_large', message: 'File exceeds the cap', details: {} } });
	});

	it("keeps the adapter's message for an Error carrying a non-500 status (BODY_SIZE_LIMIT's 413)", async () => {
		const err = Object.assign(new Error('Content-length of 125829632 exceeds limit of 115343360 bytes.'), {
			status: 413,
			text: 'Payload Too Large',
		});
		const res = toReportError(err);
		expect(res.status).toBe(413);
		expect(await res.json()).toEqual({
			error: {
				code: 'payload_too_large',
				message: 'Content-length of 125829632 exceeds limit of 115343360 bytes.',
				details: {},
			},
		});
	});

	it('never leaks an unexpected throw\'s message: a plain Error is 500 internal "Internal error"', async () => {
		const res = toReportError(new Error('ECONNREFUSED 10.0.0.7:5432'));
		expect(res.status).toBe(500);
		expect(await res.json()).toEqual({ error: { code: 'internal', message: 'Internal error', details: {} } });
	});

	it('returns a thrown Response as it is', () => {
		const original = reportErrorResponse(403, 'forbidden_scope', 'no');
		expect(toReportError(original)).toBe(original);
	});
});
