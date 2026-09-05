import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * sentinel-user-report-issue §4.1 — `POST /api/report`, at the route boundary.
 *
 * What is asserted here is what the ROUTE itself decides: the ORDER of §7's checks (a 401 before
 * any body is read, a body rejection before any project is resolved), the §7 envelope on every
 * non-2xx, §4.1-b's success shape, the replay-vs-`409` idempotency decision, the lost-race replay,
 * and that `sentinel_reports_received_total{outcome}` counts what the caller actually received.
 *
 * The database-shaped claims — that one `issues` row and one `external_issue_reports` row appear,
 * that `manual_issue_reports` stays empty, that two organizations cannot see each other's rows —
 * are asserted against a real migrated Postgres in `report.intake.integration.test.ts`, because a
 * mocked `db` cannot tell "wrote the right rows" from "wrote nothing".
 */

const authenticateReportKey = vi.fn();
const resolveReportProject = vi.fn();
const upsertExternalReporter = vi.fn();
const createExternalIssueReport = vi.fn();
const findReportIdempotency = vi.fn();
const deleteExpiredReportIdempotency = vi.fn();
const runTriage = vi.fn();

vi.mock('$lib/server/report-auth', async () => {
	const actual = await vi.importActual<typeof import('$lib/server/report-auth')>('$lib/server/report-auth');
	return { ...actual, authenticateReportKey, resolveReportProject };
});
vi.mock('$lib/db/queries/external-reporters', () => ({ upsertExternalReporter }));
vi.mock('$lib/server/triage', () => ({ runTriage }));
vi.mock('$lib/db/queries/reports', async () => {
	const actual = await vi.importActual<typeof import('$lib/db/queries/reports')>('$lib/db/queries/reports');
	return { ...actual, createExternalIssueReport };
});
vi.mock('$lib/server/report-idempotency', async () => {
	const actual =
		await vi.importActual<typeof import('$lib/server/report-idempotency')>('$lib/server/report-idempotency');
	return { ...actual, findReportIdempotency, deleteExpiredReportIdempotency };
});

const { POST } = await import('./+server');
const { reportErrorResponse } = await import('$lib/server/report-auth');
const { ReportAttachmentNotFoundError } = await import('$lib/db/queries/reports');
const { deriveReportTitle } = await import('$lib/server/report-title');
const { REPORT_OUTCOME, renderMetrics, resetMetricsForTest } = await import('$lib/server/metrics');

const CREDENTIAL = {
	keyId: 'key-1',
	organizationId: 'org-1',
	fixedProjectId: 'proj-1',
	scope: 'report',
	rateLimitRpm: 5000,
	orgWide: false,
};
const PROJECT = { projectId: 'proj-1', projectName: 'checkout' };
const ISSUE_ID = '7c9e6679-7425-40de-944b-e07fc1f90ae7';
const ATTACHMENT_ID = '3b1f0d2c-9a4e-4b77-8f21-6d5c4b3a2019';

function body(overrides: Record<string, unknown> = {}) {
	return {
		text: 'The save button on a visit detail spins forever and the visit never saves.',
		reporter: { external_id: 'u_8f3c41', display_name: 'Sarah K.' },
		...overrides,
	};
}

function request(payload: unknown, headers: Record<string, string> = {}): Request {
	return new Request('http://localhost/api/report', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json', ...headers },
		body: typeof payload === 'string' ? payload : JSON.stringify(payload),
	});
}

async function post(payload: unknown = body(), headers: Record<string, string> = {}) {
	const res = await POST({ request: request(payload, headers) } as never);
	return { status: res.status, headers: res.headers, json: await res.clone().json() };
}

/** Reads one series out of the live registry, so the metric is asserted, not the call. */
async function counter(outcome: string): Promise<number> {
	const { body: text } = await renderMetrics();
	const line = text
		.split('\n')
		.find((l) => l.startsWith(`sentinel_reports_received_total{outcome="${outcome}"}`));
	return line ? Number(line.split(' ').pop()) : NaN;
}

