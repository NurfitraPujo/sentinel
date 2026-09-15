import { describe, it, expect, vi, beforeEach } from 'vitest';
import { PgDialect } from 'drizzle-orm/pg-core';
import { sql } from 'drizzle-orm';

/**
 * sentinel-sourcemaps §4.1 / decision 26 -- the `sourcemaps` write path -- and §4.2 / §4.6's two
 * reads of the table.
 *
 * What is load-bearing on the write path: the upsert conflicts on §4.5's unique key `(project_id,
 * release_version, minified_path)` and rewrites exactly the four replace columns; the per-release
 * count is re-read INSIDE the transaction under a per-release lock and a lost race is a thrown
 * `ReleaseFullError` (D18: thrown, never returned) with nothing inserted; phase 1's read is
 * two plain selects with no transaction open; and the keys reported as displaced are the ones
 * the rows carried INSIDE the transaction, with `findReferencedStorageKeys` as the post-commit
 * guard before any delete (BUG-05).
 *
 * On the reads: `listReleaseSourcemaps` selects §4.2's five columns and NEVER `storage_key`, and
 * orders `minified_path ASC`; `countSourcemapsByRelease` is ONE grouped statement for the page's
 * versions on `(project_id, release_version)` -- the same shape `listProjectReleases` runs for
 * `issuesIntroduced` -- and builds no statement at all for an empty version list.
 */

interface Captured {
	values: unknown[];
	conflicts: unknown[];
	executes: unknown[];
	selectFields: unknown[];
	selectWheres: unknown[];
	orderBys: unknown[];
	groupBys: unknown[];
}

const captured: Captured = {
	values: [],
	conflicts: [],
	executes: [],
	selectFields: [],
	selectWheres: [],
	orderBys: [],
	groupBys: [],
};

/** Queued results for each `select().from().where()[.orderBy()|.groupBy()]` chain, in call order. */
let selectResults: unknown[][] = [];

function makeExecutor() {
	const m: any = {};
	m.select = vi.fn((fields: unknown) => {
		captured.selectFields.push(fields);
		return m;
	});
	m.from = vi.fn(() => m);
	m.where = vi.fn((w: unknown) => {
		captured.selectWheres.push(w);
		const next = selectResults.shift() ?? [];
		// The tail of a select: awaitable as-is, or first ordered / grouped (§4.2 / §4.6's reads).
		const tail: any = { then: (resolve: any) => resolve(next) };
		tail.orderBy = vi.fn((o: unknown) => {
			captured.orderBys.push(o);
			return tail;
		});
		tail.groupBy = vi.fn((g: unknown) => {
			captured.groupBys.push(g);
			return tail;
		});
		return tail;
	});
	m.insert = vi.fn(() => m);
	m.values = vi.fn((v: unknown) => {
		captured.values.push(v);
		return m;
	});
	m.onConflictDoUpdate = vi.fn((c: unknown) => {
		captured.conflicts.push(c);
		return { then: (resolve: any) => resolve(undefined) };
	});
	m.execute = vi.fn(async (q: unknown) => {
		captured.executes.push(q);
		return [];
	});
	return m;
}

const dbMock: any = makeExecutor();
dbMock.transaction = vi.fn(async (cb: (tx: unknown) => Promise<unknown>) => cb(dbMock));

vi.mock('$lib/server/db', () => ({ db: dbMock }));

const {
	readReleaseState,
	upsertReleaseSourcemaps,
	findReferencedStorageKeys,
	ReleaseFullError,
	listReleaseSourcemaps,
	countSourcemapsByRelease,
} = await import('./sourcemaps');

const dialect = new PgDialect();
/** A recorded fragment -- a predicate, an ordering, a column -- as the SQL text Postgres would get. */
function render(value: unknown): string {
	const q = dialect.sqlToQuery(sql`${value as any}`);
	return `${q.sql} -- ${q.params.map(String).join(', ')}`;
}

beforeEach(() => {
	vi.clearAllMocks();
	captured.values = [];
	captured.conflicts = [];
	captured.executes = [];
	captured.selectFields = [];
	captured.selectWheres = [];
	captured.orderBys = [];
	captured.groupBys = [];
	selectResults = [];
});

const FILE_A = { minifiedPath: '/a.js', storageKey: 'sourcemaps/p/r/a-1.map', sizeBytes: 10, sha256: '1'.repeat(64) };
const FILE_B = { minifiedPath: '/b.js', storageKey: 'sourcemaps/p/r/b-2.map', sizeBytes: 20, sha256: '2'.repeat(64) };
/** The cases that are not about decision 43 hand the transaction a check that always passes. */
const present = async () => {};

