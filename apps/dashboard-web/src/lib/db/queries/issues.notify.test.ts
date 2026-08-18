import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8) -- co-located, shuffle-safe unit
 * tests for the auto-subscribe + notify.ts fan-out wiring added to queries/issues.ts
 * (updateIssueStatus, assignIssue, createIssueRelation). Kept in its own file rather than folded
 * into issues.test.ts's batchUpdateIssues-focused chainable mock, since this needs the
 * subscriptions/notifications schema tokens and the `tx.select` chain notify.ts's
 * `listSubscribers` relies on -- issues.test.ts's double doesn't need either.
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
	issues: { id: 'id', projectId: 'projectId', status: 'status', assignedTo: 'assignedTo' },
	issueActivity: { id: 'id', issueId: 'issueId' },
	issueRelations: {
		id: 'id',
		sourceIssueId: 'sourceIssueId',
		targetIssueId: 'targetIssueId',
		relationType: 'relationType',
	},
	projects: { id: 'id', organizationId: 'organizationId' },
	organizationMembers: { organizationId: 'organizationId', userId: 'userId' },
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
		emailedAt: 'emailedAt',
	},
}));

// notifyIssueEvent (notify.ts, R1) queries current org membership via `tx` right after
// listSubscribers. This chainable `tx` double resolves EVERY bare-awaited chain (not just
// selects) through the same `.then`, so calls made by the surrounding mutation BEFORE
// notifyIssueEvent runs (an update, a plain `.insert().values()` with no `.returning()`, etc.)
// also consume a slot -- `dummyCallsBefore` accounts for those so the subscribers/members
// results land on the right calls.
function queueSubscribersThenMembers(subscribers: unknown[], memberUserIds: string[], dummyCallsBefore = 0) {
	const fn = vi.fn();
	for (let i = 0; i < dummyCallsBefore; i++) {
		fn.mockImplementationOnce((resolve: any) => resolve([]));
	}
	fn.mockImplementationOnce((resolve: any) => resolve(subscribers));
	fn.mockImplementationOnce((resolve: any) => resolve(memberUserIds.map((userId) => ({ userId }))));
	// notifyIssueEvent's own `notifications` insert (bare-awaited, no `.returning()`) is the NEXT
	// call whenever any target survives filtering -- a trailing dummy so that await resolves
	// instead of falling through to the base (no-op) implementation and hanging.
	fn.mockImplementationOnce((resolve: any) => resolve(undefined));
	txMock.then = fn;
}

const { updateIssueStatus, assignIssue, createIssueRelation, AgentAssignmentError } = await import('./issues');

beforeEach(() => {
	vi.clearAllMocks();
	txMock.__result = [];
	txMock.then = vi.fn((resolve: any) => resolve([]));
});

describe('updateIssueStatus', () => {
	it('fans out kind "resolved" (not "status_changed") when the new status is resolved, excluding the actor', async () => {
		queueSubscribersThenMembers(
			[
				{ subscriberType: 'user', subscriberId: 'reporter-1' },
				{ subscriberType: 'user', subscriberId: 'actor-1' },
			],
			['reporter-1', 'actor-1'],
			3
		);

		const { notified } = await updateIssueStatus('issue-1', 'resolved', '1.2.3', 'user', 'actor-1');

		expect(notified).toEqual([{ userId: 'reporter-1', kind: 'resolved' }]);
	});

	it('fans out kind "status_changed" for a non-resolved transition', async () => {
		queueSubscribersThenMembers([{ subscriberType: 'user', subscriberId: 'reporter-1' }], ['reporter-1'], 3);

		const { notified } = await updateIssueStatus('issue-1', 'ignored', undefined, 'user', 'actor-1');

		expect(notified).toEqual([{ userId: 'reporter-1', kind: 'status_changed' }]);
	});

	it('excludes agent subscribers from the notified list (they poll, no notifications row in M4)', async () => {
		queueSubscribersThenMembers(
			[
				{ subscriberType: 'agent', subscriberId: 'agent-1' },
				{ subscriberType: 'user', subscriberId: 'user-1' },
			],
			['user-1'],
			3
		);

		const { notified } = await updateIssueStatus('issue-1', 'unresolved', undefined, 'agent', 'agent-2');

		expect(notified).toEqual([{ userId: 'user-1', kind: 'status_changed' }]);
	});

	// R1 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): a subscriber who is no longer an org
	// member must never be notified, even though their issue_subscriptions row still exists.
	it('R1: excludes a subscriber who is subscribed but no longer a current org member', async () => {
		queueSubscribersThenMembers(
			[
				{ subscriberType: 'user', subscriberId: 'reporter-1' },
				{ subscriberType: 'user', subscriberId: 'ex-member-1' },
			],
			['reporter-1'], // ex-member-1 removed from the org, no longer in the membership rows
			3
		);

		const { notified } = await updateIssueStatus('issue-1', 'ignored', undefined, 'user', 'actor-1');

		expect(notified).toEqual([{ userId: 'reporter-1', kind: 'status_changed' }]);
	});
});

