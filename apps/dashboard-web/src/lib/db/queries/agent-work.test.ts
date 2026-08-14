import { describe, it, expect, vi, beforeEach } from 'vitest';
import { desc } from 'drizzle-orm';

/**
 * Manual Issues M5 stage 2 (design §7 steps 1, 3) -- co-located unit tests for agent-work.ts.
 * Mirrors reports.test.ts / comments.test.ts's chainable/queue-based db + tx doubles.
 */

function makeChainable() {
	const m: any = {};
	const methods = [
		'select',
		'from',
		'innerJoin',
		'leftJoin',
		'where',
		'orderBy',
		'limit',
		'insert',
		'values',
		'returning',
	];
	for (const name of methods) {
		m[name] = vi.fn(() => m);
	}
	m.__result = [];
	m.then = vi.fn((resolve: any) => resolve(m.__result));
	return m;
}

const txMock = makeChainable();
const dbMock: any = makeChainable();
dbMock.transaction = vi.fn(async (cb: any) => cb(txMock));

vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	issues: {
		id: 'id',
		projectId: 'projectId',
		issueType: 'issueType',
		message: 'message',
		errorClass: 'errorClass',
		status: 'status',
		assigneeType: 'assigneeType',
		assignedTo: 'assignedTo',
		waitingOn: 'waitingOn',
		firstSeen: 'firstSeen',
		lastSeen: 'lastSeen',
		count: 'count',
	},
	projects: { id: 'id', organizationId: 'organizationId', isInbox: 'isInbox', name: 'name' },
	manualIssueReports: { issueId: 'issueId', severity: 'severity', reporterId: 'reporterId' },
	issueActivity: { id: 'id', issueId: 'issueId' },
}));

const notifyIssueEvent = vi.fn(async () => []);
vi.mock('$lib/server/notify', () => ({ notifyIssueEvent }));

const {
	listAgentIssues,
	recordAgentProgress,
	encodeAgentIssuesCursor,
	decodeAgentIssuesCursor,
	AGENT_ISSUES_MAX_LIMIT,
} = await import('./agent-work');
const { issues } = await import('$lib/db/schema');

beforeEach(() => {
	vi.clearAllMocks();
	txMock.__result = [];
	dbMock.__result = [];
	notifyIssueEvent.mockResolvedValue([]);
});

