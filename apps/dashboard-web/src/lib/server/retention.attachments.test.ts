import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * R6 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): the retention cron's orphaned-issues delete
 * cascades attachments rows away (FK ON DELETE CASCADE) without ever touching the underlying
 * MinIO objects. This proves `cleanupRetainedData` collects storage_keys BEFORE the delete --
 * for attachments linked directly to a doomed issue AND for attachments linked to one of that
 * issue's comments -- and best-effort deletes both sets from storage after the delete.
 *
 * Queue-based `db` double, same shape as comments.test.ts's `makeQueueableDb` -- retention.ts's
 * reads/writes are plain (non-transactional) calls issued in a fixed sequence.
 */

const deleteObject = vi.fn();
const isStorageConfigured = vi.fn(() => true);
const logError = vi.fn();

vi.mock('$lib/server/storage', () => ({ deleteObject, isStorageConfigured }));
vi.mock('$lib/server/observability/log', () => ({ log: { error: logError } }));

vi.mock('$lib/db/schema', () => ({
	errorOccurrences: { id: 'id', issueId: 'issueId', createdAt: 'createdAt' },
	issues: { id: 'id', firstSeen: 'firstSeen' },
	issueComments: { id: 'id', issueId: 'issueId' },
	attachments: { id: 'id', issueId: 'issueId', commentId: 'commentId', storageKey: 'storageKey' },
}));

function makeQueueableDb() {
	const resultQueue: unknown[] = [];
	const chain: any = {};
	const methods = ['select', 'from', 'where', 'delete'];
	for (const m of methods) {
		chain[m] = vi.fn(() => chain);
	}
	chain.returning = vi.fn(() => Promise.resolve(resultQueue.shift() ?? []));
	chain.then = vi.fn((resolve: any) => resolve(resultQueue.shift() ?? []));
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
		// Call sequence inside cleanupRetainedData:
		// 1) delete errorOccurrences .returning()
		// 2) select candidate issue ids .where() (thenable)
		// 3) select comment ids for those issues .where() (thenable)
		// 4) select attachment storageKeys .where() (thenable)
		// 5) delete issues .returning()
		resultQueue.push([]); // 1: deleted occurrences
		resultQueue.push([{ id: 'issue-1' }]); // 2: candidate issue ids
		resultQueue.push([{ id: 'comment-1' }]); // 3: comment ids under issue-1
		resultQueue.push([{ storageKey: 'org/1/issue-file' }, { storageKey: 'org/1/comment-file' }]); // 4
		resultQueue.push([{ id: 'issue-1' }]); // 5: deleted issues

		const result = await cleanupRetainedData(30);

		expect(result.deletedOrphanedIssues).toBe(1);
		expect(deleteObject).toHaveBeenCalledWith('org/1/issue-file');
		expect(deleteObject).toHaveBeenCalledWith('org/1/comment-file');
		expect(deleteObject).toHaveBeenCalledTimes(2);
	});

	it('does not touch storage when there are no orphaned candidates', async () => {
		resultQueue.push([]); // deleted occurrences
		resultQueue.push([]); // no candidate issue ids
		resultQueue.push([]); // deleted issues (delete's WHERE re-evaluates, still empty)

		await cleanupRetainedData(30);

		expect(deleteObject).not.toHaveBeenCalled();
	});
});
