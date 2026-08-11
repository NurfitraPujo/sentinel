import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8) -- co-located, shuffle-safe unit
 * tests for subscriptions.ts: `subscribe` is an idempotent upsert (never a plain insert that
 * would 23505 on a re-subscribe), `unsubscribe` deletes by the same 3-tuple, `listSubscribers`
 * fetches everything for an issue.
 */

function makeChainable() {
	const m: any = {};
	const methods = [
		'select',
		'from',
		'where',
		'insert',
		'values',
		'onConflictDoNothing',
		'delete',
	];
	for (const name of methods) {
		m[name] = vi.fn(() => m);
	}
	m.__result = [];
	m.then = vi.fn((resolve: any) => resolve(m.__result));
	return m;
}

const dbMock = makeChainable();

vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	issueSubscriptions: {
		id: 'id',
		issueId: 'issueId',
		subscriberType: 'subscriberType',
		subscriberId: 'subscriberId',
	},
}));

const { subscribe, unsubscribe, listSubscribers, isSubscribed } = await import('./subscriptions');

beforeEach(() => {
	vi.clearAllMocks();
	dbMock.__result = [];
});

describe('subscribe', () => {
	it('upserts via onConflictDoNothing targeting (issueId, subscriberType, subscriberId), never a plain insert', async () => {
		await subscribe({ issueId: 'issue-1', subscriberType: 'user', subscriberId: 'user-1', reason: 'reporter' });

		expect(dbMock.values).toHaveBeenCalledWith({
			issueId: 'issue-1',
			subscriberType: 'user',
			subscriberId: 'user-1',
			reason: 'reporter',
		});
		expect(dbMock.onConflictDoNothing).toHaveBeenCalledWith({
			target: ['issueId', 'subscriberType', 'subscriberId'],
		});
	});

	it('accepts an explicit tx in place of the module-level db', async () => {
		const tx = makeChainable();
		await subscribe({ issueId: 'issue-1', subscriberType: 'user', subscriberId: 'user-1', reason: 'manual' }, tx);

		expect(tx.insert).toHaveBeenCalled();
		expect(dbMock.insert).not.toHaveBeenCalled();
	});
});

describe('unsubscribe', () => {
	it('deletes by the (issueId, subscriberType, subscriberId) tuple', async () => {
		await unsubscribe({ issueId: 'issue-1', subscriberType: 'user', subscriberId: 'user-1' });

		expect(dbMock.delete).toHaveBeenCalled();
		expect(dbMock.where).toHaveBeenCalled();
	});
});

describe('listSubscribers', () => {
	it('returns every subscriber row for an issue', async () => {
		dbMock.__result = [
			{ subscriberType: 'user', subscriberId: 'user-1' },
			{ subscriberType: 'agent', subscriberId: 'agent-1' },
		];

		const result = await listSubscribers('issue-1');

		expect(result).toHaveLength(2);
	});
});

describe('isSubscribed', () => {
	it('returns true when a matching row exists', async () => {
		dbMock.__result = [{ id: 'sub-1' }];
		expect(await isSubscribed('issue-1', 'user', 'user-1')).toBe(true);
	});

	it('returns false when no row matches', async () => {
		dbMock.__result = [];
		expect(await isSubscribed('issue-1', 'user', 'user-1')).toBe(false);
	});
});
