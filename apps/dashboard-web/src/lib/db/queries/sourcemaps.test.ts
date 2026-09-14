import { describe, it, expect, vi, beforeEach } from 'vitest';
import { PgDialect } from 'drizzle-orm/pg-core';
import type { SQL } from 'drizzle-orm';

/**
 * sentinel-sourcemaps §4.1 / decision 26 -- the `sourcemaps` write path.
 *
 * What is load-bearing: the upsert conflicts on §4.5's unique key `(project_id, release_version,
 * minified_path)` and rewrites exactly the four replace columns; the per-release count is re-read
 * INSIDE the transaction under a per-release lock and a lost race is a thrown `ReleaseFullError`
 * (D18: thrown, never returned) with nothing inserted; and phase 1's read is two plain selects
 * with no transaction open.
 */

interface Captured {
	values: unknown[];
	conflicts: unknown[];
	executes: unknown[];
	selectWheres: unknown[];
}

const captured: Captured = { values: [], conflicts: [], executes: [], selectWheres: [] };

/** Queued results for each `select().from().where()` chain, in call order. */
let selectResults: unknown[][] = [];

function makeExecutor() {
	const m: any = {};
	m.select = vi.fn(() => m);
	m.from = vi.fn(() => m);
	m.where = vi.fn((w: unknown) => {
		captured.selectWheres.push(w);
		const next = selectResults.shift() ?? [];
		return { then: (resolve: any) => resolve(next) };
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

const { readReleaseState, upsertReleaseSourcemaps, ReleaseFullError } = await import('./sourcemaps');

const dialect = new PgDialect();
function render(value: unknown): string {
	const { sql, params } = dialect.sqlToQuery(value as SQL);
	return `${sql} -- ${params.map(String).join(', ')}`;
}

beforeEach(() => {
	vi.clearAllMocks();
	captured.values = [];
	captured.conflicts = [];
	captured.executes = [];
	captured.selectWheres = [];
	selectResults = [];
});

const FILE_A = { minifiedPath: '/a.js', storageKey: 'sourcemaps/p/r/a-1.map', sizeBytes: 10, sha256: '1'.repeat(64) };
const FILE_B = { minifiedPath: '/b.js', storageKey: 'sourcemaps/p/r/b-2.map', sizeBytes: 20, sha256: '2'.repeat(64) };

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
		selectResults = [[{ count: 1 }], [{ minifiedPath: '/a.js' }]];
		const out = await upsertReleaseSourcemaps({
			projectId: 'proj-1',
			releaseVersion: 'v1',
			files: [FILE_A, FILE_B],
			maxPerRelease: 500,
		});
		expect(dbMock.transaction).toHaveBeenCalledTimes(1);
		expect(captured.executes).toHaveLength(1);
		expect(render(captured.executes[0])).toContain('pg_advisory_xact_lock(hashtextextended(');
		expect(render(captured.executes[0])).toContain('sourcemaps:proj-1:v1');
		// /a.js already present (a replace), /b.js new: 1 + 1 = 2.
		expect(out).toEqual({ releaseFileCount: 2 });
		expect(captured.values).toHaveLength(2);
	});

	it('conflicts on (project_id, release_version, minified_path) and rewrites exactly the four replace columns', async () => {
		selectResults = [[{ count: 0 }], []];
		await upsertReleaseSourcemaps({ projectId: 'proj-1', releaseVersion: 'v1', files: [FILE_A], maxPerRelease: 500 });
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
			upsertReleaseSourcemaps({ projectId: 'proj-1', releaseVersion: 'v1', files: [FILE_A], maxPerRelease: 500 })
		).rejects.toMatchObject({ name: 'ReleaseFullError', max: 500, current: 500 });
		expect(captured.values).toHaveLength(0);
		expect(dbMock.insert).not.toHaveBeenCalled();
	});

	it('does not count a replace as an add: a full release still accepts a re-upload of its own paths', async () => {
		selectResults = [[{ count: 500 }], [{ minifiedPath: '/a.js' }]];
		const out = await upsertReleaseSourcemaps({
			projectId: 'proj-1',
			releaseVersion: 'v1',
			files: [FILE_A],
			maxPerRelease: 500,
		});
		expect(out).toEqual({ releaseFileCount: 500 });
		expect(captured.values).toHaveLength(1);
	});

	it('exports ReleaseFullError as a thrown Error carrying max and current (D18)', () => {
		const err = new ReleaseFullError(500, 499);
		expect(err).toBeInstanceOf(Error);
		expect(err.max).toBe(500);
		expect(err.current).toBe(499);
	});
});
