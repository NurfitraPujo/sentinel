import { describe, it, expect, vi, beforeEach } from 'vitest';
import { and, eq, gt, sql } from 'drizzle-orm';
import { EVENTS_LAG_GUARD_INTERVAL } from '$lib/server/agent-events';

/**
 * N1b (events feed) -- co-located unit tests for events.ts. Mirrors agent-work.test.ts's
 * chainable/queue-based db double.
 */

// N8: listOrgActivity now awaits TWO queries -- the issue_activity half, then the issue_tombstones
// half (queries/events.ts's listOrgTombstones). The chainable resolves the first await from
// `__result` (activity rows) and the second from `__tombstones` (tombstone rows), so existing
// single-source tests keep their exact meaning (tombstones default to []) while new tests can drive
// the deleted-issue path. The tombstone query is skipped entirely when an eventTypes filter
// excludes 'issue_deleted', in which case only the first await fires.
function makeChainable() {
	const m: any = {};
	const methods = ['select', 'from', 'innerJoin', 'where', 'orderBy', 'limit'];
	for (const name of methods) {
		m[name] = vi.fn(() => m);
	}
	m.__result = [];
	m.__tombstones = [];
	m.__awaitIndex = 0;
	m.then = vi.fn((resolve: any) => {
		const rows = m.__awaitIndex === 0 ? m.__result : m.__tombstones;
		m.__awaitIndex += 1;
		return resolve(rows);
	});
	return m;
}

const dbMock: any = makeChainable();
vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	issueActivity: {
		seq: 'seq',
		eventType: 'eventType',
		actorType: 'actorType',
		actorId: 'actorId',
		oldValue: 'oldValue',
		newValue: 'newValue',
		createdAt: 'createdAt',
		issueId: 'issueId',
	},
	issues: {
		id: 'id',
		message: 'message',
		status: 'status',
		issueType: 'issueType',
		projectId: 'projectId',
		assigneeType: 'assigneeType',
		assignedTo: 'assignedTo',
		claimedAt: 'claimedAt',
		waitingOn: 'waitingOn',
	},
	projects: { id: 'id', organizationId: 'organizationId' },
	issueTombstones: {
		seq: 'seq',
		reason: 'reason',
		deletedAt: 'deletedAt',
		issueId: 'issueId',
		issueMessage: 'issueMessage',
		issueType: 'issueType',
		projectId: 'projectId',
		organizationId: 'organizationId',
		assigneeType: 'assigneeType',
		assignedTo: 'assignedTo',
	},
}));

const { listOrgActivity } = await import('./events');
const { issueActivity, issues, projects } = await import('$lib/db/schema');

/**
 * Serializes a drizzle SQL/condition node into a comparable plain structure. `and(...)`,
 * `eq(...)`, `gt(...)` and tagged `sql` templates all build `SQL` instances whose payload lives
 * in a non-enumerable `queryChunks` array (or nested `.sql`), so a bare `toEqual` on the object
 * doesn't see it. Walking `.queryChunks`/`.value` recursively lets the tests assert on the
 * actual predicate shape (columns, operator, literal values) instead of only "where was called".
 */
function serializeSqlNode(node: unknown): unknown {
	if (node === null || typeof node !== 'object') return node;
	const anyNode = node as any;
	if (Array.isArray(anyNode.queryChunks)) {
		return anyNode.queryChunks.map(serializeSqlNode);
	}
	if ('value' in anyNode && Array.isArray(anyNode.value)) {
		return anyNode.value.map(serializeSqlNode);
	}
	if ('sql' in anyNode && typeof anyNode.sql === 'string') {
		return { sql: anyNode.sql };
	}
	return node;
}

function whereConditionsCalledWith() {
	const call = dbMock.where.mock.calls[0];
	const andArg = call[0];
	return serializeSqlNode(andArg);
}

