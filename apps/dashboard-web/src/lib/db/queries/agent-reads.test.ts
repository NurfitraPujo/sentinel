import { describe, it, expect, vi, beforeEach } from 'vitest';

// N1c (agent read endpoints) -- co-located unit tests for agent-reads.ts. Mirrors
// agent-work.test.ts's chainable db double.

function makeChainable() {
	const m: any = {};
	const methods = ['select', 'from', 'where', 'orderBy', 'limit', 'innerJoin', 'leftJoin'];
	for (const name of methods) {
		m[name] = vi.fn(() => m);
	}
	m.__result = [];
	// sentinel-user-report-issue SR-6: `getAgentReportDetail` issues TWO queries -- the org-member
	// companion first, then the external one -- so a single `__result` cannot express "no manual
	// row, but an external one". `__queue`, when non-empty, answers successive awaits in order;
	// `__result` stays the answer for every single-query case in this file.
	m.__queue = [] as unknown[][];
	m.then = vi.fn((resolve: any) => resolve(m.__queue.length > 0 ? m.__queue.shift() : m.__result));
	return m;
}

const dbMock: any = makeChainable();

vi.mock('$lib/server/db', () => ({ db: dbMock }));

const getAgentSettingsForProjects = vi.fn();
vi.mock('$lib/db/queries/agent-settings', () => ({ getAgentSettingsForProjects }));