describe('readReleaseState (phase 1)', () => {
	it('returns the release count and the existing rows keyed by normalised path, with no transaction', async () => {
		selectResults = [[{ count: 3 }], [{ minifiedPath: '/a.js', sha256: 'aa', storageKey: 'k-a' }]];
		const state = await readReleaseState('proj-1', 'v1', ['/a.js', '/b.js']);
		expect(state.current).toBe(3);
		expect(state.existing.get('/a.js')).toEqual({ sha256: 'aa', storageKey: 'k-a' });
		expect(state.existing.has('/b.js')).toBe(false);
		expect(dbMock.transaction).not.toHaveBeenCalled();
	});

	it('scopes both reads to (project_id, release_version) -- never the paths alone (tenancy)', async () => {
		selectResults = [[{ count: 0 }], []];
		await readReleaseState('proj-1', 'v1', ['/a.js']);
		expect(captured.selectWheres).toHaveLength(2);
		for (const where of captured.selectWheres) {
			const rendered = render(where);
			expect(rendered).toContain('"project_id" = ');
			expect(rendered).toContain('"release_version" = ');
			expect(rendered).toContain('proj-1, v1');
		}
		expect(render(captured.selectWheres[1])).toContain('"minified_path" in ');
	});

	it('skips the row read entirely when there are no paths', async () => {
		selectResults = [[{ count: 7 }]];
		const state = await readReleaseState('proj-1', 'v1', []);
		expect(state.current).toBe(7);
		expect(captured.selectWheres).toHaveLength(1);
	});
});