beforeEach(() => {
	vi.clearAllMocks();
	resetMetricsForTest();
	authenticateReportKey.mockResolvedValue(CREDENTIAL);
	resolveReportProject.mockResolvedValue(PROJECT);
	upsertExternalReporter.mockResolvedValue({ id: 'reporter-1' });
	createExternalIssueReport.mockResolvedValue({ issue: { id: ISSUE_ID }, report: {} });
	findReportIdempotency.mockResolvedValue(null);
	deleteExpiredReportIdempotency.mockResolvedValue(true);
	runTriage.mockResolvedValue({
		ranAt: '2026-09-04T09:12:04.000Z',
		strategy: 'window_release',
		candidates: [],
	});
});

describe('§4.1-b success', () => {
	it('returns 201 with report_id, issue_id, status=created, deduplicated=false and the inline triage object', async () => {
		const res = await post();
		expect(res.status).toBe(201);
		expect(res.json).toEqual({
			report_id: ISSUE_ID,
			issue_id: ISSUE_ID,
			status: 'created',
			deduplicated: false,
			// §4.1-b: "the §4.2 `triage` object, inline". OQ-1 variant (b) makes triage inline-capable
			// and §4.3 runs it inside this request, after creation has committed.
			triage: { ranAt: '2026-09-04T09:12:04.000Z', strategy: 'window_release', candidates: [] },
		});
		expect(res.headers.get('Content-Type')).toBe('application/json');
	});

	// §12 / be.md criterion 16, and the one that decides whether this endpoint degrades or loses
	// data: triage runs AFTER creation has committed, so a matcher that throws costs a `triage`
	// object and nothing else. If this ever goes red as a 500, a reporter's report is being thrown
	// away because a heuristic failed.
	it('a FORCED triage failure yields triage: null and a CREATED report — never a 500, never a lost report', async () => {
		runTriage.mockRejectedValueOnce(new Error('deliberate triage failure'));

		const res = await post();

		expect(res.status).toBe(201);
		expect(res.json).toEqual({
			report_id: ISSUE_ID,
			issue_id: ISSUE_ID,
			status: 'created',
			deduplicated: false,
			triage: null,
		});
		// The report was created, and creation was not rolled back or retried by the failure.
		expect(createExternalIssueReport).toHaveBeenCalledTimes(1);
	});

	it('triage runs AFTER creation, so it can never abort it', async () => {
		const order: string[] = [];
		createExternalIssueReport.mockImplementationOnce(async () => {
			order.push('create');
			return { issue: { id: ISSUE_ID }, report: {} };
		});
		runTriage.mockImplementationOnce(async () => {
			order.push('triage');
			return { ranAt: '2026-09-04T09:12:04.000Z', strategy: 'window_release', candidates: [] };
		});

		await post();

		expect(order).toEqual(['create', 'triage']);
	});

	it('creates with source_channel/issue_type via createExternalIssueReport, never createManualIssue', async () => {
		await post(body({ severity: 'high', context: { page: 'Visits' } }));
		expect(createExternalIssueReport).toHaveBeenCalledTimes(1);
		expect(createExternalIssueReport.mock.calls[0][0]).toMatchObject({
			organizationId: 'org-1',
			projectId: 'proj-1',
			externalReporterId: 'reporter-1',
			severity: 'high',
			context: { page: 'Visits' },
			attachmentIds: [],
		});
	});

	it('takes the org and project from the CREDENTIAL, never from the body (B7)', async () => {
		await post(body({ project_key: 'someone-elses', organizationId: 'org-99' }));
		expect(upsertExternalReporter).toHaveBeenCalledWith(
			expect.objectContaining({ organizationId: 'org-1', projectId: 'proj-1' })
		);
		expect(createExternalIssueReport.mock.calls[0][0].organizationId).toBe('org-1');
	});

	it('derives the title by §4.5a when none is supplied, and stores it as issues.message', async () => {
		const text = Array(40).fill('word').join(' ');
		await post(body({ text, context: { page: 'Visits' } }));
		expect(createExternalIssueReport.mock.calls[0][0].title).toBe(
			deriveReportTitle({ text, page: 'Visits' })
		);
	});

	it('prefers a caller-supplied title over the derived one', async () => {
		await post(body({ title: '  Save is broken  ', context: { page: 'Visits' } }));
		expect(createExternalIssueReport.mock.calls[0][0].title).toBe('Save is broken');
	});

	it('stores the report text as body_md, unmodified apart from trimming', async () => {
		await post(body({ text: '  Steps:\n\n1. click save  ' }));
		expect(createExternalIssueReport.mock.calls[0][0].bodyMd).toBe('Steps:\n\n1. click save');
	});
});

