import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * R7 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): `updateIssueStatus` never cleared
 * `issues.waiting_on`, so a resolved/ignored issue that had an outstanding agent question stayed
 * stuck in the "Needs input" tab forever. This is a dedicated, isolated double (rather than
 * reusing issues.notify.test.ts's shared chainable mock) because `notifyIssueEvent` is mocked
 * away entirely here -- this file only cares about the `updateData` passed to `tx.update`, not
 * the notification fan-out.
 */

const notifyIssueEvent = vi.fn(async () => []);
vi.mock('$lib/server/notify', () => ({ notifyIssueEvent }));
vi.mock('$lib/db/queries/subscriptions', () => ({ subscribe: vi.fn() }));

vi.mock('$lib/db/schema', () => ({
	issues: { id: 'id', projectId: 'projectId', status: 'status', waitingOn: 'waitingOn' },
	issueActivity: { id: 'id', issueId: 'issueId' },
	issueRelations: {},
	projects: { id: 'id', organizationId: 'organizationId' },
}));

function makeTx() {
	const selectQueue: unknown[][] = [];
	const updateCalls: { set: any }[] = [];
	const tx: any = {
		select: vi.fn(() => ({
			from: vi.fn(() => ({ where: vi.fn(() => Promise.resolve(selectQueue.shift() ?? [])) })),
		})),
		update: vi.fn(() => ({
			set: vi.fn((values: any) => {
				updateCalls.push({ set: values });
				return { where: vi.fn(() => Promise.resolve(undefined)) };
			}),
		})),
		insert: vi.fn(() => ({ values: vi.fn(() => Promise.resolve(undefined)) })),
	};
	return { tx, selectQueue, updateCalls };
}

let txHandle = makeTx();
const dbTransaction = vi.fn(async (cb: any) => cb(txHandle.tx));
vi.mock('$lib/server/db', () => ({ db: { transaction: (cb: any) => dbTransaction(cb) } }));

const { updateIssueStatus } = await import('./issues');

beforeEach(() => {
	vi.clearAllMocks();
	txHandle = makeTx();
});

describe('updateIssueStatus clears waitingOn (R7)', () => {
	it('clears waitingOn when the new status is resolved', async () => {
		txHandle.selectQueue.push([{ status: 'unresolved', waitingOn: 'reporter' }]);

		await updateIssueStatus('issue-1', 'resolved', undefined, 'user', 'user-1');

		expect(txHandle.updateCalls[0].set).toMatchObject({ status: 'resolved', waitingOn: null });
	});

	it('clears waitingOn when the new status is ignored', async () => {
		txHandle.selectQueue.push([{ status: 'unresolved', waitingOn: 'team' }]);

		await updateIssueStatus('issue-1', 'ignored', undefined, 'user', 'user-1');

		expect(txHandle.updateCalls[0].set).toMatchObject({ status: 'ignored', waitingOn: null });
	});

	it('does not touch waitingOn when it was already null', async () => {
		txHandle.selectQueue.push([{ status: 'unresolved', waitingOn: null }]);

		await updateIssueStatus('issue-1', 'resolved', undefined, 'user', 'user-1');

		expect(txHandle.updateCalls[0].set).not.toHaveProperty('waitingOn');
	});

	it('does not touch waitingOn on a transition back to unresolved', async () => {
		txHandle.selectQueue.push([{ status: 'resolved', waitingOn: 'reporter' }]);

		await updateIssueStatus('issue-1', 'unresolved', undefined, 'user', 'user-1');

		expect(txHandle.updateCalls[0].set).not.toHaveProperty('waitingOn');
	});
});
