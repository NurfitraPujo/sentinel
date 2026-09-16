// @vitest-environment node
//
// The one file in this app that opts out of the jsdom default: the route reads each part's bytes
// with `File.arrayBuffer()`, which jsdom 25's `File` does not implement and Node's (undici's) --
// the one adapter-node hands the route in production -- does. Running under jsdom would test a
// polyfill's gap, not this route.
import { describe, it, expect, vi, beforeEach } from 'vitest';
import crypto from 'node:crypto';

/**
 * sentinel-sourcemaps contract §4.1 / §7 / flows U1–U16, U18 -- `POST /api/sourcemaps`, in
 * `report-attachments.test.ts`'s shape: the authenticator, storage, the query module and the
 * metrics are mocked, and what is asserted is what the route ITSELF decides -- the §7 envelope
 * on every non-2xx with each row's status, code and details; positional pairing; the validation
 * order; all-or-nothing; decision 26's three phases (the pre-check before the first putObject,
 * putObject with no transaction open, ONE upsert call after every object is in, the replaced
 * cleanup after commit); and which of the five metric outcomes each path records.
 *
 * U17 (the CSRF exemption) lives in `hooks.server.test.ts`; the authenticator's own rows (U4, U5,
 * U6, U6b, U14) are proved against the real function in `report-auth.test.ts` and are asserted
 * here as "the route returns the authenticator's §7 response verbatim, and records `rejected`".
 */

const authenticateSourcemapRequest = vi.fn();
const putObject = vi.fn();
const headObject = vi.fn();
const isStorageConfigured = vi.fn();
const readReleaseState = vi.fn();
const upsertReleaseSourcemaps = vi.fn();
const findReferencedStorageKeys = vi.fn();
const bestEffortDeleteObjects = vi.fn();
const recordSourcemapUpload = vi.fn();

const mockEnv: Record<string, string | undefined> = {};

vi.mock('$env/dynamic/private', () => ({ env: mockEnv }));
vi.mock('$lib/server/report-auth', async () => {
	const actual = await vi.importActual<typeof import('$lib/server/report-auth')>('$lib/server/report-auth');
	return { ...actual, authenticateSourcemapRequest };
});
vi.mock('$lib/server/storage', () => ({ putObject, headObject, isStorageConfigured }));
vi.mock('$lib/server/retention', () => ({ bestEffortDeleteObjects }));
vi.mock('$lib/db/queries/sourcemaps', async () => {
	const actual = await vi.importActual<typeof import('$lib/db/queries/sourcemaps')>('$lib/db/queries/sourcemaps');
	return { ...actual, readReleaseState, upsertReleaseSourcemaps, findReferencedStorageKeys };
});
vi.mock('$lib/server/metrics', () => ({ recordSourcemapUpload }));

const { POST } = await import('./+server');
const { ReleaseFullError } = await import('$lib/db/queries/sourcemaps');
const { reportErrorResponse } = await import('$lib/server/report-auth');
const { sourcemapStorageKey, h16 } = await import('$lib/server/sourcemap-path');

const CTX = {
	keyId: 'key-1',
	organizationId: 'org-1',
	projectId: '7c9e6679-7425-40de-944b-e07fc1f90ae7',
	projectName: 'checkout',
	scope: 'ingest',
	rateLimitRpm: 5000,
	orgWide: false,
};

const RELEASE = '2026.09.14-3f2a1c';

function sha256(bytes: Uint8Array | string): string {
	return crypto.createHash('sha256').update(bytes).digest('hex');
}

/** A minimal v3 map; `seed` makes distinct byte content. */
function mapBytes(seed = 'a', extra: Record<string, unknown> = {}): Uint8Array {
	return new TextEncoder().encode(
		JSON.stringify({ version: 3, file: 'x.js', sources: [`src/${seed}.ts`], names: [], mappings: 'AAAA', ...extra })
	);
}

/** `File` wants a `BlobPart`; TS 5.7's `Uint8Array<ArrayBufferLike>` is not one, so hand it a Blob. */
function fileOf(bytes: Uint8Array, name: string, type?: string): File {
	return new File([new Blob([bytes as unknown as ArrayBuffer])], name, type ? { type } : undefined);
}

function mapFile(seed = 'a', extra: Record<string, unknown> = {}): File {
	return fileOf(mapBytes(seed, extra), `${seed}.js.map`, 'application/json');
}

function rawFile(text: string): File {
	return fileOf(new TextEncoder().encode(text), 'x.map');
}

function makeRequest(form: FormData): Request {
	const req = new Request('http://localhost/api/sourcemaps', { method: 'POST' });
	// `request.formData()` is stubbed rather than serialised: the route only ever reads parts, and
	// building a real multipart body here would test undici, not this route.
	(req as any).formData = async () => form;
	return req;
}

/** Builds the positional form: release_version, then (minified_path, file) pairs in order. */
function form(
	pairs: Array<[string, File]>,
	releaseVersion: string | null = RELEASE,
	extraPaths: string[] = [],
	extraFiles: File[] = []
): FormData {
	const fd = new FormData();
	if (releaseVersion !== null) fd.append('release_version', releaseVersion);
	for (const [path, file] of pairs) {
		fd.append('minified_path', path);
		fd.append('file', file);
	}
	for (const p of extraPaths) fd.append('minified_path', p);
	for (const f of extraFiles) fd.append('file', f);
	return fd;
}