describe('§7 check ORDER', () => {
	it('401 before the body is read at all — an unknown key is not a body-validation oracle', async () => {
		authenticateReportKey.mockRejectedValue(
			reportErrorResponse(401, 'unauthenticated', 'Unknown or inactive API key.')
		);
		// A body that would fail every §4.1 bound. The 401 must win.
		const res = await post({ nonsense: true });
		expect(res.status).toBe(401);
		expect(res.json.error.code).toBe('unauthenticated');
		expect(resolveReportProject).not.toHaveBeenCalled();
		expect(createExternalIssueReport).not.toHaveBeenCalled();
	});

	it('403 forbidden_scope carries the key scope in details, and reads no body', async () => {
		authenticateReportKey.mockRejectedValue(
			reportErrorResponse(403, 'forbidden_scope', "This API key's scope may not submit user reports.", {
				scope: 'ingest',
			})
		);
		const res = await post();
		expect(res.status).toBe(403);
		expect(res.json.error).toMatchObject({ code: 'forbidden_scope', details: { scope: 'ingest' } });
	});

	it('429 rate_limited preserves Retry-After from the credential check', async () => {
		authenticateReportKey.mockRejectedValue(
			reportErrorResponse(429, 'rate_limited', 'Rate limit exceeded.', {}, { 'Retry-After': '42' })
		);
		const res = await post();
		expect(res.status).toBe(429);
		expect(res.headers.get('Retry-After')).toBe('42');
		expect(res.json.error).toMatchObject({ code: 'rate_limited', details: {} });
	});

	it('400 invalid_request before the project is resolved', async () => {
		const res = await post(body({ text: '   ' }));
		expect(res.status).toBe(400);
		expect(res.json.error).toMatchObject({ code: 'invalid_request', details: { field: 'text' } });
		expect(resolveReportProject).not.toHaveBeenCalled();
	});

	it('413 payload_too_large above the 1 MiB cap, and nothing is created', async () => {
		const oversized = JSON.stringify(body({ text: 'x'.repeat(1 << 20) }));
		const res = await post(oversized);
		expect(res.status).toBe(413);
		expect(res.json.error.code).toBe('payload_too_large');
		expect(createExternalIssueReport).not.toHaveBeenCalled();
	});

	it('403 project_not_found when the named project is not in the key\'s organization', async () => {
		resolveReportProject.mockRejectedValue(
			reportErrorResponse(403, 'project_not_found', 'Project not found in this organization.')
		);
		const res = await post(body({ project_key: 'someone-elses' }));
		expect(res.status).toBe(403);
		expect(res.json.error).toMatchObject({ code: 'project_not_found', details: {} });
		expect(createExternalIssueReport).not.toHaveBeenCalled();
	});
});

describe('§4.1 project resolution inputs', () => {
	it('passes X-Project-Key through when present', async () => {
		await post(body(), { 'X-Project-Key': 'checkout' });
		expect(resolveReportProject).toHaveBeenCalledWith(CREDENTIAL, 'checkout');
	});

	it('falls back to the body\'s project_key when the header is absent', async () => {
		await post(body({ project_key: 'checkout' }));
		expect(resolveReportProject).toHaveBeenCalledWith(CREDENTIAL, 'checkout');
	});

	it('prefers the header over the body when both are present', async () => {
		await post(body({ project_key: 'from-body' }), { 'X-Project-Key': 'from-header' });
		expect(resolveReportProject).toHaveBeenCalledWith(CREDENTIAL, 'from-header');
	});

	it('passes null when neither is present, leaving the decision to the resolver', async () => {
		await post(body());
		expect(resolveReportProject).toHaveBeenCalledWith(CREDENTIAL, null);
	});
});

