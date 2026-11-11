import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * sentinel-occurrence-spans TASK-01 — the issue loader's spans data: each listed occurrence's status, the issue-level link
 * target (latest occurrence WITH spans), and the "with spans only" list (`?spans=1`, a bounded server-side walk). What
 * "has spans" means is span-pull.integration.test.ts's boundary; the walk's cost bound is issue-spans.test.ts's. This file
 * owns WHEN they are asked, with WHAT scope, and that a failure degrades the badges and never the page.
 */

const authMock = vi.fn();
const requireProjectAccessMock = vi.fn();
const getOccurrencesByIssueIdMock = vi.fn();
const getOccurrencesByIdsMock = vi.fn();
const resolveViewerSpanScope = vi.fn();
const statusByOccurrence = vi.fn();
const walkOccurrencesWithSpans = vi.fn();
const isSpanPullEnabled = vi.fn();
const canReachSpanPullCore = vi.fn();

vi.mock('$env/dynamic/private', () => ({ env: {} }));
vi.mock('$lib/server/issue-access', () => ({ requireProjectAccess: requireProjectAccessMock }));
vi.mock('$lib/server/environment-access', () => ({
	resolveEnvironmentScope: vi.fn(async () => null),
	resolveEnvironmentScopes: vi.fn(async () => new Map()),
}));
vi.mock('$lib/db/queries/environments', () => ({ resolveEnvironmentDisplayName: vi.fn(async () => 'production') }));
vi.mock('$lib/server/queries/issue-queries', () => ({
	issueQueries: {
		getIssueById: vi.fn(async () => ISSUE),
		getProjectById: vi.fn(async () => ({ id: 'proj-1', name: 'P' })),
		getOccurrencesByIssueId: getOccurrencesByIssueIdMock,
		getOccurrencesByIds: getOccurrencesByIdsMock,
		countOccurrencesByIssueId: vi.fn(async () => 500),
		trendForIssue: vi.fn(async () => ({ hourly: [], daily: [] })),
		getOccurrenceDetail: vi.fn(async () => null),
		occurrenceExistsOutsideIssue: vi.fn(async () => false),
		getTagFacets: vi.fn(async () => []),
	},
}));
vi.mock('$lib/db/queries/issues', () => ({ getIssueRelations: vi.fn(async () => []) }));
vi.mock('$lib/db/queries/releases', () => ({ listPickerReleases: vi.fn(async () => []) }));
vi.mock('$lib/server/trace-links', () => ({ loadTraceLinks: vi.fn(async () => ({ orgSlug: 'acme', related: null })) }));
vi.mock('$lib/server/span-scope', () => ({ resolveViewerSpanScope }));
vi.mock('$lib/server/issue-spans', () => ({ SPANS_SCAN_BOUND: 2000, statusByOccurrence, walkOccurrencesWithSpans }));
vi.mock('$lib/server/span-pull', () => ({ isSpanPullEnabled, canReachSpanPullCore }));

const { load } = await import('./+page.server');

const ISSUE = { id: '11111111-1111-4111-8111-111111111111', projectId: 'proj-1', message: 'boom', environment: 'production', status: 'unresolved', issueType: 'system_error' };
const SCOPE = { orgId: 'org-1', projects: [{ id: 'proj-1' }], environmentScopes: new Map() };
const trace = (n: number) => n.toString(16).padStart(32, '0');
const row = (n: number) => ({ id: `occ-${n}`, issueId: ISSUE.id, level: 'error', traceId: trace(n), stacktrace: [], metadata: {}, sdk: null, createdAt: new Date(2026, 9, 6, 0, 0, 100 - n) });
const rows = (n: number) => Array.from({ length: n }, (_, i) => row(i + 1));
const run = (query = '') =>
	(load as any)({ params: { id: ISSUE.id }, locals: { auth: authMock }, url: new URL(`http://localhost/issues/${ISSUE.id}${query}`) });

beforeEach(() => {
	vi.clearAllMocks();
	authMock.mockResolvedValue({ user: { id: 'user-1' } });
	requireProjectAccessMock.mockResolvedValue({ projectId: 'proj-1', organizationId: 'org-1' });
	getOccurrencesByIssueIdMock.mockResolvedValue(rows(3));
	getOccurrencesByIdsMock.mockResolvedValue([]);
	resolveViewerSpanScope.mockResolvedValue(SCOPE);
	statusByOccurrence.mockResolvedValue({});
	walkOccurrencesWithSpans.mockResolvedValue({ ids: [], traceIds: new Map(), scanned: 0, exhausted: true, continueBefore: null });
	isSpanPullEnabled.mockReturnValue(true);
	canReachSpanPullCore.mockReturnValue(true);
});