async function post(fd: FormData) {
	const res = await POST({ request: makeRequest(fd) } as any);
	return { res, body: await res.clone().json() };
}

function outcomesRecorded(): string[] {
	return recordSourcemapUpload.mock.calls.map((c) => c[0]);
}

beforeEach(() => {
	vi.clearAllMocks();
	for (const k of Object.keys(mockEnv)) delete mockEnv[k];
	authenticateSourcemapRequest.mockResolvedValue(CTX);
	isStorageConfigured.mockReturnValue(true);
	putObject.mockResolvedValue(undefined);
	headObject.mockResolvedValue({ contentLength: 1 });
	readReleaseState.mockResolvedValue({ current: 0, existing: new Map() });
	// The default transaction mock behaves as the real one does on this point: it runs the
	// route's `ensureObjectsPresent` for every key before answering (decision 43).
	upsertReleaseSourcemaps.mockImplementation(async ({ files, ensureObjectsPresent }: any) => {
		await ensureObjectsPresent(files.map((f: any) => f.storageKey));
		return { releaseFileCount: files.length, displacedKeys: [] };
	});
	findReferencedStorageKeys.mockResolvedValue(new Set());
	bestEffortDeleteObjects.mockResolvedValue([]);
});

describe('POST /api/sourcemaps -- the happy paths (U1, U2, U3, U11, U13)', () => {
	it('U1: 201 with files[] in request order, each created, objects put before the one upsert', async () => {
		const a = mapFile('a');
		const b = mapFile('b');
		const { res, body } = await post(form([['/assets/index-3f2a1c.js', a], ['/assets/vendor-9a0d4e.js', b]]));

		expect(res.status).toBe(201);
		expect(body).toEqual({
			project_id: CTX.projectId,
			release_version: RELEASE,
			files: [
				{ minified_path: '/assets/index-3f2a1c.js', sha256: sha256(mapBytes('a')), size_bytes: mapBytes('a').byteLength, outcome: 'created' },
				{ minified_path: '/assets/vendor-9a0d4e.js', sha256: sha256(mapBytes('b')), size_bytes: mapBytes('b').byteLength, outcome: 'created' },
			],
			release_file_count: 2,
		});

		// Phase 2 then phase 3: every putObject strictly before the single upsert call.
		expect(putObject).toHaveBeenCalledTimes(2);
		expect(upsertReleaseSourcemaps).toHaveBeenCalledTimes(1);
		const lastPut = Math.max(...putObject.mock.invocationCallOrder);
		expect(upsertReleaseSourcemaps.mock.invocationCallOrder[0]).toBeGreaterThan(lastPut);

		// The key: sourcemaps/{project}/{h16(release)}/{h16(path)}-{sha256}.map, content-type JSON.
		const expectedKeyA = sourcemapStorageKey(CTX.projectId, RELEASE, '/assets/index-3f2a1c.js', sha256(mapBytes('a')));
		expect(expectedKeyA).toBe(`sourcemaps/${CTX.projectId}/${h16(RELEASE)}/${h16('/assets/index-3f2a1c.js')}-${sha256(mapBytes('a'))}.map`);
		expect(putObject).toHaveBeenNthCalledWith(1, expectedKeyA, expect.any(Uint8Array), 'application/json');
		expect(Buffer.from(putObject.mock.calls[0][1])).toEqual(Buffer.from(mapBytes('a')));

		expect(upsertReleaseSourcemaps).toHaveBeenCalledWith({
			projectId: CTX.projectId,
			releaseVersion: RELEASE,
			files: [
				{ minifiedPath: '/assets/index-3f2a1c.js', storageKey: expectedKeyA, sizeBytes: mapBytes('a').byteLength, sha256: sha256(mapBytes('a')) },
				expect.objectContaining({ minifiedPath: '/assets/vendor-9a0d4e.js' }),
			],
			maxPerRelease: 2000,
			ensureObjectsPresent: expect.any(Function),
		});
		expect(bestEffortDeleteObjects).not.toHaveBeenCalled();
		expect(outcomesRecorded()).toEqual(['created', 'created']);
	});

	it('U2: the identical re-post is 200, every file unchanged, nothing put and nothing upserted', async () => {
		const existing = new Map([
			['/a.js', { sha256: sha256(mapBytes('a')), storageKey: 'k-a' }],
			['/b.js', { sha256: sha256(mapBytes('b')), storageKey: 'k-b' }],
		]);
		readReleaseState.mockResolvedValue({ current: 2, existing });
		const { res, body } = await post(form([['/a.js', mapFile('a')], ['/b.js', mapFile('b')]]));

		expect(res.status).toBe(200);
		expect(body.files.map((f: any) => f.outcome)).toEqual(['unchanged', 'unchanged']);
		expect(body.release_file_count).toBe(2);
		expect(putObject).not.toHaveBeenCalled();
		expect(upsertReleaseSourcemaps).not.toHaveBeenCalled();
		expect(outcomesRecorded()).toEqual(['unchanged', 'unchanged']);
	});

	it('U3: one path with different bytes is replaced (201), others unchanged, the old object deleted AFTER commit', async () => {
		const existing = new Map([
			['/a.js', { sha256: sha256(mapBytes('a')), storageKey: 'old-key-a' }],
			['/b.js', { sha256: sha256(mapBytes('b')), storageKey: 'old-key-b' }],
		]);
		readReleaseState.mockResolvedValue({ current: 2, existing });
		upsertReleaseSourcemaps.mockResolvedValue({ releaseFileCount: 2, displacedKeys: ['old-key-a'] });
		const { res, body } = await post(form([['/a.js', mapFile('a', { names: ['rebuilt'] })], ['/b.js', mapFile('b')]]));

		expect(res.status).toBe(201);
		expect(body.files.map((f: any) => f.outcome)).toEqual(['replaced', 'unchanged']);
		expect(body.release_file_count).toBe(2);
		expect(putObject).toHaveBeenCalledTimes(1);
		expect(upsertReleaseSourcemaps).toHaveBeenCalledWith(
			expect.objectContaining({ files: [expect.objectContaining({ minifiedPath: '/a.js' })] })
		);
		expect(findReferencedStorageKeys).toHaveBeenCalledWith(['old-key-a']);
		expect(bestEffortDeleteObjects).toHaveBeenCalledWith(['old-key-a'], 'sourcemaps.replaced');
		expect(findReferencedStorageKeys.mock.invocationCallOrder[0]).toBeGreaterThan(
			upsertReleaseSourcemaps.mock.invocationCallOrder[0]
		);
		expect(bestEffortDeleteObjects.mock.invocationCallOrder[0]).toBeGreaterThan(
			findReferencedStorageKeys.mock.invocationCallOrder[0]
		);
		expect(outcomesRecorded()).toEqual(['replaced', 'unchanged']);
	});

	it('counts a failed replaced-cleanup delete as storage_error, and still answers 201', async () => {
		readReleaseState.mockResolvedValue({
			current: 1,
			existing: new Map([['/a.js', { sha256: 'stale', storageKey: 'old-key-a' }]]),
		});
		upsertReleaseSourcemaps.mockResolvedValue({ releaseFileCount: 1, displacedKeys: ['old-key-a'] });
		bestEffortDeleteObjects.mockResolvedValue(['old-key-a']);
		const { res } = await post(form([['/a.js', mapFile('a')]]));
		expect(res.status).toBe(201);
		expect(outcomesRecorded()).toEqual(['storage_error', 'replaced']);
	});

	it('U11: the stored and echoed minified_path is the NORMALISED one', async () => {
		const { res, body } = await post(form([['https://app.example.com/assets/x.js?v=1', mapFile('a')]]));
		expect(res.status).toBe(201);
		expect(body.files[0].minified_path).toBe('/assets/x.js');
		expect(readReleaseState).toHaveBeenCalledWith(CTX.projectId, RELEASE, ['/assets/x.js']);
		expect(upsertReleaseSourcemaps).toHaveBeenCalledWith(
			expect.objectContaining({ files: [expect.objectContaining({ minifiedPath: '/assets/x.js' })] })
		);
		expect(putObject).toHaveBeenCalledWith(
			sourcemapStorageKey(CTX.projectId, RELEASE, '/assets/x.js', sha256(mapBytes('a'))),
			expect.anything(),
			'application/json'
		);
	});

	it('U13: a version no event has carried is a plain 201 -- the route consults no releases row', async () => {
		const { res } = await post(form([['/a.js', mapFile('a')]], 'never-seen-9.9.9'));
		expect(res.status).toBe(201);
		expect(readReleaseState).toHaveBeenCalledWith(CTX.projectId, 'never-seen-9.9.9', ['/a.js']);
	});

	it('echoes release_version verbatim and uses the credential\'s project, never a form part (B7)', async () => {
		const fd = form([['/a.js', mapFile('a')]], 'V1.0.0-RC');
		fd.append('project_id', 'someone-elses');
		const { body } = await post(fd);
		expect(body.release_version).toBe('V1.0.0-RC');
		expect(body.project_id).toBe(CTX.projectId);
		expect(readReleaseState).toHaveBeenCalledWith(CTX.projectId, 'V1.0.0-RC', ['/a.js']);
	});

	it('ignores unknown multipart parts (§3): a script that also posts dist_dir= is not rejected', async () => {
		const fd = form([['/a.js', mapFile('a')]]);
		fd.append('dist_dir', 'dist');
		expect((await post(fd)).res.status).toBe(201);
	});
});