describe('§4.1 idempotency', () => {
	it('does not touch the store at all when no idempotency_key is sent', async () => {
		await post(body());
		expect(findReportIdempotency).not.toHaveBeenCalled();
		expect(createExternalIssueReport.mock.calls[0][0].idempotency).toBeNull();
	});

	it('replays the first report_id with deduplicated=true, and creates nothing', async () => {
		const { hashReportRequest } = await import('$lib/server/report-idempotency');
		const payload = body({ idempotency_key: 'bugrep-01J8Z9K3QX7M2' });
		findReportIdempotency.mockResolvedValue({
			id: 'rec-1',
			requestHash: hashReportRequest(payload, 'proj-1'),
			issueId: ISSUE_ID,
			createdAt: new Date(),
		});

		const res = await post(payload);
		expect(res.status).toBe(201);
		expect(res.json).toMatchObject({ report_id: ISSUE_ID, deduplicated: true, status: 'created' });
		expect(createExternalIssueReport).not.toHaveBeenCalled();
		expect(upsertExternalReporter).not.toHaveBeenCalled();
	});

	it('409 idempotency_conflict when the same key arrives with a different body', async () => {
		findReportIdempotency.mockResolvedValue({
			id: 'rec-1',
			requestHash: 'a-digest-of-some-other-body',
			issueId: ISSUE_ID,
			createdAt: new Date(),
		});
		const res = await post(body({ idempotency_key: 'k1' }));
		expect(res.status).toBe(409);
		expect(res.json.error).toMatchObject({ code: 'idempotency_conflict', details: {} });
		expect(createExternalIssueReport).not.toHaveBeenCalled();
	});

	it('a record older than the 7-day window is deleted and the repeat creates a SECOND report', async () => {
		const { hashReportRequest } = await import('$lib/server/report-idempotency');
		const payload = body({ idempotency_key: 'k1' });
		const eightDaysAgo = new Date(Date.now() - 8 * 24 * 60 * 60 * 1000);
		findReportIdempotency.mockResolvedValue({
			id: 'rec-1',
			requestHash: hashReportRequest(payload, 'proj-1'),
			issueId: 'an-older-issue',
			createdAt: eightDaysAgo,
		});

		const res = await post(payload);
		expect(deleteExpiredReportIdempotency).toHaveBeenCalledWith('key-1', 'k1');
		expect(res.status).toBe(201);
		expect(res.json).toMatchObject({ report_id: ISSUE_ID, deduplicated: false });
	});

	it('hands the digest to the creation call so the record is written in the same transaction', async () => {
		const { hashReportRequest } = await import('$lib/server/report-idempotency');
		const payload = body({ idempotency_key: 'k1' });
		await post(payload);
		expect(createExternalIssueReport.mock.calls[0][0].idempotency).toEqual({
			apiKeyId: 'key-1',
			idempotencyKey: 'k1',
			requestHash: hashReportRequest(payload, 'proj-1'),
		});
	});

	it('replays the winner when a concurrent request took the key first (unique violation)', async () => {
		const { hashReportRequest } = await import('$lib/server/report-idempotency');
		const payload = body({ idempotency_key: 'k1' });
		createExternalIssueReport.mockRejectedValue(
			Object.assign(new Error('duplicate key'), {
				code: '23505',
				constraint_name: 'report_idempotency_keys_key_unique',
			})
		);
		findReportIdempotency
			.mockResolvedValueOnce(null)
			.mockResolvedValueOnce({
				id: 'rec-1',
				requestHash: hashReportRequest(payload, 'proj-1'),
				issueId: 'winner-issue-id',
				createdAt: new Date(),
			});

		const res = await post(payload);
		expect(res.status).toBe(201);
		expect(res.json).toMatchObject({ report_id: 'winner-issue-id', deduplicated: true });
	});

	it('500 internal — never a bogus replay — when the creation fails for any other reason', async () => {
		createExternalIssueReport.mockRejectedValue(new Error('connection reset'));
		const res = await post();
		expect(res.status).toBe(500);
		expect(res.json.error).toMatchObject({ code: 'internal', details: {} });
	});
});