describe('assignIssue', () => {
	it('auto-subscribes a USER assignee with reason "claimant"', async () => {
		await assignIssue('issue-1', 'user', 'user-2', 'user', 'admin-1');

		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({ subscriberId: 'user-2', reason: 'claimant' })
		);
	});

	// CONTEXT.md "Claim": claims are only ever self-acquired — nothing assigns an issue *to* an
	// agent on its behalf. Guard-deletion red-proof: remove the assigneeType==='agent' check in
	// assignIssue and this fails, because the UPDATE would proceed.
	it('rejects assigning to an AGENT (claims are self-acquired only)', async () => {
		await expect(assignIssue('issue-1', 'agent', 'agent-1', 'user', 'admin-1')).rejects.toBeInstanceOf(
			AgentAssignmentError
		);
		expect(txMock.update).not.toHaveBeenCalled();
	});

	it('does not auto-subscribe on unassign (assignedTo null)', async () => {
		await assignIssue('issue-1', null, null, 'user', 'admin-1');

		const subscribeCalls = (txMock.values as any).mock.calls.filter(
			([arg]: any[]) => arg && typeof arg === 'object' && 'reason' in arg
		);
		expect(subscribeCalls).toHaveLength(0);
	});

	it('unassign clears claimedAt (release path, not a bare field write)', async () => {
		await assignIssue('issue-1', null, null, 'user', 'admin-1');

		expect(txMock.set).toHaveBeenCalledWith(
			expect.objectContaining({ assigneeType: null, assignedTo: null, claimedAt: null })
		);
	});

	it('unassigning an agent-claimed issue emits claim_released, not unassigned', async () => {
		// First resolved chain is assignIssue's prior-state SELECT: the issue is agent-claimed.
		txMock.then = vi
			.fn()
			.mockImplementationOnce((resolve: any) => resolve([{ assigneeType: 'agent', assignedTo: 'agent-1' }]))
			.mockImplementation((resolve: any) => resolve([]));

		await assignIssue('issue-1', null, null, 'user', 'admin-1');

		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({ eventType: 'claim_released', actorType: 'user', actorId: 'admin-1' })
		);
		// B5 cross-boundary contract: tools/sentinel-worker's dispatcher (loop.Classify,
		// KindSweepReconcile) identifies its own released claims by newValue.previousAssignee —
		// the same shape the stale-claim reaper writes (retention.ts). Regressing this field
		// silently breaks the Agent Worker's reaped-claim reconciliation.
		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({
				eventType: 'claim_released',
				newValue: expect.objectContaining({ previousAssignee: 'agent-1' })
			})
		);
	});

	it('unassigning a user-assigned issue still emits unassigned', async () => {
		txMock.then = vi
			.fn()
			.mockImplementationOnce((resolve: any) => resolve([{ assigneeType: 'user', assignedTo: 'user-2' }]))
			.mockImplementation((resolve: any) => resolve([]));

		await assignIssue('issue-1', null, null, 'user', 'admin-1');

		expect(txMock.values).toHaveBeenCalledWith(expect.objectContaining({ eventType: 'unassigned' }));
	});
});

describe('createIssueRelation', () => {
	it('fans out kind "linked" scoped to the source issue, excluding the actor', async () => {
		txMock.returning = vi.fn(() =>
			Promise.resolve([{ id: 'rel-1', sourceIssueId: 'issue-1', targetIssueId: 'issue-2' }])
		);
		queueSubscribersThenMembers(
			[
				{ subscriberType: 'user', subscriberId: 'watcher-1' },
				{ subscriberType: 'user', subscriberId: 'actor-1' },
			],
			['watcher-1', 'actor-1'],
			1
		);

		const { relation, notified } = await createIssueRelation('issue-1', 'issue-2', 'linked_to', 'user', 'actor-1');

		expect(relation).toEqual({ id: 'rel-1', sourceIssueId: 'issue-1', targetIssueId: 'issue-2' });
		expect(notified).toEqual([{ userId: 'watcher-1', kind: 'linked' }]);
	});
});