describe('POST /api/sourcemaps -- the replaced cleanup deletes what the transaction displaced, if nothing references it (BUG-05)', () => {
	// The review's scenario: row {S0,K0}. Request A replaced it with K1 and deleted K0. Request C
	// (a CI retry of the older artifact) then put K0 again and committed the row back to K0,
	// displacing K1. THIS request B read phase 1 BEFORE A -- its snapshot says the previous key is
	// K0 -- and is only now committing K1. What B displaced is K0 (again), what it must not touch
	// is anything else; and if C's row still points at K0 when B looks, K0 stays.
	const K0 = 'sourcemaps/p/r/a-000.map';
	const K1 = 'sourcemaps/p/r/a-111.map';
	const existingA = () => new Map([['/a.js', { sha256: 'stale-snapshot', storageKey: K0 }]]);

	it('deletes the key the transaction displaced, not the key phase 1 snapshotted', async () => {
		readReleaseState.mockResolvedValue({ current: 1, existing: existingA() });
		// Between phase 1 and phase 3 another request moved the row to K1: that is the key B
		// displaced, and the only one it may consider deleting.
		upsertReleaseSourcemaps.mockResolvedValue({ releaseFileCount: 1, displacedKeys: [K1] });
		const { res } = await post(form([['/a.js', mapFile('a')]]));
		expect(res.status).toBe(201);
		expect(findReferencedStorageKeys).toHaveBeenCalledWith([K1]);
		expect(bestEffortDeleteObjects).toHaveBeenCalledWith([K1], 'sourcemaps.replaced');
		expect(bestEffortDeleteObjects).not.toHaveBeenCalledWith(expect.arrayContaining([K0]), expect.anything());
	});

	it('does not delete a displaced key that a sourcemaps row still references after commit', async () => {
		readReleaseState.mockResolvedValue({ current: 1, existing: existingA() });
		upsertReleaseSourcemaps.mockResolvedValue({ releaseFileCount: 1, displacedKeys: [K0] });
		// Request C re-committed a row to K0 between B's commit and B's reference check.
		findReferencedStorageKeys.mockResolvedValue(new Set([K0]));
		const { res } = await post(form([['/a.js', mapFile('a')]]));
		expect(res.status).toBe(201);
		expect(bestEffortDeleteObjects).not.toHaveBeenCalled();
		expect(outcomesRecorded()).toEqual(['replaced']);
	});

	it('never deletes a key this request itself just put, even if the transaction reports it displaced', async () => {
		readReleaseState.mockResolvedValue({ current: 1, existing: existingA() });
		const bytes = mapBytes('a');
		const putKey = sourcemapStorageKey(CTX.projectId, RELEASE, '/a.js', sha256(bytes));
		upsertReleaseSourcemaps.mockResolvedValue({ releaseFileCount: 1, displacedKeys: [putKey, K0] });
		const { res } = await post(form([['/a.js', fileOf(bytes, 'a.js.map', 'application/json')]]));
		expect(res.status).toBe(201);
		expect(putObject).toHaveBeenCalledWith(putKey, expect.anything(), 'application/json');
		expect(findReferencedStorageKeys).toHaveBeenCalledWith([K0]);
		expect(bestEffortDeleteObjects).toHaveBeenCalledWith([K0], 'sourcemaps.replaced');
	});

	it('when the transaction displaced nothing, neither the reference check nor a delete runs', async () => {
		readReleaseState.mockResolvedValue({ current: 1, existing: existingA() });
		upsertReleaseSourcemaps.mockResolvedValue({ releaseFileCount: 1, displacedKeys: [] });
		const { res, body } = await post(form([['/a.js', mapFile('a')]]));
		expect(res.status).toBe(201);
		expect(body.files[0].outcome).toBe('replaced');
		expect(findReferencedStorageKeys).not.toHaveBeenCalled();
		expect(bestEffortDeleteObjects).not.toHaveBeenCalled();
	});

	it('a reference check that fails leaves the object in place (an orphan, counted), never a row pointing at nothing', async () => {
		readReleaseState.mockResolvedValue({ current: 1, existing: existingA() });
		upsertReleaseSourcemaps.mockResolvedValue({ releaseFileCount: 1, displacedKeys: [K0] });
		findReferencedStorageKeys.mockRejectedValue(new Error('db down'));
		const { res } = await post(form([['/a.js', mapFile('a')]]));
		expect(res.status).toBe(201);
		expect(bestEffortDeleteObjects).not.toHaveBeenCalled();
		expect(outcomesRecorded()).toEqual(['storage_error', 'replaced']);
	});
});

