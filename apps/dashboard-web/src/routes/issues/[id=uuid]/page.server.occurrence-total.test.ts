import { describe, it, expect, vi, beforeEach } from 'vitest';
import { errorOccurrences, issueOccurrenceBuckets } from '$lib/db/schema';

// Flow `D2`, sentinel-issue-page-slow-count BUG-01: the `Occurrence History (N)` total. The loader
// passes the issue row's `issue_type` to the REAL `countOccurrencesByIssueId`, and the table that
// reader selects from is asserted (B10): a system-error issue sums its hourly buckets, a long-retention
// (`user_report`, retention.ts's `manualRetentionDays`) issue takes the exact `COUNT(*)`. Only the
// Drizzle `db` is mocked; it answers a different number per table, so the returned total also says
// which source was read. What each source returns against real Postgres is
// issue-queries.occurrence-total.integration.test.ts's.

const fromTables: unknown[] = [];
const ANSWERS = new Map<unknown, number>([
	[issueOccurrenceBuckets, 1_788_214],
	[errorOccurrences, 3],
]);
function makeDb() {
	let table: unknown = null;
	const m: any = {};
	m.select = vi.fn(() => m);
	m.from = vi.fn((t: unknown) => {
		table = t;
		fromTables.push(t);
		return m;
	});
	m.where = vi.fn(() => m);
	m.then = (resolve: any, reject: any) =>
		Promise.resolve([{ count: ANSWERS.get(table) ?? -1 }]).then(resolve, reject);
	return m;
}
const dbMock = makeDb();
vi.mock('$lib/server/db', () => ({ db: dbMock }));

const getIssueByIdMock = vi.fn();
vi.mock('$env/dynamic/private', () => ({ env: {} }));
vi.mock('$lib/server/issue-access', () => ({
	requireProjectAccess: vi.fn(async () => ({ organizationId: 'org-1' })),
}));
vi.mock('$lib/server/environment-access', () => ({
	resolveEnvironmentScope: vi.fn(async () => null),
	resolveEnvironmentScopes: vi.fn(async () => new Map()),
	environmentScopeCondition: vi.fn(),
}));
vi.mock('$lib/db/queries/environments', () => ({ resolveEnvironmentDisplayName: vi.fn(async () => 'Production') }));
vi.mock('$lib/server/queries/issue-queries', async (importOriginal) => {
	const actual = await importOriginal<typeof import('$lib/server/queries/issue-queries')>();
	return {
		...actual,
		issueQueries: {
			...actual.issueQueries,
			getIssueById: getIssueByIdMock,
			getProjectById: vi.fn(async () => ({ id: 'proj-1', name: 'P' })),
			getOccurrencesByIssueId: vi.fn(async () => []),
			trendForIssue: vi.fn(async () => ({ hourly: [], daily: [] })),
			getTagFacets: vi.fn(async () => []),
			// countOccurrencesByIssueId: the real one, against dbMock.
		},
	};
});
vi.mock('$lib/db/queries/issues', () => ({ getIssueRelations: vi.fn(async () => []) }));
vi.mock('$lib/db/queries/releases', () => ({ listPickerReleases: vi.fn(async () => []) }));

const { load } = await import('./+page.server');

const ISSUE = {
	id: '11111111-1111-4111-8111-111111111111',
	projectId: 'proj-1',
	message: 'boom',
	environment: 'production',
	status: 'unresolved',
};

function loadAs() {
	return load({
		params: { id: ISSUE.id },
		locals: { auth: async () => ({ user: { id: 'user-1' } }) },
	} as any) as Promise<any>;
}

beforeEach(() => {
	vi.clearAllMocks();
	fromTables.length = 0;
});

describe('/issues/[id] load — the occurrence total (flow D2)', () => {
	it('reads a system-error issue’s total from the hourly bucket sum, never COUNT(*) over error_occurrences', async () => {
		getIssueByIdMock.mockResolvedValue({ ...ISSUE, issueType: 'system_error' });

		const result = await loadAs();

		expect(fromTables).toEqual([issueOccurrenceBuckets]);
		expect(result.occurrenceTotal).toBe(1_788_214);
	});

	it('takes the exact COUNT(*) for a long-retention (user_report) issue', async () => {
		getIssueByIdMock.mockResolvedValue({ ...ISSUE, issueType: 'user_report' });

		const result = await loadAs();

		expect(fromTables).toEqual([errorOccurrences]);
		expect(result.occurrenceTotal).toBe(3);
	});
});
