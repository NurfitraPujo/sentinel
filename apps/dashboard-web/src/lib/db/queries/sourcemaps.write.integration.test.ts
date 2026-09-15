import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// sentinel-sourcemaps BUG-04: the `sourcemaps` write path (contract §4.1, decision 26) against a
// REAL Postgres. `sourcemaps.test.ts` drives `upsertReleaseSourcemaps` through a self-returning
// mock that echoes SQL fragments, and a mock has nothing to say about the three things this path
// is for -- every one of them is a database property:
//
//   1. ON CONFLICT (project_id, release_version, minified_path) DO UPDATE really does replace the
//      row in place (same `id`, new storage_key / sha256 / size_bytes, `uploaded_at` moved) rather
//      than fail on the UNIQUE or write a second row;
//   2. `pg_advisory_xact_lock` + the count re-read inside the transaction is what makes the
//      contract's sentence true -- "a concurrent upload that races past the pre-check is refused
//      there": two transactions that both saw cap−1 cannot both insert;
//   3. `ReleaseFullError` is THROWN inside the callback (D18), so the losing transaction rolls
//      back and leaves no row.
//
// The disposable-DB contract is the same as every other `*.integration.test.ts` beside this file:
// it seeds its own organization and project and deletes them again (the FK cascades the
// `sourcemaps` rows), with a per-run suffix so two concurrent runs cannot collide. The probe
// requires the `sourcemaps` TABLE, not just a listening Postgres, so a pre-1725100000 dev
// database skips rather than fails. Set SOURCEMAPS_WRITE_INTEGRATION_REQUIRED=1 (or
// SCHEMA_DRIFT_REQUIRED=1, which CI's `dashboard` job sets) to make "no reachable database" a
// hard failure instead of a silent skip.
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required =
	process.env.SOURCEMAPS_WRITE_INTEGRATION_REQUIRED === '1' || process.env.SCHEMA_DRIFT_REQUIRED === '1';

let dbReachable = false;
const probeSql = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	await probeSql`select 1 from sourcemaps limit 1`;
	dbReachable = true;
} catch {
	dbReachable = false;
} finally {
	await probeSql.end({ timeout: 1 }).catch(() => {});
}

if (required && !dbReachable) {
	throw new Error(
		`SOURCEMAPS_WRITE_INTEGRATION_REQUIRED=1 but no database with a sourcemaps table answered at ${connectionString}. ` +
			'This test is the only coverage that runs the sourcemaps upsert, its advisory lock and the ' +
			'in-transaction release_full refusal against real Postgres; skipping it silently would leave ' +
			'the ON CONFLICT DO UPDATE arm and the concurrency guarantee unverified.'
	);
}

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();

/** A key in `sourcemapStorageKey`'s shape; the content is irrelevant here, uniqueness is not. */
function key(release: string, path: string, sha: string): string {
	return `sourcemaps/${projectId}/${release}/${path.replace(/\//g, '_')}-${sha}.map`;
}
function sha(seed: string): string {
	return seed.padEnd(64, '0').slice(0, 64);
}

