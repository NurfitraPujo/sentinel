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
		assignedTo: 'assignedTo',
		assigneeType: 'assigneeType',
		firstSeen: 'firstSeen',
	},
	manualIssueReports: { issueId: 'issueId', reporterId: 'reporterId' },
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

describe('listReports needs-input tab excludes resolved/ignored (R7)', () => {
	it('filters on status = unresolved in addition to waitingOn IS NOT NULL', async () => {
		await listReports({ organizationId: 'org-1', tab: 'needs-input', userId: 'user-1' });

		const whereArg = dbMock.where.mock.calls[0][0];
		const serialized = JSON.stringify(whereArg);
		expect(serialized).toContain('waitingOn');
		expect(serialized).toContain('status');
		expect(serialized).toContain('unresolved');
	});

	it('the "all" tab does NOT filter on status', async () => {
		await listReports({ organizationId: 'org-1', tab: 'all', userId: 'user-1' });

		const whereArg = dbMock.where.mock.calls[0][0];
		const serialized = JSON.stringify(whereArg);
		expect(serialized).not.toContain('unresolved');
	});
});
