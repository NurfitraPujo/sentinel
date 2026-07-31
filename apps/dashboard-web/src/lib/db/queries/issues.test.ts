import { describe, it, expect, vi, beforeEach } from 'vitest';

// A chainable Drizzle-query double, mirroring src/routes/api/alerts/alerts.test.ts's approach:
// every method returns the same object so `.update(x).set(y).where(z).returning(w)` and
// `.insert(x).values(y)` chains all resolve, and we can inspect what each method was called with.
//
// `__result` is what awaiting the chain yields. batchUpdateIssues awaits its UPDATE for the
// RETURNING rows, so a test can decide which ids the database "matched" — which is the whole point
// of the tenant-scoping assertion below.
function makeChainable() {
	const m: any = {};
	const methods = ['select', 'from', 'where', 'insert', 'values', 'returning', 'update', 'set', 'delete'];
	for (const name of methods) {
		m[name] = vi.fn(() => m);
	}
	m.__result = [];
	m.then = vi.fn((resolve: any) => resolve(m.__result));
	return m;
}

const txMock = makeChainable();
const dbMock: any = makeChainable();
// batchUpdateIssues runs entirely inside db.transaction(async (tx) => { ... }); everything the
// function does is against `tx`, not `db` directly, so the transaction stub must actually invoke
// the callback with txMock rather than just resolving.
dbMock.transaction = vi.fn(async (cb: any) => cb(txMock));

vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	issues: { id: 'id', projectId: 'projectId', status: 'status' },
	issueActivity: { id: 'id', issueId: 'issueId' },
	issueRelations: { id: 'id' },
}));

// Top-level await import, as alerts.test.ts does, to dodge vi.mock hoisting/TDZ.
const { batchUpdateIssues, MAX_BATCH_ISSUE_IDS } = await import('./issues');

beforeEach(() => {
	vi.clearAllMocks();
	txMock.__result = [];
});

describe('batchUpdateIssues', () => {
	// The defect this pins: the UPDATE is correctly scoped by projectId, but the activity insert used
	// to map over the caller's raw id list. An id belonging to a DIFFERENT project therefore got an
	// audit row appended to another tenant's issue while its status was, correctly, left alone —
	// a cross-tenant write driven entirely by request-body input (B7). Activity must follow the
	// UPDATE's RETURNING rows, never the request.
	it('writes activity only for issues the UPDATE actually matched, not for every id sent', async () => {
		// The caller names three ids; the database matches only two (the third is another project's).
		txMock.__result = [{ id: 'issue-a' }, { id: 'issue-b' }];

		const updatedCount = await batchUpdateIssues(
			'proj-1',
			'resolve',
			['issue-a', 'issue-b', 'other-tenants-issue'],
			{}
		);

		const insertedRows = txMock.values.mock.calls.at(-1)?.[0];
		expect(Array.isArray(insertedRows)).toBe(true);
		expect(insertedRows.map((r: any) => r.issueId)).toEqual(['issue-a', 'issue-b']);
		expect(insertedRows.map((r: any) => r.issueId)).not.toContain('other-tenants-issue');

		// And the reported count must be what changed, not what was asked for.
		expect(updatedCount).toBe(2);
	});

	it('inserts no activity at all when the UPDATE matched nothing', async () => {
		txMock.__result = [];

		const updatedCount = await batchUpdateIssues('proj-1', 'ignore', ['not-in-this-project'], {});

		expect(updatedCount).toBe(0);
		expect(txMock.insert).not.toHaveBeenCalled();
	});

	it('rejects a batch over MAX_BATCH_ISSUE_IDS before doing any database work', async () => {
		const overCap = Array.from({ length: MAX_BATCH_ISSUE_IDS + 1 }, (_, i) => `id-${i}`);

		await expect(batchUpdateIssues('proj-1', 'resolve', overCap, {})).rejects.toThrow(/Batch too large/);
		// No partial work: the cap is checked before the transaction opens.
		expect(dbMock.transaction).not.toHaveBeenCalled();
	});

	it('accepts a batch exactly at MAX_BATCH_ISSUE_IDS', async () => {
		const atCap = Array.from({ length: MAX_BATCH_ISSUE_IDS }, (_, i) => `id-${i}`);
		txMock.__result = atCap.map((id) => ({ id }));

		await expect(batchUpdateIssues('proj-1', 'resolve', atCap, {})).resolves.toBe(MAX_BATCH_ISSUE_IDS);
	});
});