describe('POST /api/sourcemaps -- every key a row is committed to is confirmed under the lock, and re-put if gone (decision 43, BUG-07)', () => {
	// The review's interleaving, one level up from BUG-05: THIS request B put K1 in phase 2. Before
	// B's transaction took the lock, C (a CI retry of the older artifact) committed its row back
	// to K0, found no row on K1 -- B has not committed -- and deleted K1. B's `ensureObjectsPresent`
	// is what the transaction runs under the lock before it writes B's row: HEAD K1, and put it
	// again from the bytes B still holds when the bucket says it is gone.
	const notFound = () => Object.assign(new Error('NotFound'), { name: 'NotFound', $metadata: { httpStatusCode: 404 } });
	const bytesA = mapBytes('a');
	const keyA = () => sourcemapStorageKey(CTX.projectId, RELEASE, '/a.js', sha256(bytesA));

	it('hands the transaction an ensureObjectsPresent that HEADs each key and puts nothing when the object is there', async () => {
		const { res } = await post(form([['/a.js', fileOf(bytesA, 'a.js.map', 'application/json')]]));
		expect(res.status).toBe(201);
		expect(headObject).toHaveBeenCalledTimes(1);
		expect(headObject).toHaveBeenCalledWith(keyA());
		// One put in phase 2, none from the check.
		expect(putObject).toHaveBeenCalledTimes(1);
		expect(outcomesRecorded()).toEqual(['created']);
	});

	it('re-puts the part\'s own bytes under the key when HEAD says the object is gone, then the row is written and the request is 201', async () => {
		headObject.mockRejectedValueOnce(notFound());
		const { res, body } = await post(form([['/a.js', fileOf(bytesA, 'a.js.map', 'application/json')]]));
		expect(res.status).toBe(201);
		expect(body.files[0].outcome).toBe('created');
		expect(putObject).toHaveBeenCalledTimes(2);
		expect(putObject).toHaveBeenNthCalledWith(2, keyA(), expect.any(Uint8Array), 'application/json');
		expect(Buffer.from(putObject.mock.calls[1][1])).toEqual(Buffer.from(bytesA));
		// The repair happened INSIDE the transaction call, not after it.
		expect(putObject.mock.invocationCallOrder[1]).toBeGreaterThan(upsertReleaseSourcemaps.mock.invocationCallOrder[0]);
		expect(outcomesRecorded()).toEqual(['created']);
	});

	it('checks every key the transaction names, in order, and repairs only the missing ones', async () => {
		const bytesB = mapBytes('b');
		const keyB = sourcemapStorageKey(CTX.projectId, RELEASE, '/b.js', sha256(bytesB));
		headObject.mockImplementation(async (key: string) => {
			if (key === keyB) throw notFound();
			return { contentLength: 1 };
		});
		const { res } = await post(
			form([
				['/a.js', fileOf(bytesA, 'a.js.map', 'application/json')],
				['/b.js', fileOf(bytesB, 'b.js.map', 'application/json')],
			])
		);
		expect(res.status).toBe(201);
		expect(headObject.mock.calls.map((c) => c[0])).toEqual([keyA(), keyB]);
		expect(putObject).toHaveBeenCalledTimes(3);
		expect(putObject).toHaveBeenNthCalledWith(3, keyB, expect.any(Uint8Array), 'application/json');
		expect(Buffer.from(putObject.mock.calls[2][1])).toEqual(Buffer.from(bytesB));
	});

	it('a HEAD that fails for any reason but "not found" aborts the transaction: 503 storage_unavailable, nothing put again, counted as storage_error', async () => {
		headObject.mockRejectedValueOnce(Object.assign(new Error('AccessDenied'), { name: 'AccessDenied', $metadata: { httpStatusCode: 403 } }));
		const { res, body } = await post(form([['/a.js', fileOf(bytesA, 'a.js.map', 'application/json')]]));
		expect(res.status).toBe(503);
		expect(body.error).toEqual({ code: 'storage_unavailable', message: expect.any(String), details: {} });
		expect(putObject).toHaveBeenCalledTimes(1);
		expect(bestEffortDeleteObjects).not.toHaveBeenCalled();
		expect(outcomesRecorded()).toEqual(['storage_error']);
	});

	it('a re-put that fails aborts the transaction the same way: 503, no row, counted', async () => {
		headObject.mockRejectedValueOnce(notFound());
		putObject.mockResolvedValueOnce(undefined).mockRejectedValueOnce(new Error('bucket gone'));
		const { res, body } = await post(form([['/a.js', fileOf(bytesA, 'a.js.map', 'application/json')]]));
		expect(res.status).toBe(503);
		expect(body.error.code).toBe('storage_unavailable');
		expect(outcomesRecorded()).toEqual(['storage_error']);
	});

	it('the transaction\'s own failure after the check is still 500 internal -- the repair error is the only storage answer mapped to 503', async () => {
		upsertReleaseSourcemaps.mockImplementation(async ({ files, ensureObjectsPresent }: any) => {
			await ensureObjectsPresent(files.map((f: any) => f.storageKey));
			throw new Error('connection reset');
		});
		const { res, body } = await post(form([['/a.js', fileOf(bytesA, 'a.js.map', 'application/json')]]));
		expect(res.status).toBe(500);
		expect(body.error.code).toBe('internal');
	});
});

