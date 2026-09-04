import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * sentinel-user-report-issue §4.5 -- `POST /api/report/attachments`.
 *
 * The route is thin on purpose (auth, bounds, the reporter upsert, then the shared upload core),
 * so these tests assert the things the route ITSELF decides: the §7 envelope on every non-2xx,
 * that the reporter row is upserted from the CREDENTIAL's org/project and never from the form,
 * that `uploader_type='external_reporter'` and `uploader_id` is the upserted reporter's id, and
 * that the byte budget and the per-minute throttle both return 429 with `Retry-After`.
 *
 * The 25 MB cap and the magic-byte rejection live in `upload-core.ts`/`attachment-sniff.ts` and
 * are tested there; what is asserted here is that this route reuses them and translates their
 * failures into §7's envelope instead of SvelteKit's `{"message": ...}`.
 */

const authenticateReportRequest = vi.fn();
const handleAttachmentUpload = vi.fn();
const checkDeclaredLength = vi.fn();
const checkReportUploadBudget = vi.fn();
const upsertExternalReporter = vi.fn();
const checkReportUploadRateLimit = vi.fn();

const mockEnv: Record<string, string | undefined> = {};

vi.mock('$env/dynamic/private', () => ({ env: mockEnv }));
vi.mock('$lib/server/report-auth', async () => {
	const actual = await vi.importActual<typeof import('$lib/server/report-auth')>('$lib/server/report-auth');
	return {
		...actual,
		authenticateReportRequest,
	};
});
vi.mock('$lib/server/upload-core', async () => {
	const actual = await vi.importActual<typeof import('$lib/server/upload-core')>('$lib/server/upload-core');
	return {
		...actual,
		handleAttachmentUpload,
		checkDeclaredLength,
		checkReportUploadBudget,
	};
});
vi.mock('$lib/db/queries/external-reporters', () => ({ upsertExternalReporter }));
vi.mock('$lib/rate-limit', async () => {
	const actual = await vi.importActual<typeof import('$lib/rate-limit')>('$lib/rate-limit');
	return { ...actual, checkReportUploadRateLimit };
});

const { POST } = await import('./+server');

const CTX = {
	keyId: 'key-1',
	organizationId: 'org-1',
	projectId: 'proj-1',
	projectName: 'checkout',
	scope: 'report',
	rateLimitRpm: 5000,
	orgWide: false,
};