describe.skipIf(!dbReachable)('sourcemaps write path (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 3 });
		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'sm-write-it-' + suffix}, ${'sm-write-it-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'sm-write-it-' + suffix}, ${orgId},
			        ${'k_' + suffix}, ${'h_' + suffix})
		`;
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from sourcemaps where project_id = ${projectId}`;
			await sql`delete from projects where id = ${projectId}`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	describe('create → replace → unchanged on one (project, release, path)', () => {
		const release = 'r-crud-' + suffix;
		const path = '/assets/index-3f2a1c.js';

		it('create: the first upsert inserts one row with the given columns and counts it', async () => {
			const { upsertReleaseSourcemaps } = await import('./sourcemaps');
			const out = await upsertReleaseSourcemaps({
				projectId,
				releaseVersion: release,
				files: [{ minifiedPath: path, storageKey: key(release, path, sha('a1')), sizeBytes: 10, sha256: sha('a1') }],
				maxPerRelease: 500,
			});
			expect(out).toEqual({ releaseFileCount: 1 });

			const rows = await sql`
				select id, storage_key, size_bytes, sha256, uploaded_at
				from sourcemaps where project_id = ${projectId} and release_version = ${release}
			`;
			expect(rows).toHaveLength(1);
			expect(rows[0].storage_key).toBe(key(release, path, sha('a1')));
			expect(Number(rows[0].size_bytes)).toBe(10);
			expect(rows[0].sha256).toBe(sha('a1'));
		});

		it('replace: the same path with new bytes rewrites the SAME row -- new storage_key / sha256 / size_bytes, uploaded_at moved, id kept, still one row', async () => {
			const { upsertReleaseSourcemaps } = await import('./sourcemaps');
			const [before] = await sql`
				select id, uploaded_at from sourcemaps
				where project_id = ${projectId} and release_version = ${release} and minified_path = ${path}
			`;
			// `now()` is transaction time at microsecond resolution; make sure the clock has moved.
			await new Promise((r) => setTimeout(r, 5));

			const out = await upsertReleaseSourcemaps({
				projectId,
				releaseVersion: release,
				files: [{ minifiedPath: path, storageKey: key(release, path, sha('b2')), sizeBytes: 22, sha256: sha('b2') }],
				maxPerRelease: 500,
			});
			// A replace is not an add: the count stays 1 (§4.1's release_file_count).
			expect(out).toEqual({ releaseFileCount: 1 });

			const rows = await sql`
				select id, storage_key, size_bytes, sha256, uploaded_at
				from sourcemaps where project_id = ${projectId} and release_version = ${release}
			`;
			expect(rows).toHaveLength(1);
			expect(rows[0].id).toBe(before.id);
			expect(rows[0].storage_key).toBe(key(release, path, sha('b2')));
			expect(Number(rows[0].size_bytes)).toBe(22);
			expect(rows[0].sha256).toBe(sha('b2'));
			expect(new Date(rows[0].uploaded_at).getTime()).toBeGreaterThan(new Date(before.uploaded_at).getTime());
		});

		it('unchanged: phase 1 reports the row with its sha256 and storageKey, so the route never calls phase 3 for it', async () => {
			const { readReleaseState } = await import('./sourcemaps');
			const state = await readReleaseState(projectId, release, [path, '/assets/never-uploaded.js']);
			expect(state.current).toBe(1);
			expect(state.existing.get(path)).toEqual({ sha256: sha('b2'), storageKey: key(release, path, sha('b2')) });
			expect(state.existing.has('/assets/never-uploaded.js')).toBe(false);
		});
	});

	describe('the per-release cap inside the transaction', () => {
		const CAP = 4;

		async function seedToCapMinusOne(release: string): Promise<void> {
			const { upsertReleaseSourcemaps } = await import('./sourcemaps');
			const files = Array.from({ length: CAP - 1 }, (_, i) => ({
				minifiedPath: `/seed-${i}.js`,
				storageKey: key(release, `/seed-${i}.js`, sha(`s${i}`)),
				sizeBytes: 1,
				sha256: sha(`s${i}`),
			}));
			await upsertReleaseSourcemaps({ projectId, releaseVersion: release, files, maxPerRelease: CAP });
		}

		it('two concurrent transactions at cap−1, each adding one path: exactly one succeeds, the other rejects with ReleaseFullError and leaves no row', async () => {
			const release = 'r-race-' + suffix;
			await seedToCapMinusOne(release);
			const { upsertReleaseSourcemaps, ReleaseFullError } = await import('./sourcemaps');

			// Both start from the same phase-1 picture (cap−1 rows, one new path each), which is
			// exactly the race the route's pre-check cannot refuse. The module's `db` is a pool, so
			// the two calls run on two connections; the advisory lock is what serialises them.
			const attempt = (name: string) =>
				upsertReleaseSourcemaps({
					projectId,
					releaseVersion: release,
					files: [{ minifiedPath: `/${name}.js`, storageKey: key(release, `/${name}.js`, sha(name)), sizeBytes: 1, sha256: sha(name) }],
					maxPerRelease: CAP,
				});
			const results = await Promise.allSettled([attempt('racer-a'), attempt('racer-b')]);

			const fulfilled = results.filter((r) => r.status === 'fulfilled');
			const rejected = results.filter((r) => r.status === 'rejected');
			expect(fulfilled).toHaveLength(1);
			expect(rejected).toHaveLength(1);
			expect((fulfilled[0] as PromiseFulfilledResult<{ releaseFileCount: number }>).value).toEqual({ releaseFileCount: CAP });
			const err = (rejected[0] as PromiseRejectedResult).reason;
			expect(err).toBeInstanceOf(ReleaseFullError);
			expect(err).toMatchObject({ name: 'ReleaseFullError', max: CAP, current: CAP });

			// The loser rolled back: exactly CAP rows, and the loser's path has none.
			const [{ count }] = await sql`
				select count(*)::int as count from sourcemaps
				where project_id = ${projectId} and release_version = ${release}
			`;
			expect(count).toBe(CAP);
			const loserPath = results[0].status === 'rejected' ? '/racer-a.js' : '/racer-b.js';
			const loserRows = await sql`
				select 1 from sourcemaps
				where project_id = ${projectId} and release_version = ${release} and minified_path = ${loserPath}
			`;
			expect(loserRows).toHaveLength(0);
		});

		// The case above is the contract's sentence verbatim, but the window between two real
		// transactions' count reads is microseconds: with the lock line deleted it still passes
		// (measured: 3 of 3 runs). This one is what actually goes red without the lock -- one side
		// is hand-driven so the interleaving is forced, not hoped for.
		it('the lock is what does it: a transaction holding the release lock with an uncommitted cap-reaching row blocks the upsert until it commits, and the upsert then refuses', async () => {
			const release = 'r-lock-' + suffix;
			await seedToCapMinusOne(release);
			const { upsertReleaseSourcemaps, ReleaseFullError } = await import('./sourcemaps');

			// T1: the "other" upload, hand-driven so the interleaving is deterministic. It takes the
			// same lock id `upsertReleaseSourcemaps` computes, inserts the CAP-th row and does NOT
			// commit yet.
			let releaseT1!: () => void;
			const holdT1 = new Promise<void>((r) => (releaseT1 = r));
			let t1Reached = false;
			const t1 = sql.begin(async (tx) => {
				await tx`select pg_advisory_xact_lock(hashtextextended(${`sourcemaps:${projectId}:${release}`}, 0))`;
				await tx`
					insert into sourcemaps (project_id, release_version, minified_path, storage_key, size_bytes, sha256)
					values (${projectId}, ${release}, ${'/t1.js'}, ${key(release, '/t1.js', sha('t1'))}, 1, ${sha('t1')})
				`;
				t1Reached = true;
				await holdT1;
			});
			while (!t1Reached) await new Promise((r) => setTimeout(r, 5));

			// T2: the real function. Under READ COMMITTED it would count CAP−1 (T1's row is not
			// committed) and insert; the lock makes it wait instead.
			let settled = false;
			const t2 = upsertReleaseSourcemaps({
				projectId,
				releaseVersion: release,
				files: [{ minifiedPath: '/t2.js', storageKey: key(release, '/t2.js', sha('t2')), sizeBytes: 1, sha256: sha('t2') }],
				maxPerRelease: CAP,
			}).finally(() => (settled = true));
			await new Promise((r) => setTimeout(r, 250));
			expect(settled).toBe(false);

			releaseT1();
			await t1;
			await expect(t2).rejects.toBeInstanceOf(ReleaseFullError);
			await expect(t2).rejects.toMatchObject({ max: CAP, current: CAP });

			const [{ count }] = await sql`
				select count(*)::int as count from sourcemaps
				where project_id = ${projectId} and release_version = ${release}
			`;
			expect(count).toBe(CAP);
		});

		it('a replace on a FULL release is accepted: the re-read counts only paths with no row', async () => {
			const release = 'r-full-' + suffix;
			await seedToCapMinusOne(release);
			const { upsertReleaseSourcemaps } = await import('./sourcemaps');
			await upsertReleaseSourcemaps({
				projectId,
				releaseVersion: release,
				files: [{ minifiedPath: '/last.js', storageKey: key(release, '/last.js', sha('l1')), sizeBytes: 1, sha256: sha('l1') }],
				maxPerRelease: CAP,
			});

			const out = await upsertReleaseSourcemaps({
				projectId,
				releaseVersion: release,
				files: [{ minifiedPath: '/last.js', storageKey: key(release, '/last.js', sha('l2')), sizeBytes: 2, sha256: sha('l2') }],
				maxPerRelease: CAP,
			});
			expect(out).toEqual({ releaseFileCount: CAP });
			const [row] = await sql`
				select storage_key from sourcemaps
				where project_id = ${projectId} and release_version = ${release} and minified_path = ${'/last.js'}
			`;
			expect(row.storage_key).toBe(key(release, '/last.js', sha('l2')));
		});
	});
});