describe('the spans data of the default list', () => {
	it('asks about the LISTED occurrences with the viewer’s scope, and exposes statuses, the organization and whether pull is offered', async () => {
		statusByOccurrence.mockResolvedValue({ 'occ-1': 'none', 'occ-2': 'available', 'occ-3': 'pending' });
		const data = await run();
		expect(resolveViewerSpanScope).toHaveBeenCalledWith('user-1', 'org-1');
		expect(statusByOccurrence).toHaveBeenCalledTimes(1);
		expect(statusByOccurrence.mock.calls[0][1]).toBe(SCOPE);
		expect(statusByOccurrence.mock.calls[0][0].map((o: any) => o.id)).toEqual(['occ-1', 'occ-2', 'occ-3']);
		expect(data.spansStatus).toEqual({ 'occ-1': 'none', 'occ-2': 'available', 'occ-3': 'pending' });
		expect(data.organizationId).toBe('org-1');
		expect(data.spanPullEnabled).toBe(true);
		expect(data.spansFilter.active).toBe(false);
		expect(isSpanPullEnabled).toHaveBeenCalledWith('org-1');
	});

	it('the issue-level link is the LATEST occurrence with spans, not merely the latest', async () => {
		statusByOccurrence.mockResolvedValue({ 'occ-1': 'none', 'occ-2': 'available', 'occ-3': 'available' });
		const data = await run();
		expect(data.latestSpansTrace).toEqual({ occurrenceId: 'occ-2', traceId: trace(2) });
		expect(walkOccurrencesWithSpans).not.toHaveBeenCalled();
	});

	it('with none in the listed page, the default page computes NOTHING more: no walk, no second status statement', async () => {
		getOccurrencesByIssueIdMock.mockResolvedValue(rows(100));
		statusByOccurrence.mockResolvedValue({});
		const data = await run();
		expect(walkOccurrencesWithSpans).not.toHaveBeenCalled();
		expect(statusByOccurrence).toHaveBeenCalledTimes(1);
		expect(data.latestSpansTrace).toBeNull();
		expect(data.spansFilter).toMatchObject({ active: false }); // the page offers "find an occurrence with spans" (?spans=1)
	});

	it('an organization that is not allow-listed gets no status query, no scope lookup and no badges', async () => {
		isSpanPullEnabled.mockReturnValue(false);
		const data = await run();
		expect(resolveViewerSpanScope).not.toHaveBeenCalled();
		expect(statusByOccurrence).not.toHaveBeenCalled();
		expect(walkOccurrencesWithSpans).not.toHaveBeenCalled();
		expect(data.spansStatus).toEqual({});
		expect(data.spansFilter).toBeNull();
		expect(data.spanPullEnabled).toBe(false);
	});

	it('a viewer who cannot reach the core project gets no badges and no pull button', async () => {
		canReachSpanPullCore.mockReturnValue(false);
		const data = await run('?spans=1');
		expect(statusByOccurrence).not.toHaveBeenCalled();
		expect(walkOccurrencesWithSpans).not.toHaveBeenCalled();
		expect(data.spansStatus).toEqual({});
		expect(data.spanPullEnabled).toBe(false);
	});

	it('with none anywhere, the link target is null (the page falls back to the latest traced occurrence, labelled)', async () => {
		const data = await run();
		expect(data.latestSpansTrace).toBeNull();
	});

	it('a failing status read degrades the badges, never the page', async () => {
		statusByOccurrence.mockRejectedValue(new Error('spans table timed out'));
		const data = await run();
		expect(data.spansStatus).toEqual({});
		expect(data.latestSpansTrace).toBeNull();
		expect(data.occurrences).toHaveLength(3);
	});

	it('asks nothing when the access check fails', async () => {
		requireProjectAccessMock.mockRejectedValue(Object.assign(new Error('no'), { status: 403 }));
		await expect(run()).rejects.toMatchObject({ status: 403 });
		expect(resolveViewerSpanScope).not.toHaveBeenCalled();
		expect(statusByOccurrence).not.toHaveBeenCalled();
	});
});

describe('the "with spans only" list (?spans=1)', () => {
	it('is built server-side from the bounded walk, not from the default newest-100 list', async () => {
		walkOccurrencesWithSpans.mockResolvedValue({
			ids: ['occ-7', 'occ-3'], traceIds: new Map([['occ-7', trace(7)], ['occ-3', trace(3)]]), scanned: 2000, exhausted: false, continueBefore: null,
		});
		getOccurrencesByIdsMock.mockResolvedValue([row(7), row(3)]);
		const data = await run('?spans=1');
		expect(walkOccurrencesWithSpans).toHaveBeenCalledWith({ issueId: ISSUE.id, scope: SCOPE, want: 100 });
		expect(getOccurrencesByIssueIdMock).not.toHaveBeenCalled();
		expect(getOccurrencesByIdsMock).toHaveBeenCalledWith(ISSUE.id, ['occ-7', 'occ-3']);
		expect(data.occurrences.map((o: any) => o.id)).toEqual(['occ-7', 'occ-3']);
		expect(data.spansStatus).toEqual({ 'occ-7': 'available', 'occ-3': 'available' });
		expect(data.spansFilter).toEqual({ active: true, scanned: 2000, bound: 2000, exhausted: false });
		expect(data.latestSpansTrace).toEqual({ occurrenceId: 'occ-7', traceId: trace(7) });
		expect(statusByOccurrence).not.toHaveBeenCalled(); // every row is `available` by construction
	});

	it('an issue with no occurrence with spans renders an empty filtered list, not an error', async () => {
		const data = await run('?spans=1');
		expect(data.occurrences).toEqual([]);
		expect(data.spansFilter.active).toBe(true);
		expect(data.latestSpansTrace).toBeNull();
	});

	it('any other value of the parameter is the plain list', async () => {
		await run('?spans=0');
		expect(walkOccurrencesWithSpans).not.toHaveBeenCalled();
		expect(getOccurrencesByIssueIdMock).toHaveBeenCalled();
	});
});