describe('POST /api/sourcemaps -- the authenticator\'s answers, verbatim (U4, U5, U6, U6b, U14)', () => {
	it.each([
		['U4', 403, 'forbidden_scope', { scope: 'report' }],
		['U5', 401, 'unauthenticated', {}],
		['U6', 400, 'invalid_request', { field: 'X-Project-Key' }],
		['U6b (org-wide key, foreign project)', 403, 'project_not_found', {}],
		['U6b (project-scoped key, foreign project)', 403, 'project_not_found', {}],
	])('%s: propagates %i %s with its details, records rejected, reads nothing', async (_flow, status, code, details) => {
		authenticateSourcemapRequest.mockRejectedValue(reportErrorResponse(status, code as any, 'no', details));
		const { res, body } = await post(form([['/a.js', mapFile('a')]]));
		expect(res.status).toBe(status);
		expect(body.error).toEqual({ code, message: 'no', details });
		expect(readReleaseState).not.toHaveBeenCalled();
		expect(putObject).not.toHaveBeenCalled();
		expect(outcomesRecorded()).toEqual(['rejected']);
	});

	it('U14: 429 rate_limited keeps its Retry-After header', async () => {
		authenticateSourcemapRequest.mockRejectedValue(
			reportErrorResponse(429, 'rated_limited' as any, 'slow down', {}, { 'Retry-After': '17' })
		);
		const { res } = await post(form([['/a.js', mapFile('a')]]));
		expect(res.status).toBe(429);
		expect(res.headers.get('Retry-After')).toBe('17');
	});

	it('authenticates BEFORE the body is read: a 401 is never shadowed by a 413 from a body nobody should have read', async () => {
		authenticateSourcemapRequest.mockRejectedValue(reportErrorResponse(401, 'unauthenticated', 'no'));
		const formDataSpy = vi.fn(async () => {
			throw { status: 413, body: { message: 'Content-length exceeds limit' } };
		});
		const req = new Request('http://localhost/api/sourcemaps', { method: 'POST' });
		(req as any).formData = formDataSpy;
		const res = await POST({ request: req } as any);
		expect(res.status).toBe(401);
		expect(formDataSpy).not.toHaveBeenCalled();
	});
});