function row(overrides: Partial<Record<string, unknown>> = {}) {
	return {
		seq: 1,
		eventType: 'status_changed',
		actorType: 'agent',
		actorId: 'agent-1',
		oldValue: null,
		newValue: { status: 'resolved' },
		createdAt: new Date('2026-08-14T00:00:00Z'),
		issueId: 'issue-1',
		issueMessage: 'Something broke',
		issueStatus: 'resolved',
		issueType: 'system_error',
		issueProjectId: 'project-1',
		issueAssigneeType: 'agent',
		issueAssignedTo: 'agent-1',
		issueClaimedAt: new Date('2026-08-14T00:00:00Z'),
		issueWaitingOn: 'reporter',
		...overrides,
	};
}

function tombstoneRow(overrides: Partial<Record<string, unknown>> = {}) {
	return {
		seq: 7,
		reason: 'retention',
		deletedAt: new Date('2026-08-15T00:00:00Z'),
		issueId: 'issue-gone',
		issueMessage: 'Deleted issue title',
		issueType: 'user_report',
		issueProjectId: 'project-1',
		...overrides,
	};
}

beforeEach(() => {
	vi.clearAllMocks();
	dbMock.__result = [];
	dbMock.__tombstones = [];
	dbMock.__awaitIndex = 0;
});

