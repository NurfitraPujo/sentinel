import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * R7 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): the "needs-input" tab filtered only on
 * `waitingOn IS NOT NULL`, so a report resolved/ignored WHILE still `waitingOn` (before
 * updateIssueStatus started clearing it) stayed stuck in that tab forever. This proves
 * `listReports('needs-input')` now also requires `status = 'unresolved'`.
 *
 * Dedicated double (rather than reusing reports.test.ts's chainable mock) because `listReports`
 * needs `issueComments` in the schema mock for its commentCount subquery, which that file's mock
 * doesn't provide.
 */

function makeChainable() {
	const m: any = {};
	const methods = ['select', 'from', 'innerJoin', 'leftJoin', 'where', 'orderBy'];
	for (const name of methods) {
		m[name] = vi.fn(() => m);
	}
	m.__result = [];
	m.then = vi.fn((resolve: any) => resolve(m.__result));
	return m;
}

const dbMock: any = makeChainable();
vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	issues: {
		id: 'id',
		projectId: 'projectId',
		issueType: 'issueType',
		status: 'status',
		waitingOn: 'waitingOn',
		sourceChannel: 'sourceChannel',
		assignedTo: 'assignedTo',
		assigneeType: 'assigneeType',
		firstSeen: 'firstSeen',
		// sentinel-project-environments §2.2, decision 36: listReports composes an environment
		// predicate over these two, resolved from the caller's own grant rows.
		environment: 'environment',
	},
	projectEnvironmentGrants: { userId: 'userId', projectId: 'projectId', environments: 'environments' },
	manualIssueReports: { issueId: 'issueId', reporterId: 'reporterId' },
	// sentinel-user-report-issue §4.2's correction: listReports left-joins the external companion.
	externalIssueReports: { issueId: 'issueId', externalReporterId: 'externalReporterId' },
	externalReporters: { id: 'id', displayName: 'displayName', email: 'email' },
	projects: { id: 'id', organizationId: 'organizationId', isInbox: 'isInbox', name: 'name' },
	users: { id: 'id', name: 'name', email: 'email' },
	issueComments: { id: 'id', issueId: 'issueId' },
	issueActivity: { id: 'id', issueId: 'issueId' },
	attachments: {},
}));

const { listReports } = await import('./reports');

beforeEach(() => {
	vi.clearAllMocks();
	dbMock.__result = [];
});

/**
 * sentinel-project-environments §2.2, decision 36: `listReports` now resolves the caller's
 * environment grants FIRST, on the same `db` double, so the query under test is the LAST `where`
 * call rather than the first. Reading `calls[0]` here would assert against the grant lookup's
 * predicate and pass or fail for reasons that have nothing to do with the tab.
 */
function lastWhereArg() {
	const calls = dbMock.where.mock.calls;
	return calls[calls.length - 1][0];
}

describe('listReports needs-input tab excludes resolved/ignored (R7)', () => {
	it('filters on status = unresolved in addition to waitingOn IS NOT NULL', async () => {
		await listReports({ organizationId: 'org-1', tab: 'needs-input', userId: 'user-1' });

		const whereArg = lastWhereArg();
		const serialized = JSON.stringify(whereArg);
		expect(serialized).toContain('waitingOn');
		expect(serialized).toContain('status');
		expect(serialized).toContain('unresolved');
	});

	it('the "all" tab does NOT filter on status', async () => {
		await listReports({ organizationId: 'org-1', tab: 'all', userId: 'user-1' });

		const whereArg = lastWhereArg();
		const serialized = JSON.stringify(whereArg);
		expect(serialized).not.toContain('unresolved');
	});
});