describe('POST /api/sourcemaps -- the 400 table', () => {
	it('400 invalid_request { field: body } for a body that is not multipart', async () => {
		const req = new Request('http://localhost/api/sourcemaps', { method: 'POST' });
		(req as any).formData = async () => {
			throw new TypeError('Could not parse content as FormData');
		};
		const res = await POST({ request: req } as any);
		expect(res.status).toBe(400);
		expect((await res.json()).error).toMatchObject({ code: 'invalid_request', details: { field: 'body' } });
		expect(outcomesRecorded()).toEqual(['rejected']);
	});

	it('400 { field: release_version } when release_version is missing, empty, or over 100 bytes', async () => {
		for (const version of [null, '', 'v'.repeat(101), 'é'.repeat(51)]) {
			vi.clearAllMocks();
			const { res, body } = await post(form([['/a.js', mapFile('a')]], version));
			expect(res.status).toBe(400);
			expect(body.error).toMatchObject({ code: 'invalid_request', details: { field: 'release_version' } });
			expect(body.error.details.reason).toBeUndefined();
			expect(readReleaseState).not.toHaveBeenCalled();
		}
		// Exactly 100 bytes is accepted.
		expect((await post(form([['/a.js', mapFile('a')]], 'v'.repeat(100)))).res.status).toBe(201);
	});

	it.each(['1.2.3\n', ' 1.2.3', '1.2.3 ', '\t1.2.3\r\n'])(
		'U16: 400 { field: release_version, reason: whitespace } for %j -- refused, never trimmed',
		async (version) => {
			const { res, body } = await post(form([['/a.js', mapFile('a')]], version));
			expect(res.status).toBe(400);
			expect(body.error).toEqual({
				code: 'invalid_request',
				message: expect.any(String),
				details: { field: 'release_version', reason: 'whitespace' },
			});
			expect(readReleaseState).not.toHaveBeenCalled();
			expect(putObject).not.toHaveBeenCalled();
		}
	);

	it('400 { field: minified_path } when there is no minified_path part', async () => {
		const { res, body } = await post(form([], RELEASE, [], [mapFile('a')]));
		expect(res.status).toBe(400);
		expect(body.error.details).toEqual({ field: 'minified_path' });
	});

	it('U7: 400 with both counts when 3 minified_path and 2 file parts are posted, nothing written', async () => {
		const { res, body } = await post(form([['/a.js', mapFile('a')], ['/b.js', mapFile('b')]], RELEASE, ['/c.js']));
		expect(res.status).toBe(400);
		expect(body.error).toMatchObject({
			code: 'invalid_request',
			details: { minified_path_count: 3, file_count: 2 },
		});
		expect(putObject).not.toHaveBeenCalled();
		expect(upsertReleaseSourcemaps).not.toHaveBeenCalled();
	});

	it('400 too_many_files { max, received } over SOURCEMAP_MAX_FILES_PER_REQUEST, read from the env', async () => {
		mockEnv.SOURCEMAP_MAX_FILES_PER_REQUEST = '2';
		const { res, body } = await post(form([['/a.js', mapFile('a')], ['/b.js', mapFile('b')], ['/c.js', mapFile('c')]]));
		expect(res.status).toBe(400);
		expect(body.error).toMatchObject({ code: 'too_many_files', details: { max: 2, received: 3 } });
		expect(putObject).not.toHaveBeenCalled();
	});

	it('defaults SOURCEMAP_MAX_FILES_PER_REQUEST to 50 when unset or garbage', async () => {
		mockEnv.SOURCEMAP_MAX_FILES_PER_REQUEST = 'lots';
		const pairs: Array<[string, File]> = [];
		for (let i = 0; i < 51; i++) pairs.push([`/${i}.js`, mapFile(String(i))]);
		const { res, body } = await post(form(pairs));
		expect(res.status).toBe(400);
		expect(body.error.details).toEqual({ max: 50, received: 51 });
	});

	it('400 { field: minified_path, index } for a path that is empty or over 512 chars AFTER normalisation', async () => {
		let out = await post(form([['/a.js', mapFile('a')], ['?x=1', mapFile('b')]]));
		expect(out.res.status).toBe(400);
		expect(out.body.error.details).toEqual({ field: 'minified_path', index: 1 });

		out = await post(form([['/' + 'p'.repeat(512), mapFile('a')]]));
		expect(out.res.status).toBe(400);
		expect(out.body.error.details).toEqual({ field: 'minified_path', index: 0 });

		// 512 exactly, reached only after the query is stripped, is fine.
		out = await post(form([['/' + 'p'.repeat(511) + '?v=' + 'q'.repeat(100), mapFile('a')]]));
		expect(out.res.status).toBe(201);
	});

	it('U15: 400 { field, duplicate, indexes } when two parts normalise to one path, nothing written', async () => {
		const { res, body } = await post(
			form([['/assets/x.js', mapFile('a')], ['https://app.example.com/assets/x.js?v=2', mapFile('b')]])
		);
		expect(res.status).toBe(400);
		expect(body.error).toEqual({
			code: 'invalid_request',
			message: expect.any(String),
			details: { field: 'minified_path', duplicate: '/assets/x.js', indexes: [0, 1] },
		});
		expect(readReleaseState).not.toHaveBeenCalled();
		expect(putObject).not.toHaveBeenCalled();
		expect(upsertReleaseSourcemaps).not.toHaveBeenCalled();
		expect(outcomesRecorded()).toEqual(['rejected']);
	});

	it('reports the FIRST duplicate pair by index when there are several', async () => {
		const { body } = await post(
			form([['/a.js', mapFile('a')], ['/b.js', mapFile('b')], ['/b.js#x', mapFile('c')], ['/a.js?y', mapFile('d')]])
		);
		expect(body.error.details).toEqual({ field: 'minified_path', duplicate: '/b.js', indexes: [1, 2] });
	});
});