function png(bytes = 32): File {
	const buf = new Uint8Array(bytes);
	buf.set([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
	return new File([buf], 'screen.png', { type: 'image/png' });
}

function makeRequest(form: FormData): Request {
	const req = new Request('http://localhost/api/report/attachments', { method: 'POST' });
	// `request.formData()` is stubbed rather than serialised: the route only ever reads parts, and
	// building a real multipart body here would test undici, not this route.
	(req as any).formData = async () => form;
	return req;
}

function form(fields: Record<string, string | File | null> = {}): FormData {
	const fd = new FormData();
	const merged: Record<string, string | File | null> = {
		file: png(),
		external_id: 'u_8f3c41',
		...fields,
	};
	for (const [k, v] of Object.entries(merged)) {
		if (v === null) continue;
		fd.append(k, v as any);
	}
	return fd;
}

async function post(fd: FormData = form()) {
	const res = await POST({ request: makeRequest(fd) } as any);
	return { res, body: await res.clone().json() };
}

beforeEach(() => {
	vi.clearAllMocks();
	for (const k of Object.keys(mockEnv)) delete mockEnv[k];
	authenticateReportRequest.mockResolvedValue(CTX);
	checkDeclaredLength.mockReturnValue(undefined);
	checkReportUploadRateLimit.mockReturnValue({ allowed: true });
	checkReportUploadBudget.mockResolvedValue({
		allowed: true,
		usedBytes: 0,
		budgetBytes: 250 * 1024 * 1024,
		retryAfter: 86400,
	});
	upsertExternalReporter.mockResolvedValue({ id: 'reporter-1' });
	handleAttachmentUpload.mockResolvedValue({
		id: 'att-1',
		url: '/api/attachments/att-1',
		filename: 'screen.png',
		contentType: 'image/png',
		sizeBytes: 184213,
	});
});

describe('POST /api/report/attachments', () => {
	it('returns §4.5\'s 201 body in snake_case', async () => {
		const { res, body } = await post();
		expect(res.status).toBe(201);
		expect(body).toEqual({
			attachment_id: 'att-1',
			filename: 'screen.png',
			content_type: 'image/png',
			size_bytes: 184213,
		});
	});

	it("writes uploader_type='external_reporter' and uploader_id = the upserted reporter's id", async () => {
		await post();
		expect(handleAttachmentUpload).toHaveBeenCalledWith(
			expect.objectContaining({
				organizationId: 'org-1',
				uploaderType: 'external_reporter',
				uploaderId: 'reporter-1',
			})
		);
	});

	it('upserts the reporter from the CREDENTIAL, ignoring an organizationId form part (B7)', async () => {
		await post(form({ organizationId: 'org-someone-else', display_name: 'Ada', email: 'ada@example.test' }));
		expect(upsertExternalReporter).toHaveBeenCalledWith({
			organizationId: 'org-1',
			projectId: 'proj-1',
			externalId: 'u_8f3c41',
			displayName: 'Ada',
			email: 'ada@example.test',
		});
	});

	it('propagates the authenticator\'s §7 response verbatim (401/403)', async () => {
		const forbidden = new Response(
			JSON.stringify({ error: { code: 'forbidden_scope', message: 'no', details: { scope: 'ingest' } } }),
			{ status: 403, headers: { 'Content-Type': 'application/json' } }
		);
		authenticateReportRequest.mockRejectedValue(forbidden);
		const { res, body } = await post();
		expect(res.status).toBe(403);
		expect(body.error).toMatchObject({ code: 'forbidden_scope', details: { scope: 'ingest' } });
	});

	it('400 invalid_request when external_id is missing, with the field named', async () => {
		const { res, body } = await post(form({ external_id: null }));
		expect(res.status).toBe(400);
		expect(body.error).toMatchObject({ code: 'invalid_request', details: { field: 'external_id' } });
		expect(upsertExternalReporter).not.toHaveBeenCalled();
		expect(handleAttachmentUpload).not.toHaveBeenCalled();
	});

	it('400 invalid_request when external_id is over 255 characters', async () => {
		const { res, body } = await post(form({ external_id: 'u'.repeat(256) }));
		expect(res.status).toBe(400);
		expect(body.error.details).toEqual({ field: 'external_id' });
	});

	it('400 invalid_request when there is no file part', async () => {
		const { res, body } = await post(form({ file: null }));
		expect(res.status).toBe(400);
		expect(body.error.details).toEqual({ field: 'file' });
	});

	it('turns the shared core\'s 413 into §7\'s payload_too_large envelope', async () => {
		checkDeclaredLength.mockImplementation(() => {
			throw { status: 413, body: { message: 'File exceeds the 26214400 byte cap' } };
		});
		const { res, body } = await post();
		expect(res.status).toBe(413);
		expect(body.error.code).toBe('payload_too_large');
		expect(body.error.message).toMatch(/byte cap/);
	});

	it('preserves a 413 thrown while parsing the body (adapter-node BODY_SIZE_LIMIT), not 400', async () => {
		const req = new Request('http://localhost/api/report/attachments', { method: 'POST' });
		(req as any).formData = async () => {
			throw { status: 413, body: { message: 'Content-length exceeds limit' } };
		};
		const res = await POST({ request: req } as any);
		expect(res.status).toBe(413);
		expect((await res.json()).error.code).toBe('payload_too_large');
	});

	it('reports a genuinely unparseable body as 400 invalid_request', async () => {
		const req = new Request('http://localhost/api/report/attachments', { method: 'POST' });
		(req as any).formData = async () => {
			throw new TypeError('Could not parse content as FormData');
		};
		const res = await POST({ request: req } as any);
		expect(res.status).toBe(400);
		expect((await res.json()).error.details).toEqual({ field: 'body' });
	});

	it('turns the magic-byte rejection into §7\'s unsupported_media_type envelope', async () => {
		handleAttachmentUpload.mockRejectedValue({
			status: 415,
			body: { message: 'File content is not an allowed type (or does not match its declared type)' },
		});
		const { res, body } = await post();
		expect(res.status).toBe(415);
		expect(body.error.code).toBe('unsupported_media_type');
	});

	it('429 rate_limited with Retry-After when REPORT_UPLOAD_RPM is spent, before the body is read', async () => {
		checkReportUploadRateLimit.mockReturnValue({ allowed: false, retryAfter: 42 });
		const { res, body } = await post();
		expect(res.status).toBe(429);
		expect(res.headers.get('Retry-After')).toBe('42');
		expect(body.error.code).toBe('rate_limited');
		expect(handleAttachmentUpload).not.toHaveBeenCalled();
	});

	it('reads REPORT_UPLOAD_RPM from the environment, defaulting to 10', async () => {
		await post();
		expect(checkReportUploadRateLimit).toHaveBeenCalledWith('key-1', 10);

		vi.clearAllMocks();
		authenticateReportRequest.mockResolvedValue(CTX);
		checkReportUploadRateLimit.mockReturnValue({ allowed: true });
		checkReportUploadBudget.mockResolvedValue({ allowed: true, usedBytes: 0, budgetBytes: 1, retryAfter: 1 });
		upsertExternalReporter.mockResolvedValue({ id: 'reporter-1' });
		handleAttachmentUpload.mockResolvedValue({ id: 'a', url: '', filename: 'f', contentType: 'image/png', sizeBytes: 1 });
		mockEnv.REPORT_UPLOAD_RPM = '3';
		await post();
		expect(checkReportUploadRateLimit).toHaveBeenCalledWith('key-1', 3);
	});

	it('429 rate_limited with Retry-After when the daily byte budget is exhausted, and nothing is written', async () => {
		checkReportUploadBudget.mockResolvedValue({
			allowed: false,
			usedBytes: 1000,
			budgetBytes: 1000,
			retryAfter: 3600,
		});
		const { res, body } = await post();
		expect(res.status).toBe(429);
		expect(res.headers.get('Retry-After')).toBe('3600');
		expect(body.error).toEqual({ code: 'rate_limited', message: expect.any(String), details: {} });
		expect(handleAttachmentUpload).not.toHaveBeenCalled();
		expect(upsertExternalReporter).not.toHaveBeenCalled();
	});

	it('counts the incoming file against the credential\'s org, using the lowered budget from the env', async () => {
		mockEnv.REPORT_UPLOAD_BYTES_PER_DAY = '4096';
		const file = png(64);
		await post(form({ file }));
		expect(checkReportUploadBudget).toHaveBeenCalledWith('org-1', file.size, 4096);
	});
});