describe('§4.1 attachments', () => {
	it('400 attachment_not_found with details.field=attachments, and nothing is created', async () => {
		createExternalIssueReport.mockRejectedValue(new ReportAttachmentNotFoundError());
		const res = await post(body({ attachments: [ATTACHMENT_ID] }));
		expect(res.status).toBe(400);
		expect(res.json.error).toMatchObject({
			code: 'attachment_not_found',
			details: { field: 'attachments' },
		});
	});

	it('passes the ids through to the creation transaction so the claim is atomic with it', async () => {
		await post(body({ attachments: [ATTACHMENT_ID] }));
		expect(createExternalIssueReport.mock.calls[0][0].attachmentIds).toEqual([ATTACHMENT_ID]);
	});
});

describe('§7 envelope', () => {
	it('every non-2xx is {error:{code,message,details}} and never SvelteKit\'s {message}', async () => {
		const cases: Array<() => Promise<{ json: any }>> = [
			() => {
				authenticateReportKey.mockRejectedValueOnce(
					reportErrorResponse(401, 'unauthenticated', 'Missing X-API-Key header.')
				);
				return post();
			},
			() => post(body({ text: '' })),
			() => {
				resolveReportProject.mockRejectedValueOnce(
					reportErrorResponse(403, 'project_not_found', 'Project not found in this organization.')
				);
				return post();
			},
			() => {
				createExternalIssueReport.mockRejectedValueOnce(new Error('boom'));
				return post();
			},
		];

		for (const run of cases) {
			const { json } = await run();
			expect(Object.keys(json)).toEqual(['error']);
			expect(json.error).toHaveProperty('code');
			expect(json.error).toHaveProperty('message');
			expect(json.error).toHaveProperty('details');
			expect(json).not.toHaveProperty('message');
		}
	});
});

describe('§12 sentinel_reports_received_total', () => {
	it('counts a created report', async () => {
		await post();
		expect(await counter(REPORT_OUTCOME.Created)).toBe(1);
	});

	it('counts a deduplicated replay separately from a created one', async () => {
		const { hashReportRequest } = await import('$lib/server/report-idempotency');
		const payload = body({ idempotency_key: 'k1' });
		findReportIdempotency.mockResolvedValue({
			id: 'rec-1',
			requestHash: hashReportRequest(payload, 'proj-1'),
			issueId: ISSUE_ID,
			createdAt: new Date(),
		});
		await post(payload);
		expect(await counter(REPORT_OUTCOME.Deduplicated)).toBe(1);
		expect(await counter(REPORT_OUTCOME.Created)).toBe(0);
	});

	it.each([
		[401, 'unauthenticated' as const, REPORT_OUTCOME.Unauthorized],
		[403, 'forbidden_scope' as const, REPORT_OUTCOME.Forbidden],
		[429, 'rate_limited' as const, REPORT_OUTCOME.RateLimited],
	])('maps a %s to outcome=%s', async (status, code, outcome) => {
		authenticateReportKey.mockRejectedValue(reportErrorResponse(status, code, 'no'));
		await post();
		expect(await counter(outcome)).toBe(1);
	});

	it('counts a validation failure as `rejected`, not as `error` — a bad body must not page anyone', async () => {
		await post(body({ text: '' }));
		expect(await counter(REPORT_OUTCOME.Rejected)).toBe(1);
		expect(await counter(REPORT_OUTCOME.Error)).toBe(0);
	});

	it('counts a 500 as `error` — the one value §12 alerts on', async () => {
		createExternalIssueReport.mockRejectedValue(new Error('boom'));
		await post();
		expect(await counter(REPORT_OUTCOME.Error)).toBe(1);
	});

	it('counts a 409 as `conflict`', async () => {
		findReportIdempotency.mockResolvedValue({
			id: 'rec-1',
			requestHash: 'different',
			issueId: ISSUE_ID,
			createdAt: new Date(),
		});
		await post(body({ idempotency_key: 'k1' }));
		expect(await counter(REPORT_OUTCOME.Conflict)).toBe(1);
	});
});