describe('POST /api/sourcemaps -- per-file validation, in order, all-or-nothing (U8, U9)', () => {
	it.each([
		['not_json', rawFile('{"version": 3, "mappings": ')],
		['not_json', fileOf(new Uint8Array([0x89, 0x50, 0x4e, 0x47]), 'screen.png')],
		['not_v3', rawFile('[]')],
		['not_v3', rawFile('"a string"')],
		['not_v3', rawFile(JSON.stringify({ version: '3', mappings: 'AAAA', sources: [] }))],
		['not_v3', rawFile(JSON.stringify({ version: 2, mappings: 'AAAA', sources: [] }))],
		['no_mappings', rawFile(JSON.stringify({ version: 3, sources: [] }))],
		['no_mappings', rawFile(JSON.stringify({ version: 3, mappings: ['AAAA'], sources: [] }))],
		['no_sources', rawFile(JSON.stringify({ version: 3, mappings: 'AAAA' }))],
		['no_sources', rawFile(JSON.stringify({ version: 3, mappings: 'AAAA', sources: 'src/a.ts' }))],
	])('U8: 422 invalid_sourcemap { index: 1, reason: %s }, and the valid part 0 is NOT written', async (reason, bad) => {
		const { res, body } = await post(form([['/a.js', mapFile('a')], ['/bad.js', bad]]));
		expect(res.status).toBe(422);
		expect(body.error).toEqual({
			code: 'invalid_sourcemap',
			message: expect.any(String),
			details: { index: 1, reason },
		});
		expect(readReleaseState).not.toHaveBeenCalled();
		expect(putObject).not.toHaveBeenCalled();
		expect(upsertReleaseSourcemaps).not.toHaveBeenCalled();
		expect(outcomesRecorded()).toEqual(['rejected']);
	});

	it('accepts a map with an empty mappings string and sourcesContent present -- structural, not a decode', async () => {
		const { res } = await post(
			form([['/a.js', mapFile('a', { mappings: '', sourcesContent: ['const x = 1;'], sourceRoot: '' })]])
		);
		expect(res.status).toBe(201);
	});

	it('U9: 413 payload_too_large { index, max_bytes } for a part over SOURCEMAP_MAX_BYTES, nothing written', async () => {
		// Part 0 fits the cap exactly; part 1 carries one more key and does not.
		const big = mapFile('b', { names: ['over'] });
		mockEnv.SOURCEMAP_MAX_BYTES = String(mapBytes('a').byteLength);
		const { res, body } = await post(form([['/a.js', mapFile('a')], ['/b.js', big]]));
		expect(res.status).toBe(413);
		expect(body.error).toEqual({
			code: 'payload_too_large',
			message: expect.any(String),
			details: { index: 1, max_bytes: mapBytes('a').byteLength },
		});
		expect(putObject).not.toHaveBeenCalled();
		expect(outcomesRecorded()).toEqual(['rejected']);
	});

	it('checks each file in order: an invalid part 0 is reported before an oversized part 1', async () => {
		// 'nope' is 4 bytes and under the cap; the valid map at part 1 is over it.
		mockEnv.SOURCEMAP_MAX_BYTES = '10';
		const { res, body } = await post(form([['/a.js', rawFile('nope')], ['/b.js', mapFile('b')]]));
		expect(res.status).toBe(422);
		expect(body.error.details).toEqual({ index: 0, reason: 'not_json' });
	});

	it('defaults SOURCEMAP_MAX_BYTES to 50 MiB', async () => {
		mockEnv.SOURCEMAP_MAX_BYTES = '-5';
		const { res } = await post(form([['/a.js', mapFile('a')]]));
		expect(res.status).toBe(201);
	});

	it('U10: preserves a 413 thrown while parsing the body (adapter-node BODY_SIZE_LIMIT) in §7\'s envelope', async () => {
		const req = new Request('http://localhost/api/sourcemaps', { method: 'POST' });
		(req as any).formData = async () => {
			throw { status: 413, body: { message: 'Content-length exceeds limit' } };
		};
		const res = await POST({ request: req } as any);
		expect(res.status).toBe(413);
		expect(await res.json()).toEqual({
			error: { code: 'payload_too_large', message: 'Content-length exceeds limit', details: {} },
		});
		expect(outcomesRecorded()).toEqual(['rejected']);
	});
});

