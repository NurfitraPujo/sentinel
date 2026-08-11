import { describe, it, expect, vi, beforeEach } from 'vitest';

// Chainable Drizzle-query double, mirroring queries/issues.test.ts's approach: every method
// returns the same object so `.insert(x).values(y).returning()` / `.update(x).set(y).where(z)
// .returning()` / `.select(...).from(...).where(...)` chains all resolve, and `__result` decides
// what awaiting the chain yields.
function makeChainable() {
	const m: any = {};
	const methods = [
		'select',
		'from',
		'innerJoin',
		'leftJoin',
		'where',
		'orderBy',
		'insert',
		'values',
		'returning',
		'update',
		'set',
		'delete',
		'onConflictDoNothing',
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
	issues: { id: 'id', projectId: 'projectId', assignedTo: 'assignedTo', issueType: 'issueType' },
	issueActivity: { id: 'id', issueId: 'issueId' },
	manualIssueReports: { issueId: 'issueId', reporterId: 'reporterId' },
	projects: { id: 'id', organizationId: 'organizationId', isInbox: 'isInbox', name: 'name' },
	users: { id: 'id', name: 'name', email: 'email' },
	issueSubscriptions: {
		id: 'id',
		issueId: 'issueId',
		subscriberType: 'subscriberType',
		subscriberId: 'subscriberId',
	},
	notifications: {
		id: 'id',
		userId: 'userId',
		issueId: 'issueId',
		kind: 'kind',
		createdAt: 'createdAt',
	},
}));

vi.mock('./issues', () => ({
	getIssueActivity: vi.fn(),
}));

const { createManualIssue, claimIssue, releaseClaim, ClaimConflictError, moveIssueToProject } =
	await import('./reports');

beforeEach(() => {
	vi.clearAllMocks();
	txMock.__result = [];
	dbMock.__result = [];
	// listSubscribers (called from inside notifyIssueEvent) runs a plain select against tx that
	// resolves via txMock.then -- default it to "no subscribers" so claim/release tests don't have
	// to think about fan-out unless they're the ones testing it.
	txMock.then = vi.fn((resolve: any) => resolve([]));
});

describe('createManualIssue', () => {
	it('creates the issues row, the manual_issue_reports companion, one activity row, and auto-subscribes the reporter, inside a single transaction', async () => {
		// The inserts below run in sequence against tx; queue each RETURNING result in order.
		let call = 0;
		txMock.returning = vi.fn(() => {
			call += 1;
			if (call === 1) return Promise.resolve([{ id: 'issue-1', projectId: 'project-1' }]);
			if (call === 2) return Promise.resolve([{ issueId: 'issue-1', reporterId: 'user-1' }]);
			return Promise.resolve([]);
		});

		const result = await createManualIssue({
			organizationId: 'org-1',
			projectId: 'project-1',
			reporterId: 'user-1',
			title: 'Cannot log in',
			bodyMd: 'Steps to reproduce...',
			severity: 'high',
		});

		expect(dbMock.transaction).toHaveBeenCalledTimes(1);
		expect(result.issue).toEqual({ id: 'issue-1', projectId: 'project-1' });
		expect(result.report).toEqual({ issueId: 'issue-1', reporterId: 'user-1' });
		// insert called 4 times: issues, manual_issue_reports, issue_activity, issue_subscriptions
		// (the reporter auto-subscribe).
		expect(txMock.insert).toHaveBeenCalledTimes(4);
		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({
				issueId: 'issue-1',
				subscriberType: 'user',
				subscriberId: 'user-1',
				reason: 'reporter',
			})
		);
		// Idempotent upsert: subscribe() always goes through onConflictDoNothing, never a plain
		// insert that would 23505 on a re-subscribe.
		expect(txMock.onConflictDoNothing).toHaveBeenCalled();
	});

	it('rejects an empty title before opening a transaction', async () => {
		await expect(
			createManualIssue({
				organizationId: 'org-1',
				projectId: 'project-1',
				reporterId: 'user-1',
				title: '   ',
				bodyMd: 'body',
				severity: 'low',
			})
		).rejects.toThrow('title must not be empty');

		expect(dbMock.transaction).not.toHaveBeenCalled();
	});
});

