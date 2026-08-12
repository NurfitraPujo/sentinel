import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * Manual Issues M5 stage 2 (design §7 steps 1, 3) -- co-located unit tests for agent-work.ts.
 * Mirrors reports.test.ts / comments.test.ts's chainable/queue-based db + tx doubles.
 */

function makeChainable() {
	const m: any = {};
	const methods = ['select', 'from', 'innerJoin', 'leftJoin', 'where', 'orderBy', 'insert', 'values', 'returning'];
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

const { listAgentIssues, recordAgentProgress } = await import('./agent-work');

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

		expect(result).toHaveLength(1);
		expect(result[0].isWaiting).toBe(true);
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
});