describe('POST /api/sourcemaps -- the three phases (U12, U18)', () => {
	it('U18: 422 release_full { max: 2000, current: 1999 } from the pre-check -- before the first putObject', async () => {
		readReleaseState.mockResolvedValue({ current: 1999, existing: new Map() });
		const { res, body } = await post(form([['/a.js', mapFile('a')], ['/b.js', mapFile('b')]]));
		expect(res.status).toBe(422);
		expect(body.error).toEqual({
			code: 'release_full',
			message: expect.any(String),
			details: { max: 2000, current: 1999 },
		});
		expect(putObject).not.toHaveBeenCalled();
		expect(upsertReleaseSourcemaps).not.toHaveBeenCalled();
		expect(outcomesRecorded()).toEqual(['rejected']);
	});

	it('the pre-check counts only paths with no row yet: 1999 rows + 1 replace + 1 new = 2000 is accepted', async () => {
		readReleaseState.mockResolvedValue({
			current: 1999,
			existing: new Map([['/a.js', { sha256: 'stale', storageKey: 'old' }]]),
		});
		upsertReleaseSourcemaps.mockResolvedValue({ releaseFileCount: 2000, displacedKeys: [] });
		const { res, body } = await post(form([['/a.js', mapFile('a')], ['/b.js', mapFile('b')]]));
		expect(res.status).toBe(201);
		expect(body.release_file_count).toBe(2000);
	});

	it('reads SOURCEMAP_MAX_FILES_PER_RELEASE from the env and hands it to the transaction', async () => {
		mockEnv.SOURCEMAP_MAX_FILES_PER_RELEASE = '3';
		readReleaseState.mockResolvedValue({ current: 3, existing: new Map() });
		const full = await post(form([['/a.js', mapFile('a')]]));
		expect(full.res.status).toBe(422);
		expect(full.body.error.details).toEqual({ max: 3, current: 3 });

		readReleaseState.mockResolvedValue({ current: 2, existing: new Map() });
		const ok = await post(form([['/a.js', mapFile('a')]]));
		expect(ok.res.status).toBe(201);
		expect(upsertReleaseSourcemaps).toHaveBeenCalledWith(expect.objectContaining({ maxPerRelease: 3 }));
	});

	it('U12: 503 storage_unavailable when storage is unconfigured -- no read, no put, no row', async () => {
		isStorageConfigured.mockReturnValue(false);
		const { res, body } = await post(form([['/a.js', mapFile('a')]]));
		expect(res.status).toBe(503);
		expect(body.error).toEqual({ code: 'storage_unavailable', message: expect.any(String), details: {} });
		expect(readReleaseState).not.toHaveBeenCalled();
		expect(putObject).not.toHaveBeenCalled();
		expect(upsertReleaseSourcemaps).not.toHaveBeenCalled();
		expect(outcomesRecorded()).toEqual(['storage_error']);
	});

	it('U12: putObject failing on part k (k=3 of 4) is 503 with NO row and k-1 = 2 orphans already put', async () => {
		putObject
			.mockResolvedValueOnce(undefined)
			.mockResolvedValueOnce(undefined)
			.mockRejectedValueOnce(new Error('minio: connection refused'));
		const { res, body } = await post(
			form([['/a.js', mapFile('a')], ['/b.js', mapFile('b')], ['/c.js', mapFile('c')], ['/d.js', mapFile('d')]])
		);
		expect(res.status).toBe(503);
		expect(body.error).toEqual({ code: 'storage_unavailable', message: expect.any(String), details: {} });
		// Exactly k = 3 attempts: the two orphans, the failure, and NOT the fourth part.
		expect(putObject).toHaveBeenCalledTimes(3);
		expect(upsertReleaseSourcemaps).not.toHaveBeenCalled();
		expect(bestEffortDeleteObjects).not.toHaveBeenCalled();
		expect(outcomesRecorded()).toEqual(['storage_error']);
	});

	it('a lost race at the cap inside the transaction is 422 release_full with the re-read count, counted as storage_error', async () => {
		upsertReleaseSourcemaps.mockRejectedValue(new ReleaseFullError(2000, 2000));
		const { res, body } = await post(form([['/a.js', mapFile('a')]]));
		expect(res.status).toBe(422);
		expect(body.error).toMatchObject({ code: 'release_full', details: { max: 2000, current: 2000 } });
		expect(putObject).toHaveBeenCalledTimes(1);
		expect(bestEffortDeleteObjects).not.toHaveBeenCalled();
		expect(outcomesRecorded()).toEqual(['storage_error']);
	});

	it('an unexpected transaction failure after the objects were put is 500 internal in §7\'s envelope', async () => {
		upsertReleaseSourcemaps.mockRejectedValue(new Error('connection reset'));
		const { res, body } = await post(form([['/a.js', mapFile('a')]]));
		expect(res.status).toBe(500);
		expect(body.error).toEqual({ code: 'internal', message: expect.any(String), details: {} });
		expect(bestEffortDeleteObjects).not.toHaveBeenCalled();
		expect(outcomesRecorded()).toEqual(['storage_error']);
	});

	it('a failed phase-1 read is 500 internal with nothing put', async () => {
		readReleaseState.mockRejectedValue(new Error('db down'));
		const { res, body } = await post(form([['/a.js', mapFile('a')]]));
		expect(res.status).toBe(500);
		expect(body.error.code).toBe('internal');
		expect(putObject).not.toHaveBeenCalled();
	});
});
