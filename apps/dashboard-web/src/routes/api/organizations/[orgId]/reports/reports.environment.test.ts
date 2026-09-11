import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * sentinel-project-environments §4.8a — the environment assertion on
 * `POST /api/organizations/[orgId]/reports`, at the ROUTE boundary.
 *
 * What this file owns is what the route decides: which channel is read, which one wins, what counts
 * as "present", and which values are a `400`. What `createManualIssue` then WRITES to
 * `issues.environment` is a database claim and is asserted against a real migrated Postgres in
 * `src/lib/db/queries/reports.e2e-flow.integration.test.ts` — a mocked query layer cannot tell
 * "wrote `production`" from "wrote nothing".
 *
 * The value asserted below is the RAW assertion, not the resolved one: the route's job is to hand
 * `createManualIssue` what the caller asserted (or `null`), and the resolution to `production` is
 * the one helper in `reports.ts` that both writers share. Resolving here as well would be the
 * second rule §4.8a exists to prevent.
 */

const createManualIssue = vi.fn(async () => ({ issue: { id: 'issue-1' }, report: { issueId: 'issue-1' } }));
const listReports = vi.fn(async () => []);
const requireReportAccess = vi.fn(async () => ({ organizationId: 'org-1', role: 'engineer' }));
const getAccessibleProjectIds = vi.fn(async () => []);

vi.mock('$lib/db/queries/reports', async () => {
	const actual = await vi.importActual<typeof import('$lib/db/queries/reports')>('$lib/db/queries/reports');
	return { ...actual, createManualIssue, listReports };
});
vi.mock('$lib/server/report-access', () => ({ requireReportAccess }));
vi.mock('$lib/server/issue-access', () => ({ getAccessibleProjectIds }));

const { POST } = await import('./+server');

function makeEvent(body: Record<string, unknown>, headers: Record<string, string> = {}) {
	const lower = new Map(Object.entries(headers).map(([k, v]) => [k.toLowerCase(), v]));
	return {
		params: { orgId: 'org-1' },
		locals: { auth: async () => ({ user: { id: 'user-1' } }) },
		request: {
			json: async () => body,
			headers: { get: (name: string) => lower.get(name.toLowerCase()) ?? null },
		},
	} as any;
}

const validBody = { title: 'save spins forever', bodyMd: 'it spins', severity: 'high' };

async function statusOf(run: () => unknown): Promise<number> {
	try {
		await run();
		return 200;
	} catch (err) {
		return (err as { status: number }).status;
	}
}

beforeEach(() => {
	vi.clearAllMocks();
});

describe('POST /api/organizations/[orgId]/reports — the environment assertion (§4.8a)', () => {
	it('passes null when neither the body nor the header asserts anything', async () => {
		const res = await POST(makeEvent(validBody));

		expect(res.status).toBe(201);
		expect(createManualIssue).toHaveBeenCalledWith(expect.objectContaining({ environment: null }));
	});

	// "Present" means a non-empty string and NOTHING else. A chain that treated `null` as an
	// assertion would file the report under the literal string "null", or 23502 on it.
	it.each([
		['an omitted key', {}],
		['an explicit null', { environment: null }],
		['an explicit empty string', { environment: '' }],
	])('treats %s as "not asserted" and falls through to the default', async (_name, override) => {
		const res = await POST(makeEvent({ ...validBody, ...override }));

		expect(res.status).toBe(201);
		expect(createManualIssue).toHaveBeenCalledWith(expect.objectContaining({ environment: null }));
	});

	it('reads a valid body field verbatim, including an operator-shaped name', async () => {
		await POST(makeEvent({ ...validBody, environment: 'eu-west-1' }));
		expect(createManualIssue).toHaveBeenCalledWith(
			expect.objectContaining({ environment: 'eu-west-1' })
		);
	});

	it('reads the X-Sentinel-Environment header when the body says nothing, case-insensitively on the NAME', async () => {
		await POST(makeEvent(validBody, { 'X-Sentinel-Environment': 'staging-2' }));
		expect(createManualIssue).toHaveBeenCalledWith(
			expect.objectContaining({ environment: 'staging-2' })
		);
	});

	// Body over header, and the header is IGNORED rather than a 400: a proxy that adds a default
	// header must not break a client that was explicit.
	it('lets the body win over the header when both are present and they differ', async () => {
		const res = await POST(
			makeEvent({ ...validBody, environment: 'production' }, { 'X-Sentinel-Environment': 'staging' })
		);

		expect(res.status).toBe(201);
		expect(createManualIssue).toHaveBeenCalledWith(
			expect.objectContaining({ environment: 'production' })
		);
	});

	it('falls through to the header when the body key is present but empty', async () => {
		await POST(makeEvent({ ...validBody, environment: '' }, { 'X-Sentinel-Environment': 'staging' }));
		expect(createManualIssue).toHaveBeenCalledWith(expect.objectContaining({ environment: 'staging' }));
	});

	// Decision 10's widened set, and nothing more. The field is NEW on this route, so a rejection
	// here breaks no existing client — the difference from POST /api/report, which gains no new
	// error case at all.
	it.each([
		['Prod!', 'a bang is outside the accepted set'],
		['Production', 'the set is lower-case only'],
		['-prod', 'a leading hyphen fails ^[a-z0-9]'],
		['_unspecified', "the migration's transient DEFAULT can never be asserted (decision 51)"],
		['a'.repeat(51), 'the 50-character bound is folded into the pattern'],
	])('rejects a present but non-matching %s with a 400 (%s)', async (value) => {
		expect(await statusOf(() => POST(makeEvent({ ...validBody, environment: value })))).toBe(400);
		expect(createManualIssue).not.toHaveBeenCalled();
	});

	it('rejects a non-matching header with a 400 too — the header is not a way around the rule', async () => {
		expect(await statusOf(() => POST(makeEvent(validBody, { 'X-Sentinel-Environment': 'Prod!' })))).toBe(
			400
		);
		expect(createManualIssue).not.toHaveBeenCalled();
	});

	it('rejects a non-string environment rather than coercing it', async () => {
		expect(await statusOf(() => POST(makeEvent({ ...validBody, environment: 42 })))).toBe(400);
		expect(createManualIssue).not.toHaveBeenCalled();
	});

	// Decisions 8 and 9: the registry is not consulted on this path at all. `findOrCreateTriageProject`
	// provisions inbox projects on first use and a fresh project has ZERO declared environments, so a
	// rule that required a registry row would fail the first report into every new organisation.
	it('does not consult the registry — an unregistered name is stored, not rejected', async () => {
		const res = await POST(makeEvent({ ...validBody, environment: 'eu-west-1' }));

		expect(res.status).toBe(201);
		expect(createManualIssue).toHaveBeenCalledWith(
			expect.objectContaining({ environment: 'eu-west-1' })
		);
	});
});