describe('upsertReleaseSourcemaps (phase 3)', () => {
	it('runs inside ONE transaction: lock, count, presence read, then the upserts', async () => {
		selectResults = [[{ count: 1 }], [{ minifiedPath: '/a.js', storageKey: 'sourcemaps/p/r/a-0.map' }]];
		const out = await upsertReleaseSourcemaps({
			projectId: 'proj-1',
			releaseVersion: 'v1',
			files: [FILE_A, FILE_B],
			maxPerRelease: 500,
			ensureObjectsPresent: present,
		});
		expect(dbMock.transaction).toHaveBeenCalledTimes(1);
		expect(captured.executes).toHaveLength(1);
		expect(render(captured.executes[0])).toContain('pg_advisory_xact_lock(hashtextextended(');
		expect(render(captured.executes[0])).toContain('sourcemaps:proj-1:v1');
		// /a.js already present (a replace), /b.js new: 1 + 1 = 2; the replace displaced /a.js's
		// previous key, read under the lock.
		expect(out).toEqual({ releaseFileCount: 2, displacedKeys: ['sourcemaps/p/r/a-0.map'] });
		expect(captured.values).toHaveLength(2);
	});

	it('BUG-05: displacedKeys is what the row pointed at INSIDE the transaction -- the presence read selects storage_key', async () => {
		selectResults = [[{ count: 1 }], [{ minifiedPath: '/a.js', storageKey: 'moved-since-phase-1' }]];
		const out = await upsertReleaseSourcemaps({ projectId: 'proj-1', releaseVersion: 'v1', files: [FILE_A], maxPerRelease: 500, ensureObjectsPresent: present });
		expect(out.displacedKeys).toEqual(['moved-since-phase-1']);
		// The presence read asks for both columns; a read that dropped storage_key would silently
		// report nothing displaced.
		expect(Object.keys(captured.selectFields[1] as object).sort()).toEqual(['minifiedPath', 'storageKey']);
	});

	it('BUG-05: a row rewritten with the key it already has displaces nothing', async () => {
		selectResults = [[{ count: 1 }], [{ minifiedPath: '/a.js', storageKey: FILE_A.storageKey }]];
		const out = await upsertReleaseSourcemaps({ projectId: 'proj-1', releaseVersion: 'v1', files: [FILE_A], maxPerRelease: 500, ensureObjectsPresent: present });
		expect(out.displacedKeys).toEqual([]);
	});

	it('conflicts on (project_id, release_version, minified_path) and rewrites exactly the four replace columns', async () => {
		selectResults = [[{ count: 0 }], []];
		await upsertReleaseSourcemaps({ projectId: 'proj-1', releaseVersion: 'v1', files: [FILE_A], maxPerRelease: 500, ensureObjectsPresent: present });
		expect(captured.values[0]).toEqual({
			projectId: 'proj-1',
			releaseVersion: 'v1',
			minifiedPath: '/a.js',
			storageKey: 'sourcemaps/p/r/a-1.map',
			sizeBytes: 10,
			sha256: '1'.repeat(64),
		});
		const conflict: any = captured.conflicts[0];
		expect(conflict.target.map((c: any) => c.name)).toEqual(['project_id', 'release_version', 'minified_path']);
		expect(Object.keys(conflict.set).sort()).toEqual(['sha256', 'sizeBytes', 'storageKey', 'uploadedAt']);
		expect(conflict.set.storageKey).toBe('sourcemaps/p/r/a-1.map');
		expect(render(conflict.set.uploadedAt)).toContain('now()');
	});

	it('throws ReleaseFullError with the re-read count when a race pushed the release past the cap, inserting nothing', async () => {
		// The route's pre-check saw 498; a concurrent upload landed two rows since.
		selectResults = [[{ count: 500 }], []];
		await expect(
			upsertReleaseSourcemaps({ projectId: 'proj-1', releaseVersion: 'v1', files: [FILE_A], maxPerRelease: 500, ensureObjectsPresent: present })
		).rejects.toMatchObject({ name: 'ReleaseFullError', max: 500, current: 500 });
		expect(captured.values).toHaveLength(0);
		expect(dbMock.insert).not.toHaveBeenCalled();
	});

	it('does not count a replace as an add: a full release still accepts a re-upload of its own paths', async () => {
		selectResults = [[{ count: 500 }], [{ minifiedPath: '/a.js', storageKey: 'k-old' }]];
		const out = await upsertReleaseSourcemaps({
			projectId: 'proj-1',
			releaseVersion: 'v1',
			files: [FILE_A],
			maxPerRelease: 500,
			ensureObjectsPresent: present,
		});
		expect(out).toEqual({ releaseFileCount: 500, displacedKeys: ['k-old'] });
		expect(captured.values).toHaveLength(1);
	});

	it('decision 43 (BUG-07): ensureObjectsPresent is called once with every storageKey, after the lock and the cap check and before the first insert', async () => {
		selectResults = [[{ count: 1 }], [{ minifiedPath: '/a.js', storageKey: 'sourcemaps/p/r/a-0.map' }]];
		const ensureObjectsPresent = vi.fn(async () => {
			// At the moment of the check the lock statement has run and nothing has been inserted.
			expect(captured.executes).toHaveLength(1);
			expect(captured.values).toHaveLength(0);
		});
		await upsertReleaseSourcemaps({ projectId: 'proj-1', releaseVersion: 'v1', files: [FILE_A, FILE_B], maxPerRelease: 500, ensureObjectsPresent });
		expect(ensureObjectsPresent).toHaveBeenCalledTimes(1);
		expect(ensureObjectsPresent).toHaveBeenCalledWith([FILE_A.storageKey, FILE_B.storageKey]);
		expect(captured.values).toHaveLength(2);
	});

	it('decision 43 (BUG-07): a check that throws propagates out of the transaction with nothing inserted (D18: thrown, rolled back)', async () => {
		selectResults = [[{ count: 0 }], []];
		const boom = new Error('object gone and the re-put failed');
		await expect(
			upsertReleaseSourcemaps({ projectId: 'proj-1', releaseVersion: 'v1', files: [FILE_A], maxPerRelease: 500, ensureObjectsPresent: async () => { throw boom; } })
		).rejects.toBe(boom);
		expect(dbMock.insert).not.toHaveBeenCalled();
	});

	it('decision 43 (BUG-07): the check is not made when the cap refuses -- nothing will be committed to those keys', async () => {
		selectResults = [[{ count: 500 }], []];
		const ensureObjectsPresent = vi.fn(async () => {});
		await expect(
			upsertReleaseSourcemaps({ projectId: 'proj-1', releaseVersion: 'v1', files: [FILE_A], maxPerRelease: 500, ensureObjectsPresent })
		).rejects.toBeInstanceOf(ReleaseFullError);
		expect(ensureObjectsPresent).not.toHaveBeenCalled();
	});

	it('exports ReleaseFullError as a thrown Error carrying max and current (D18)', () => {
		const err = new ReleaseFullError(500, 499);
		expect(err).toBeInstanceOf(Error);
		expect(err.max).toBe(500);
		expect(err.current).toBe(499);
	});
});

