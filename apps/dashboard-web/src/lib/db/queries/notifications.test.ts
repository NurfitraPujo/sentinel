import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8) -- co-located, shuffle-safe unit
 * tests for the notifications read-side query module.
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
		'offset',
		'update',
		'set',
		'returning',
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
	notifications: {
		id: 'id',
		userId: 'userId',
		issueId: 'issueId',
		kind: 'kind',
		actorType: 'actorType',
		actorId: 'actorId',
		payload: 'payload',
		readAt: 'readAt',
		createdAt: 'createdAt',
	},
	issues: { id: 'id', message: 'message', issueType: 'issueType', projectId: 'projectId' },
	projects: { id: 'id', organizationId: 'organizationId' },
	organizations: { id: 'id', slug: 'slug' },
	users: { id: 'id', name: 'name' },
}));

const {
	listNotifications,
	getUnreadNotificationCount,
	getNotificationCount,
	markNotificationRead,
	markAllNotificationsRead,
} = await import('./notifications');

beforeEach(() => {
	vi.clearAllMocks();
	dbMock.__result = [];
});

describe('listNotifications', () => {
	it('clamps limit to MAX_PAGE_SIZE (100)', async () => {
		await listNotifications({ userId: 'user-1', limit: 500 });
		expect(dbMock.limit).toHaveBeenCalledWith(100);
	});

	it('defaults limit to 25 when unset', async () => {
		await listNotifications({ userId: 'user-1' });
		expect(dbMock.limit).toHaveBeenCalledWith(25);
	});

	it('floors a negative offset to 0', async () => {
		await listNotifications({ userId: 'user-1', offset: -5 });
		expect(dbMock.offset).toHaveBeenCalledWith(0);
	});
});

describe('getUnreadNotificationCount', () => {
	it('returns the count column', async () => {
		dbMock.__result = [{ count: 3 }];
		expect(await getUnreadNotificationCount('user-1')).toBe(3);
	});

	it('returns 0 when no row comes back', async () => {
		dbMock.__result = [];
		expect(await getUnreadNotificationCount('user-1')).toBe(0);
	});
});

describe('getNotificationCount', () => {
	it('returns the count column', async () => {
		dbMock.__result = [{ count: 7 }];
		expect(await getNotificationCount('user-1')).toBe(7);
	});

	it('returns 0 when no row comes back', async () => {
		dbMock.__result = [];
		expect(await getNotificationCount('user-1')).toBe(0);
	});
});

describe('markNotificationRead', () => {
	it('returns true when a row was updated', async () => {
		dbMock.returning = vi.fn(() => Promise.resolve([{ id: 'notif-1' }]));
		expect(await markNotificationRead('notif-1', 'user-1')).toBe(true);
	});

	it('returns false when no row matched (wrong owner or missing id)', async () => {
		dbMock.returning = vi.fn(() => Promise.resolve([]));
		expect(await markNotificationRead('notif-1', 'user-2')).toBe(false);
	});
});

describe('markAllNotificationsRead', () => {
	it('returns the number of rows updated', async () => {
		dbMock.returning = vi.fn(() => Promise.resolve([{ id: 'a' }, { id: 'b' }]));
		expect(await markAllNotificationsRead('user-1')).toBe(2);
	});
});