describe('listOrgActivity', () => {
	it('maps rows into the event shape with a nested issue', async () => {
		dbMock.__result = [row()];

		const result = await listOrgActivity({ organizationId: 'org-1', after: 0, limit: 50 });

		expect(result.events).toHaveLength(1);
		expect(result.events[0]).toEqual({
			seq: 1,
			eventType: 'status_changed',
			actorType: 'agent',
			actorId: 'agent-1',
			oldValue: null,
			newValue: { status: 'resolved' },
			createdAt: new Date('2026-08-14T00:00:00Z'),
			issue: {
				id: 'issue-1',
				title: 'Something broke',
				status: 'resolved',
				issueType: 'system_error',
				projectId: 'project-1',
				assigneeType: 'agent',
				assignedTo: 'agent-1',
				claimedAt: new Date('2026-08-14T00:00:00Z'),
				waitingOn: 'reporter',
			},
		});
	});

	it('advances cursor to the highest seq returned', async () => {
		dbMock.__result = [row({ seq: 5 }), row({ seq: 9 })];

		const result = await listOrgActivity({ organizationId: 'org-1', after: 0, limit: 50 });

		expect(result.cursor).toBe(9);
	});

	it('leaves cursor at `after` when no rows are returned', async () => {
		dbMock.__result = [];

		const result = await listOrgActivity({ organizationId: 'org-1', after: 42, limit: 50 });

		expect(result.cursor).toBe(42);
		expect(result.events).toEqual([]);
	});

	it('computes hasMore by fetching limit+1 and slicing the extra row off', async () => {
		dbMock.__result = [row({ seq: 1 }), row({ seq: 2 }), row({ seq: 3 })];

		const result = await listOrgActivity({ organizationId: 'org-1', after: 0, limit: 2 });

		expect(result.events).toHaveLength(2);
		expect(result.events.map((e) => e.seq)).toEqual([1, 2]);
		expect(result.hasMore).toBe(true);
		expect(dbMock.limit).toHaveBeenCalledWith(3);
	});

	it('hasMore is false when fewer than limit+1 rows come back', async () => {
		dbMock.__result = [row({ seq: 1 })];

		const result = await listOrgActivity({ organizationId: 'org-1', after: 0, limit: 50 });

		expect(result.hasMore).toBe(false);
	});

	it('scopes to the given organizationId via the projects join condition', async () => {
		dbMock.__result = [];

		await listOrgActivity({ organizationId: 'org-1', after: 0, limit: 50 });

		// Two WHEREs now: the issue_activity half (call[0], asserted below) and the tombstone half.
		expect(dbMock.where).toHaveBeenCalledTimes(2);

		const expected = serializeSqlNode(
			and(
				eq(projects.organizationId, 'org-1'),
				gt(issueActivity.seq, 0),
				sql`${issueActivity.createdAt} < now() - interval '${sql.raw(EVENTS_LAG_GUARD_INTERVAL)}'`
			)
		);
		expect(whereConditionsCalledWith()).toEqual(expected);
	});

	it('applies the seq cursor and the lag-guard interval, not just organizationId', async () => {
		dbMock.__result = [];

		await listOrgActivity({ organizationId: 'org-1', after: 41, limit: 50 });

		const serialized = JSON.stringify(whereConditionsCalledWith());
		// The seq cursor: deleting `gt(issueActivity.seq, options.after)` from events.ts must
		// fail this assertion -- the literal cursor value has to appear in the WHERE.
		expect(serialized).toContain('41');
		// The lag guard: deleting the `createdAt < now() - interval ...` condition must fail
		// this assertion -- the configured interval literal has to appear in the WHERE.
		expect(serialized).toContain(EVENTS_LAG_GUARD_INTERVAL);
		expect(serialized).toContain('now()');
	});

	it('applies eventTypes, projectId and claimedByAgentId filters without throwing', async () => {
		dbMock.__result = [];

		await listOrgActivity({
			organizationId: 'org-1',
			after: 0,
			limit: 50,
			eventTypes: ['status_changed', 'claimed'],
			projectId: 'project-1',
			claimedByAgentId: 'agent-1',
		});

		expect(dbMock.where).toHaveBeenCalledTimes(1);
	});

	// N8 (DECISIONS.md D20): the tombstone half of the feed.
	it('surfaces a tombstone as an issue_deleted event merged into seq order', async () => {
		dbMock.__result = [row({ seq: 3 }), row({ seq: 9 })];
		dbMock.__tombstones = [tombstoneRow({ seq: 5 })];

		const result = await listOrgActivity({ organizationId: 'org-1', after: 0, limit: 50 });

		expect(result.events.map((e) => e.seq)).toEqual([3, 5, 9]);
		const deleted = result.events.find((e) => e.seq === 5)!;
		expect(deleted).toEqual({
			seq: 5,
			eventType: 'issue_deleted',
			actorType: 'system',
			actorId: 'sentinel-retention',
			oldValue: null,
			newValue: { reason: 'retention', deletedAt: new Date('2026-08-15T00:00:00Z') },
			createdAt: new Date('2026-08-15T00:00:00Z'),
			issue: {
				id: 'issue-gone',
				title: 'Deleted issue title',
				status: 'deleted',
				issueType: 'user_report',
				projectId: 'project-1',
			},
		});
		// cursor advances to the highest merged seq.
		expect(result.cursor).toBe(9);
	});

	it('re-applies hasMore/cursor over the merged set, slicing across both sources', async () => {
		dbMock.__result = [row({ seq: 1 }), row({ seq: 4 })];
		dbMock.__tombstones = [tombstoneRow({ seq: 2 }), tombstoneRow({ seq: 3 })];

		const result = await listOrgActivity({ organizationId: 'org-1', after: 0, limit: 2 });

		// Combined ascending order is [1,2,3,4]; limit 2 keeps [1,2], and the merge is what makes
		// hasMore true even though neither source alone exceeded the limit.
		expect(result.events.map((e) => e.seq)).toEqual([1, 2]);
		expect(result.hasMore).toBe(true);
		expect(result.cursor).toBe(2);
	});

	it('skips the tombstone source entirely when eventTypes excludes issue_deleted', async () => {
		dbMock.__result = [row({ seq: 1 })];
		dbMock.__tombstones = [tombstoneRow({ seq: 2 })];

		const result = await listOrgActivity({
			organizationId: 'org-1',
			after: 0,
			limit: 50,
			eventTypes: ['status_changed'],
		});

		// Only one query fired (the activity half); the tombstone row must not appear.
		expect(dbMock.where).toHaveBeenCalledTimes(1);
		expect(result.events.map((e) => e.seq)).toEqual([1]);
	});

	it('includes the tombstone source when eventTypes explicitly requests issue_deleted', async () => {
		dbMock.__result = [];
		dbMock.__tombstones = [tombstoneRow({ seq: 8 })];

		const result = await listOrgActivity({
			organizationId: 'org-1',
			after: 0,
			limit: 50,
			eventTypes: ['issue_deleted'],
		});

		expect(dbMock.where).toHaveBeenCalledTimes(2);
		expect(result.events.map((e) => e.eventType)).toEqual(['issue_deleted']);
	});
});