describe('findReferencedStorageKeys (BUG-05)', () => {
	it('is one select of storage_key WHERE storage_key IN (<keys>) across the whole table, answered as a Set', async () => {
		selectResults = [[{ storageKey: 'k-1' }]];
		const referenced = await findReferencedStorageKeys(['k-1', 'k-2']);
		expect(referenced).toEqual(new Set(['k-1']));
		expect(captured.selectWheres).toHaveLength(1);
		const rendered = render(captured.selectWheres[0]);
		expect(rendered).toContain('"storage_key" in ');
		expect(rendered).toContain('k-1, k-2');
		// No project or release clause: a key referenced by ANY row must survive.
		expect(rendered).not.toContain('"project_id"');
	});

	it('builds no statement for an empty key list', async () => {
		const referenced = await findReferencedStorageKeys([]);
		expect(referenced).toEqual(new Set());
		expect(dbMock.select).not.toHaveBeenCalled();
	});
});

describe('listReleaseSourcemaps (§4.2)', () => {
	const UPLOADED = new Date('2026-09-14T08:12:44Z');
	const ROW = { id: '0f2c0000-0000-4000-8000-000000000001', minifiedPath: '/assets/index-3f2a1c.js', sizeBytes: 4193821, sha256: '9'.repeat(64), uploadedAt: UPLOADED };

	it('selects exactly the five §4.2 columns -- never storage_key -- scoped to (project_id, release_version)', async () => {
		selectResults = [[ROW]];
		const rows = await listReleaseSourcemaps('proj-1', 'v1');
		expect(rows).toEqual([ROW]);

		expect(captured.selectFields).toHaveLength(1);
		const fields = captured.selectFields[0] as Record<string, { name: string }>;
		expect(Object.keys(fields).sort()).toEqual(['id', 'minifiedPath', 'sha256', 'sizeBytes', 'uploadedAt']);
		expect(Object.values(fields).map((c) => c.name).sort()).toEqual(['id', 'minified_path', 'sha256', 'size_bytes', 'uploaded_at']);
		expect(Object.values(fields).map((c) => c.name)).not.toContain('storage_key');

		expect(captured.selectWheres).toHaveLength(1);
		expect(render(captured.selectWheres[0])).toBe(
			'("sourcemaps"."project_id" = $1 and "sourcemaps"."release_version" = $2) -- proj-1, v1'
		);
		expect(dbMock.transaction).not.toHaveBeenCalled();
	});

	it('orders minified_path ASC and hands back [] -- not null -- for a release with no rows', async () => {
		selectResults = [[]];
		const rows = await listReleaseSourcemaps('proj-1', 'v-none');
		expect(rows).toEqual([]);
		expect(rows).not.toBeNull();
		expect(captured.orderBys).toHaveLength(1);
		expect(render(captured.orderBys[0])).toBe('"sourcemaps"."minified_path" asc -- ');
		expect(captured.groupBys).toHaveLength(0);
	});
});

describe('countSourcemapsByRelease (§4.6)', () => {
	it('is ONE grouped COUNT(*) on (project_id, release_version IN <versions>) -- the issuesIntroduced shape', async () => {
		selectResults = [[{ version: 'v1.3.0', sourcemapCount: 2 }, { version: 'v1.1.0', sourcemapCount: 1 }]];
		const counts = await countSourcemapsByRelease('proj-1', ['v1.3.0', 'v1.2.0', 'v1.1.0']);

		expect(counts).toEqual(new Map([['v1.3.0', 2], ['v1.1.0', 1]]));
		// Absent, not 0, for a version with no rows: the loader is what reads `?? 0`.
		expect(counts.has('v1.2.0')).toBe(false);

		expect(captured.selectWheres).toHaveLength(1);
		expect(render(captured.selectWheres[0])).toBe(
			'("sourcemaps"."project_id" = $1 and "sourcemaps"."release_version" in ($2, $3, $4)) -- proj-1, v1.3.0, v1.2.0, v1.1.0'
		);
		expect(captured.groupBys).toHaveLength(1);
		expect(render(captured.groupBys[0])).toBe('"sourcemaps"."release_version" -- ');
		const fields = captured.selectFields[0] as Record<string, unknown>;
		expect(render(fields.sourcemapCount)).toBe('count(*)::int -- ');
		expect(captured.orderBys).toHaveLength(0);
	});

	it('builds no statement for an empty version list -- IN () is not SQL', async () => {
		const counts = await countSourcemapsByRelease('proj-1', []);
		expect(counts.size).toBe(0);
		expect(dbMock.select).not.toHaveBeenCalled();
	});
});
