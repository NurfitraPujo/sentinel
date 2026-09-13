import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * R6 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): the retention cron's orphaned-issues delete
 * cascades attachments rows away (FK ON DELETE CASCADE) without ever touching the underlying
 * MinIO objects. This proves `cleanupRetainedData` collects storage_keys BEFORE the delete --
 * for attachments linked directly to a doomed issue AND for attachments linked to one of that
 * issue's comments -- and best-effort deletes both sets from storage after the delete.
 *
 * Queue-based `db` double, same shape as comments.test.ts's `makeQueueableDb` -- retention.ts's
 * reads/writes are plain (non-transactional) calls issued in a fixed sequence. The one exception
 * is sentinel-affected-users §4.5's side-table step (two `db.transaction`s, three raw
 * `tx.execute`s each, between the occurrence delete and the candidate select): it is stubbed here
 * as a pass-through whose every `execute` resolves to no rows, so it neither consumes the queue nor
 * proves anything -- `retention.affected.integration.test.ts` proves it against real Postgres.
 */

const deleteObject = vi.fn();
const isStorageConfigured = vi.fn(() => true);
const logError = vi.fn();

vi.mock('$lib/server/storage', () => ({ deleteObject, isStorageConfigured }));
vi.mock('$lib/server/observability/log', () => ({ log: { error: logError } }));

vi.mock('$lib/db/schema', () => ({
	errorOccurrences: { id: 'id', issueId: 'issueId', createdAt: 'createdAt' },
	issues: { id: 'id', firstSeen: 'firstSeen', projectId: 'projectId', message: 'message', issueType: 'issueType', assigneeType: 'assigneeType', assignedTo: 'assignedTo', affectedUserCount: { name: 'affected_user_count' }, affectedTenantCount: { name: 'affected_tenant_count' } },
	// sentinel-affected-users §4.5: the two side tables the sweep's transactional step reads and
	// deletes from (raw SQL, so only their identity matters to the mock).
	issueAffectedUsers: { issueId: 'issueId', lastSeen: 'lastSeen' },
	issueAffectedTenants: { issueId: 'issueId', lastSeen: 'lastSeen' },
	// sentinel-issue-trend §4.5: the bucket rollup the sweep deletes from with one raw `db.execute`
	// (again only its identity matters to the mock).
	issueOccurrenceBuckets: { issueId: 'issueId', bucketStart: 'bucketStart' },
	issueComments: { id: 'id', issueId: 'issueId' },
	attachments: { id: 'id', issueId: 'issueId', commentId: 'commentId', storageKey: 'storageKey' },
	// N8: retention now also joins projects (for organization_id) and writes/prunes issueTombstones.
	projects: { id: 'id', organizationId: 'organizationId' },
	issueTombstones: { id: 'id', deletedAt: 'deletedAt' },
	// sentinel-user-report-issue §6: the sweep also reaps `external_reporters` rows that no longer
	// have a report -- nothing cascades to them from `issues`.
	externalIssueReports: { issueId: 'issueId', externalReporterId: 'externalReporterId' },
	externalReporters: { id: 'id', lastSeenAt: 'lastSeenAt' },
	agentIdempotencyKeys: { id: 'id', createdAt: 'createdAt' },
	reportIdempotencyKeys: { id: 'id', createdAt: 'createdAt' },
	issueActivity: { id: 'id', issueId: 'issueId' },
}));

function makeQueueableDb() {
	const resultQueue: unknown[] = [];
	const chain: any = {};
	const methods = ['select', 'from', 'where', 'delete', 'innerJoin', 'insert', 'values'];
	for (const m of methods) {
		chain[m] = vi.fn(() => chain);
	}
	chain.returning = vi.fn(() => Promise.resolve(resultQueue.shift() ?? []));
	chain.then = vi.fn((resolve: any) => resolve(resultQueue.shift() ?? []));
	// sentinel-affected-users §4.5: the side-table step runs inside `db.transaction` and issues raw
	// `tx.execute` statements; a pass-through transaction with an always-empty execute keeps it off
	// the result queue (the sequence below is the autocommit calls only).
	const tx = { execute: vi.fn(() => Promise.resolve([])) };
	chain.transaction = vi.fn((callback: (tx: unknown) => unknown) => callback(tx));
	// sentinel-issue-trend §4.5: the bucket delete is one raw top-level `db.execute` between the
	// side-table transactions and the candidate select; always-empty here for the same reason
	// (retention.buckets.integration.test.ts proves it against real Postgres).
	chain.execute = vi.fn(() => Promise.resolve([]));
	return { db: chain, resultQueue };
}

const { db: dbMock, resultQueue } = makeQueueableDb();
vi.mock('$lib/server/db', () => ({ db: dbMock }));

const { cleanupRetainedData } = await import('./retention');

beforeEach(() => {
	vi.clearAllMocks();
	isStorageConfigured.mockReturnValue(true);
	resultQueue.length = 0;
});

describe('cleanupRetainedData attachment cleanup (R6)', () => {
	it('deletes storage objects for both issue-attached and comment-attached files on retention delete', async () => {
		// Call sequence inside cleanupRetainedData (the two §4.5 side-table transactions and the
		// bucket delete's `execute` between 1 and 2 go through the pass-through `transaction` /
		// `execute` stubs and never touch this queue):
		// 1) delete errorOccurrences .returning()
		// 2) select candidate issues (+innerJoin projects) .where() (thenable)
		// 3) select comment ids for those issues .where() (thenable)
		// 4) select attachment storageKeys .where() (thenable)
		// 5) delete issues .returning()
		// 6) insert issueTombstones .values() (thenable) -- N8, for the actually-deleted issue
		// 7) delete stale issueTombstones .returning() -- N8 prune
		resultQueue.push([]); // 1: deleted occurrences
		resultQueue.push([{ id: 'issue-1', projectId: 'p1', organizationId: 'o1', message: 'm', issueType: 'system_error', assigneeType: null, assignedTo: null }]); // 2: candidate rows
		resultQueue.push([{ id: 'comment-1' }]); // 3: comment ids under issue-1
		resultQueue.push([{ storageKey: 'org/1/issue-file' }, { storageKey: 'org/1/comment-file' }]); // 4
		resultQueue.push([{ id: 'issue-1' }]); // 5: deleted issues
		resultQueue.push([]); // 6: tombstone insert result (unused)
		resultQueue.push([]); // 7: pruned tombstones

		const result = await cleanupRetainedData(30);

		expect(result.deletedOrphanedIssues).toBe(1);
		expect(deleteObject).toHaveBeenCalledWith('org/1/issue-file');
		expect(deleteObject).toHaveBeenCalledWith('org/1/comment-file');
		expect(deleteObject).toHaveBeenCalledTimes(2);
	});

	it('does not touch storage when there are no orphaned candidates', async () => {
		resultQueue.push([]); // deleted occurrences
		resultQueue.push([]); // no candidate issue ids (skips comment/attachment selects)
		resultQueue.push([]); // deleted issues (delete's WHERE re-evaluates, still empty)
		resultQueue.push([]); // pruned tombstones (N8; no insert since nothing was deleted)

		await cleanupRetainedData(30);

		expect(deleteObject).not.toHaveBeenCalled();
	});
});
