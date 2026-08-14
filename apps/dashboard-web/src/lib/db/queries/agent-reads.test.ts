import { describe, it, expect, vi, beforeEach } from 'vitest';

// N1c (agent read endpoints) -- co-located unit tests for agent-reads.ts. Mirrors
// agent-work.test.ts's chainable db double.

function makeChainable() {
	const m: any = {};
	const methods = ['select', 'from', 'where', 'orderBy', 'limit'];
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
	issues: { id: 'id' },
	projects: { id: 'id', organizationId: 'organizationId', name: 'name', isInbox: 'isInbox' },
	manualIssueReports: { issueId: 'issueId', bodyMd: 'bodyMd', severity: 'severity', reporterId: 'reporterId' },
	errorOccurrences: {
		id: 'id',
		issueId: 'issueId',
		environment: 'environment',
		platform: 'platform',
		releaseVersion: 'releaseVersion',
		stacktrace: 'stacktrace',
		metadata: 'metadata',
		traceId: 'traceId',
		createdAt: 'createdAt',
	},
}));

const {
	getAgentIssueDetail,
	getAgentReportDetail,
	getLatestAgentOccurrence,
	listAgentOccurrences,
	listAgentProjects,
} = await import('./agent-reads');

beforeEach(() => {
	vi.clearAllMocks();
	dbMock.__result = [];
});

describe('getAgentIssueDetail', () => {
	it('returns null when no row is found', async () => {
		dbMock.__result = [];
		expect(await getAgentIssueDetail('issue-1')).toBeNull();
	});

	it('returns the row when found', async () => {
		dbMock.__result = [{ id: 'issue-1', message: 'x' }];
		expect(await getAgentIssueDetail('issue-1')).toEqual({ id: 'issue-1', message: 'x' });
	});
});

describe('getAgentReportDetail', () => {
	it('returns null for a system_error issue (no companion row)', async () => {
		dbMock.__result = [];
		expect(await getAgentReportDetail('issue-1')).toBeNull();
	});

	it('returns bodyMd/severity/reporterId for a user_report issue', async () => {
		dbMock.__result = [{ bodyMd: 'body', severity: 'high', reporterId: 'user-1' }];
		expect(await getAgentReportDetail('issue-1')).toEqual({ bodyMd: 'body', severity: 'high', reporterId: 'user-1' });
	});
});

describe('getLatestAgentOccurrence', () => {
	it('returns null when the issue has no occurrences', async () => {
		dbMock.__result = [];
		expect(await getLatestAgentOccurrence('issue-1')).toBeNull();
	});

	it('returns the newest occurrence', async () => {
		dbMock.__result = [{ id: 'occ-1' }];
		expect(await getLatestAgentOccurrence('issue-1')).toEqual({ id: 'occ-1' });
		expect(dbMock.orderBy).toHaveBeenCalled();
		expect(dbMock.limit).toHaveBeenCalledWith(1);
	});
});

describe('listAgentOccurrences', () => {
	it('defaults to a limit of 20', async () => {
		await listAgentOccurrences({ issueId: 'issue-1' });
		expect(dbMock.limit).toHaveBeenCalledWith(20);
	});

	it('clamps a limit above 50 down to 50', async () => {
		await listAgentOccurrences({ issueId: 'issue-1', limit: 500 });
		expect(dbMock.limit).toHaveBeenCalledWith(50);
	});

	it('clamps a limit below 1 up to 1', async () => {
		await listAgentOccurrences({ issueId: 'issue-1', limit: 0 });
		expect(dbMock.limit).toHaveBeenCalledWith(1);
	});

	it('passes an explicit valid limit through unchanged', async () => {
		await listAgentOccurrences({ issueId: 'issue-1', limit: 10 });
		expect(dbMock.limit).toHaveBeenCalledWith(10);
	});
});

describe('listAgentProjects', () => {
	it('scopes to the given organizationId', async () => {
		dbMock.__result = [{ id: 'p1', name: 'Web', isInbox: false }];
		const result = await listAgentProjects('org-1');
		expect(result).toEqual([{ id: 'p1', name: 'Web', isInbox: false }]);
		expect(dbMock.where).toHaveBeenCalled();
	});
});