describe('claimIssue', () => {
	// This is the test that proves the atomic-claim conflict logic: the UPDATE is scoped
	// WHERE assigned_to IS NULL, so a second claimant racing the first gets 0 rows back, and the
	// function must surface that as a distinguishable failure (ClaimConflictError) rather than
	// silently succeeding or returning undefined (D18: the caller must be able to tell "I claimed
	// it" from "I did nothing").
	it('throws ClaimConflictError when the conditional UPDATE matches zero rows', async () => {
		txMock.returning = vi.fn(() => Promise.resolve([]));

		await expect(claimIssue('issue-1', 'user', 'user-2')).rejects.toBeInstanceOf(ClaimConflictError);

		// No activity should be written for a claim that did not happen (the UPDATE is not an
		// `insert` call at all).
		expect(txMock.insert).not.toHaveBeenCalled();
	});

	it('succeeds, writes one "claimed" activity row, auto-subscribes the claimant, and fans out to other subscribers, excluding the actor', async () => {
		txMock.returning = vi.fn(() =>
			Promise.resolve([{ id: 'issue-1', assigneeType: 'user', assignedTo: 'user-1' }])
		);
		// listSubscribers returns two USER subscribers: the reporter ('user-2', notified) and the
		// claimant themselves ('user-1', excluded as the actor).
		txMock.then = vi.fn((resolve: any) =>
			resolve([
				{ subscriberType: 'user', subscriberId: 'user-2' },
				{ subscriberType: 'user', subscriberId: 'user-1' },
			])
		);

		const { issue, notified } = await claimIssue('issue-1', 'user', 'user-1');

		expect(issue).toEqual({ id: 'issue-1', assigneeType: 'user', assignedTo: 'user-1' });
		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({ eventType: 'claimed', actorType: 'user', actorId: 'user-1' })
		);
		// Auto-subscribe (reason 'claimant').
		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({ subscriberId: 'user-1', reason: 'claimant' })
		);
		// notifyIssueEvent excludes the actor: only user-2 is notified, never user-1.
		expect(notified).toEqual([{ userId: 'user-2', kind: 'claimed' }]);
		expect(txMock.values).toHaveBeenCalledWith(
			expect.arrayContaining([expect.objectContaining({ userId: 'user-2', kind: 'claimed' })])
		);
	});

	it('does not auto-subscribe an agent claimant (agents get no notifications row in M4)', async () => {
		txMock.returning = vi.fn(() =>
			Promise.resolve([{ id: 'issue-1', assigneeType: 'agent', assignedTo: 'agent-1' }])
		);
		txMock.then = vi.fn((resolve: any) => resolve([]));

		await claimIssue('issue-1', 'agent', 'agent-1');

		const subscribeCalls = (txMock.values as any).mock.calls.filter(
			([arg]: any[]) => arg && typeof arg === 'object' && 'reason' in arg
		);
		expect(subscribeCalls).toHaveLength(0);
	});
});

describe('releaseClaim', () => {
	it('throws ClaimConflictError when a non-force release matches zero rows (not the current claimant)', async () => {
		txMock.returning = vi.fn(() => Promise.resolve([]));

		await expect(releaseClaim('issue-1', 'user-2')).rejects.toBeInstanceOf(ClaimConflictError);
	});

	it('force release succeeds, writes a claim_released activity row, and fans out a "claimed" notification with released:true', async () => {
		txMock.returning = vi.fn(() => Promise.resolve([{ id: 'issue-1', assignedTo: null }]));
		txMock.then = vi.fn((resolve: any) => resolve([{ subscriberType: 'user', subscriberId: 'user-2' }]));

		const { issue, notified } = await releaseClaim('issue-1', 'admin-1', { force: true });

		expect(issue).toEqual({ id: 'issue-1', assignedTo: null });
		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({ eventType: 'claim_released', newValue: { force: true } })
		);
		expect(notified).toEqual([{ userId: 'user-2', kind: 'claimed' }]);
		expect(txMock.values).toHaveBeenCalledWith(
			expect.arrayContaining([
				expect.objectContaining({ userId: 'user-2', kind: 'claimed', payload: { released: true, force: true } }),
			])
		);
	});
});

describe('moveIssueToProject', () => {
	it('throws when the issue does not exist, without writing anything', async () => {
		txMock.where = vi.fn(() => txMock);
		txMock.__result = [];

		await expect(moveIssueToProject('missing-issue', 'project-2', 'user', 'user-1')).rejects.toThrow(
			'not found'
		);
	});
});