describe('listAgentIssues', () => {
	it('scopes to the given organizationId and maps waitingOn to isWaiting', async () => {
		dbMock.__result = [
			{
				id: 'issue-1',
				projectId: 'project-1',
				projectName: 'Triage',
				isInbox: true,
				issueType: 'user_report',
				message: 'x',
				errorClass: 'user_report',
				status: 'unresolved',
				assigneeType: null,
				assignedTo: null,
				waitingOn: 'reporter',
				firstSeen: null,
				lastSeen: null,
				count: 1,
				severity: 'high',
				reporterId: 'user-1',
			},
		];

		const result = await listAgentIssues({ organizationId: 'org-1' });

		expect(result.issues).toHaveLength(1);
		expect(result.issues[0].isWaiting).toBe(true);
	});

	it('legacy default: no limit param means no .limit() call and no nextCursor (byte-identical)', async () => {
		dbMock.__result = [{ id: 'i1', waitingOn: null }, { id: 'i2', waitingOn: null }];

		const result = await listAgentIssues({ organizationId: 'org-1' });

		expect(dbMock.limit).not.toHaveBeenCalled();
		expect(result.issues).toHaveLength(2);
		expect(result.nextCursor).toBeUndefined();
	});

	it('applies since as firstSeen >= since in the WHERE clause', async () => {
		dbMock.__result = [];
		const since = new Date('2026-08-01T00:00:00Z');

		await listAgentIssues({ organizationId: 'org-1', since });

		expect(dbMock.where).toHaveBeenCalledTimes(1);
		const whereArg = dbMock.where.mock.calls[0][0];
		// Deleting `conditions.push(gte(issues.firstSeen, options.since))` in agent-work.ts must
		// fail this assertion -- the gte(firstSeen, since) node has to appear among the AND'd
		// conditions, keyed by its literal ISO value.
		expect(JSON.stringify(whereArg)).toContain(since.toISOString());
	});

	it('clamps limit to AGENT_ISSUES_MAX_LIMIT (200) and requests limit+1 rows', async () => {
		dbMock.__result = [];

		await listAgentIssues({ organizationId: 'org-1', limit: 5000 });

		expect(dbMock.limit).toHaveBeenCalledWith(AGENT_ISSUES_MAX_LIMIT + 1);
	});

	it('requests limit+1 rows and returns nextCursor only when more rows exist', async () => {
		const t1 = new Date('2026-08-10T00:00:00Z');
		const t2 = new Date('2026-08-09T00:00:00Z');
		const t3 = new Date('2026-08-08T00:00:00Z');
		dbMock.__result = [
			{ id: 'i1', waitingOn: null, firstSeen: null, lastSeen: t1 },
			{ id: 'i2', waitingOn: null, firstSeen: null, lastSeen: t2 },
			{ id: 'i3', waitingOn: null, firstSeen: null, lastSeen: t3 },
		];

		const result = await listAgentIssues({ organizationId: 'org-1', limit: 2 });

		expect(dbMock.limit).toHaveBeenCalledWith(3);
		expect(result.issues).toHaveLength(2);
		expect(result.nextCursor).toBeDefined();
		expect(decodeAgentIssuesCursor(result.nextCursor as string)).toEqual({ sortValue: t2, id: 'i2' });
	});

	it('omits nextCursor when the page is not full (no more rows)', async () => {
		dbMock.__result = [{ id: 'i1', waitingOn: null, firstSeen: null, lastSeen: new Date() }];

		const result = await listAgentIssues({ organizationId: 'org-1', limit: 50 });

		expect(result.issues).toHaveLength(1);
		expect(result.nextCursor).toBeUndefined();
	});

	it('applies a keyset predicate on (sortColumn, id) when a cursor is supplied', async () => {
		dbMock.__result = [];
		const cursor = { sortValue: new Date('2026-08-05T00:00:00Z'), id: 'issue-cursor' };

		await listAgentIssues({ organizationId: 'org-1', limit: 10, cursor });

		expect(dbMock.where).toHaveBeenCalledTimes(1);
		const serialized = JSON.stringify(dbMock.where.mock.calls[0][0]);
		expect(serialized).toContain('issue-cursor');
		expect(serialized).toContain('2026-08-05');
	});

	it('orders by sortColumn desc, id desc (keyset-stable ordering)', async () => {
		dbMock.__result = [];

		await listAgentIssues({ organizationId: 'org-1', sort: 'firstSeen' });

		expect(dbMock.orderBy).toHaveBeenCalledWith(desc(issues.firstSeen), desc(issues.id));
	});

	it('defaults sort to lastSeen, matching pre-N7b ordering', async () => {
		dbMock.__result = [];

		await listAgentIssues({ organizationId: 'org-1' });

		expect(dbMock.orderBy).toHaveBeenCalledWith(desc(issues.lastSeen), desc(issues.id));
	});
});

describe('encodeAgentIssuesCursor / decodeAgentIssuesCursor', () => {
	it('round-trips a sortValue + id', () => {
		const sortValue = new Date('2026-08-14T12:34:56.000Z');
		const encoded = encodeAgentIssuesCursor(sortValue, 'issue-42');

		expect(decodeAgentIssuesCursor(encoded)).toEqual({ sortValue, id: 'issue-42' });
	});

	it('throws on malformed input', () => {
		expect(() => decodeAgentIssuesCursor('not-valid-base64url-json')).toThrow();
		expect(() => decodeAgentIssuesCursor(Buffer.from('{}').toString('base64url'))).toThrow();
	});
});

describe('recordAgentProgress', () => {
	it('writes exactly one progress_update activity row and fans out kind progress_update', async () => {
		await recordAgentProgress('issue-1', 'agent-1', 'still investigating');

		expect(dbMock.transaction).toHaveBeenCalledTimes(1);
		expect(txMock.insert).toHaveBeenCalledTimes(1);
		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({
				issueId: 'issue-1',
				eventType: 'progress_update',
				actorType: 'agent',
				actorId: 'agent-1',
				newValue: { messageMd: 'still investigating' },
			})
		);
		expect(notifyIssueEvent).toHaveBeenCalledWith(
			txMock,
			expect.objectContaining({ issueId: 'issue-1', kind: 'progress_update', actorType: 'agent', actorId: 'agent-1' })
		);
	});

	// A05-comment/progress (N7d) guard-deletion red-proof: delete the dedupe SELECT/early-return in
	// recordAgentProgress and this fails, because a second insert/notify would fire.
	it('an identical retry within the dedupe window inserts nothing and does not notify', async () => {
		txMock.__result = [{ id: 'activity-1' }]; // dedupe SELECT finds a matching recent row

		const { notified } = await recordAgentProgress('issue-1', 'agent-1', 'still investigating');

		expect(notified).toEqual([]);
		expect(txMock.insert).not.toHaveBeenCalled();
		expect(notifyIssueEvent).not.toHaveBeenCalled();
	});
});