vi.mock('$lib/db/schema', () => ({
	issues: { id: 'id' },
	projects: { id: 'id', organizationId: 'organizationId', name: 'name', isInbox: 'isInbox' },
	manualIssueReports: { issueId: 'issueId', bodyMd: 'bodyMd', severity: 'severity', reporterId: 'reporterId' },
	// sentinel-user-report-issue SR-6: getAgentReportDetail reads BOTH companions now.
	externalIssueReports: {
		issueId: 'issueId',
		bodyMd: 'bodyMd',
		severity: 'severity',
		context: 'context',
		externalReporterId: 'externalReporterId',
	},
	externalReporters: {
		id: 'id',
		displayName: 'displayName',
		email: 'email',
		externalId: 'externalId',
	},
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
		// sentinel-event-schema-v2 §5.5: the v2 columns, each mocked as its own property name.
		level: 'level',
		message: 'message',
		errorClass: 'errorClass',
		logger: 'logger',
		serverName: 'serverName',
		transaction: 'transaction',
		dist: 'dist',
		source: 'source',
		traceFlags: 'traceFlags',
		receivedAt: 'receivedAt',
		sdk: 'sdk',
		eventUser: 'eventUser',
		tags: 'tags',
		contexts: 'contexts',
		request: 'request',
		breadcrumbs: 'breadcrumbs',
		exception: 'exception',
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
	dbMock.__queue = [];
	getAgentSettingsForProjects.mockResolvedValue(new Map());
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
		expect(await getAgentReportDetail('issue-1')).toEqual({
			bodyMd: 'body',
			severity: 'high',
			reporterId: 'user-1',
			reporterKind: 'org_member',
			externalReporterId: null,
			reporterDisplayName: null,
			reporterEmail: null,
			reporterExternalId: null,
			context: null,
		});
	});

	// sentinel-user-report-issue SR-6 / §4.2's correction. This is the case the function got wrong:
	// an externally-submitted report has NO `manual_issue_reports` row, so the old single-table read
	// returned null and an agent saw a title and nothing else.
	it('returns the EXTERNAL companion when there is no org-member one', async () => {
		dbMock.__queue = [
			[],
			[
				{
					bodyMd: 'the save button spins forever',
					severity: 'high',
					context: { page: 'Visits', release: 'v31.70' },
					externalReporterId: 'ext-1',
					displayName: 'Sarah K.',
					email: null,
					externalId: 'u_8f3c41',
				},
			],
		];

		expect(await getAgentReportDetail('issue-1')).toEqual({
			bodyMd: 'the save button spins forever',
			severity: 'high',
			// An external reporter is DATA, never a Sentinel principal (§2), so `reporterId` -- a
			// `"user".id` -- stays null and the external id lives in its own field.
			reporterId: null,
			reporterKind: 'external',
			externalReporterId: 'ext-1',
			reporterDisplayName: 'Sarah K.',
			reporterEmail: null,
			reporterExternalId: 'u_8f3c41',
			context: { page: 'Visits', release: 'v31.70' },
		});
	});

	it('returns null when NEITHER companion exists (a system_error issue)', async () => {
		dbMock.__queue = [[], []];
		expect(await getAgentReportDetail('issue-1')).toBeNull();
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
	it('scopes to the given organizationId and defaults agentSettings when none exist', async () => {
		dbMock.__result = [{ id: 'p1', name: 'Web', isInbox: false }];
		getAgentSettingsForProjects.mockResolvedValue(new Map());
		const result = await listAgentProjects('org-1');
		expect(result).toEqual([
			{
				id: 'p1',
				name: 'Web',
				isInbox: false,
				agentSettings: { fixEnabled: false, maxPrsPerDay: null, repo: null },
			},
		]);
		expect(dbMock.where).toHaveBeenCalled();
		expect(getAgentSettingsForProjects).toHaveBeenCalledWith(['p1']);
	});

	it('attaches fixEnabled/maxPrsPerDay/repo (including testCmd) when settings exist', async () => {
		dbMock.__result = [{ id: 'p1', name: 'Web', isInbox: false }];
		getAgentSettingsForProjects.mockResolvedValue(
			new Map([
				[
					'p1',
					{
						fixEnabled: true,
						maxPrsPerDay: 5,
						repo: {
							projectId: 'p1',
							provider: 'github',
							owner: 'acme',
							repo: 'widgets',
							defaultBranch: 'main',
							testCmd: 'npm test',
							agentCmd: 'npm run fix',
							cloneDepth: 1,
							createdAt: null,
							updatedAt: null,
						},
					},
				],
			])
		);
		const result = await listAgentProjects('org-1');
		expect(result).toEqual([
			{
				id: 'p1',
				name: 'Web',
				isInbox: false,
				agentSettings: {
					fixEnabled: true,
					maxPrsPerDay: 5,
					repo: {
						provider: 'github',
						owner: 'acme',
						repo: 'widgets',
						defaultBranch: 'main',
						testCmd: 'npm test',
						agentCmd: 'npm run fix',
						cloneDepth: 1,
					},
				},
			},
		]);
	});
});

// sentinel-event-schema-v2 §8.1 / V7c: both agent occurrence reads pass ONE shared column object to
// select(), binding every v2 key to its errorOccurrences column -- `user` to `eventUser` (the column is
// event_user). Asserted on the ARGUMENT of the select call, not on a mock's return value (B10): a
// mock that returns whatever it is told cannot tell a read that selects the column from one that does not.
describe('agent occurrence column object (V7c)', () => {
	const v2Bindings = {
		level: 'level',
		message: 'message',
		errorClass: 'errorClass',
		logger: 'logger',
		serverName: 'serverName',
		transaction: 'transaction',
		dist: 'dist',
		source: 'source',
		traceFlags: 'traceFlags',
		receivedAt: 'receivedAt',
		sdk: 'sdk',
		user: 'eventUser',
		tags: 'tags',
		contexts: 'contexts',
		request: 'request',
		breadcrumbs: 'breadcrumbs',
		exception: 'exception',
	};
	const preV2Bindings = {
		id: 'id',
		environment: 'environment',
		platform: 'platform',
		releaseVersion: 'releaseVersion',
		stacktrace: 'stacktrace',
		metadata: 'metadata',
		traceId: 'traceId',
		createdAt: 'createdAt',
	};

	it('getLatestAgentOccurrence selects every pre-v2 and v2 column', async () => {
		dbMock.__result = [];
		await getLatestAgentOccurrence('issue-1');
		expect(dbMock.select).toHaveBeenCalledTimes(1);
		expect(dbMock.select.mock.calls[0][0]).toEqual({ ...preV2Bindings, ...v2Bindings });
	});

	it('listAgentOccurrences selects the same object', async () => {
		dbMock.__result = [];
		await listAgentOccurrences({ issueId: 'issue-1' });
		expect(dbMock.select).toHaveBeenCalledTimes(1);
		expect(dbMock.select.mock.calls[0][0]).toEqual({ ...preV2Bindings, ...v2Bindings });
	});

	it('the two reads share one object, so they cannot drift', async () => {
		await getLatestAgentOccurrence('issue-1');
		await listAgentOccurrences({ issueId: 'issue-1' });
		expect(dbMock.select.mock.calls[0][0]).toBe(dbMock.select.mock.calls[1][0]);
	});
});
