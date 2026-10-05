import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * sentinel-cross-repo-trace-view D5 — the loader hands the trace links their data AFTER both access
 * checks, asks for the related-errors count for the SELECTED occurrence only, and asks for nothing
 * when no occurrence has a trace. What the count IS (the same guarded conditions as the trace page)
 * is trace-view.integration.test.ts's boundary; this file owns WHEN and WITH WHAT it is asked.
 */

const authMock = vi.fn();
const requireProjectAccessMock = vi.fn();
const loadTraceLinksMock = vi.fn();
const getOccurrenceDetailMock = vi.fn();
const getOccurrencesByIssueIdMock = vi.fn();

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
		countOccurrencesByIssueId: vi.fn(async () => 2),
		trendForIssue: vi.fn(async () => ({ hourly: [], daily: [] })),
		getOccurrenceDetail: getOccurrenceDetailMock,
		occurrenceExistsOutsideIssue: vi.fn(async () => false),
		getTagFacets: vi.fn(async () => []),
	},
}));
vi.mock('$lib/db/queries/issues', () => ({ getIssueRelations: vi.fn(async () => []) }));
vi.mock('$lib/db/queries/releases', () => ({ listPickerReleases: vi.fn(async () => []) }));
vi.mock('$lib/server/trace-links', () => ({ loadTraceLinks: loadTraceLinksMock }));

const { load } = await import('./+page.server');

const ISSUE = { id: '11111111-1111-4111-8111-111111111111', projectId: 'proj-1', message: 'boom', environment: 'production', status: 'unresolved', issueType: 'system_error' };
const FIRST = 'aaaaaaaa-0000-4000-8000-000000000001';
const SECOND = 'aaaaaaaa-0000-4000-8000-000000000002';
const TRACE_1 = '1'.repeat(32);
const TRACE_2 = '2'.repeat(32);

const row = (id: string, traceId: string | null) => ({
	id, issueId: ISSUE.id, level: 'error', traceId, stacktrace: [], metadata: {}, sdk: null, createdAt: new Date(),
});
const run = (query = '') =>
	(load as any)({
		params: { id: ISSUE.id },
		locals: { auth: authMock },
		url: new URL(`http://localhost/issues/${ISSUE.id}${query}`),
	});

beforeEach(() => {
	vi.clearAllMocks();
	authMock.mockResolvedValue({ user: { id: 'user-1' } });
	requireProjectAccessMock.mockResolvedValue({ projectId: 'proj-1', organizationId: 'org-1' });
	getOccurrencesByIssueIdMock.mockResolvedValue([row(FIRST, TRACE_1), row(SECOND, TRACE_2)]);
	getOccurrenceDetailMock.mockImplementation(async (_i: string, id: string) => ({ id, traceId: id === FIRST ? TRACE_1 : TRACE_2 }));
	loadTraceLinksMock.mockResolvedValue({ orgSlug: 'acme', related: { count: 2, capped: false } });
});

describe('the trace links data', () => {
	it('is asked for the DEFAULT-selected occurrence’s trace, scoped to the issue’s project and org', async () => {
		const data = await run();
		expect(loadTraceLinksMock).toHaveBeenCalledTimes(1);
		expect(loadTraceLinksMock).toHaveBeenCalledWith({
			userId: 'user-1', organizationId: 'org-1', projectId: 'proj-1', selectedTraceId: TRACE_1,
		});
		expect(data.traceOrgSlug).toBe('acme');
		expect(data.relatedTraceErrors).toEqual({ count: 2, capped: false });
	});

	it('follows ?occurrence= to the OTHER occurrence’s trace (one call, not one per card)', async () => {
		await run(`?occurrence=${SECOND}`);
		expect(loadTraceLinksMock).toHaveBeenCalledTimes(1);
		expect(loadTraceLinksMock.mock.calls[0][0].selectedTraceId).toBe(TRACE_2);
	});

	it('is not asked at all when no occurrence has a trace id', async () => {
		getOccurrencesByIssueIdMock.mockResolvedValue([row(FIRST, null)]);
		getOccurrenceDetailMock.mockResolvedValue({ id: FIRST, traceId: null });
		const data = await run();
		expect(loadTraceLinksMock).not.toHaveBeenCalled();
		expect(data.traceOrgSlug).toBeNull();
		expect(data.relatedTraceErrors).toBeNull();
	});

	it('is not reached when the access check fails', async () => {
		requireProjectAccessMock.mockRejectedValue(Object.assign(new Error('no'), { status: 403 }));
		await expect(run()).rejects.toMatchObject({ status: 403 });
		expect(loadTraceLinksMock).not.toHaveBeenCalled();
	});
});
