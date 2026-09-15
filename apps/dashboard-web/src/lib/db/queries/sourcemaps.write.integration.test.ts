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
//      back and leaves no row;
//   4. (BUG-07, decision 43) `ensureObjectsPresent` runs under that same lock and before the
//      row is written, so an object a concurrent cleanup deleted between this request's put and
//      its commit is put back before any row references it.
//
// Every `it` here is order-independent (review minor 2 on sentinel#27): a `describe` owns its
// release, an `it` owns its path or seeds the row it asserts on, and a sequence whose later
// steps need the earlier ones is one `it`. `pnpm vitest run --sequence.shuffle` with any seed is
// the check.
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
/** Decision 43's check, for the cases that are not about it: every object is where it should be. */
const present = async () => {};

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

	// Order-independent by construction (review minor 2): the three phases below share one row
	// on purpose -- replace needs a row to replace and "unchanged" needs the replaced row -- so
	// they are ONE case, not three `it`s that only pass in file order. Every `describe` in this
	// file owns its release; every `it` owns its path, or seeds what it asserts on.
	describe('create → replace → unchanged on one (project, release, path)', () => {
		const release = 'r-crud-' + suffix;
		const path = '/assets/index-3f2a1c.js';

		it('create inserts one row with the given columns; replace rewrites the SAME row (new storage_key / sha256 / size_bytes, uploaded_at moved, id kept, still one row); unchanged is reported by phase 1 so the route never calls phase 3 for it', async () => {
			const { upsertReleaseSourcemaps, readReleaseState } = await import('./sourcemaps');

			// create
			const created = await upsertReleaseSourcemaps({
				projectId,
				releaseVersion: release,
				files: [{ minifiedPath: path, storageKey: key(release, path, sha('a1')), sizeBytes: 10, sha256: sha('a1') }],
				maxPerRelease: 500,
				ensureObjectsPresent: present,
			});
			expect(created).toEqual({ releaseFileCount: 1, displacedKeys: [] });

			const afterCreate = await sql`
				select id, storage_key, size_bytes, sha256, uploaded_at
				from sourcemaps where project_id = ${projectId} and release_version = ${release}
			`;
			expect(afterCreate).toHaveLength(1);
			expect(afterCreate[0].storage_key).toBe(key(release, path, sha('a1')));
			expect(Number(afterCreate[0].size_bytes)).toBe(10);
			expect(afterCreate[0].sha256).toBe(sha('a1'));
			const before = afterCreate[0];

			// replace: `now()` is transaction time at microsecond resolution; make sure the clock has moved.
			await new Promise((r) => setTimeout(r, 5));
			const replaced = await upsertReleaseSourcemaps({
				projectId,
				releaseVersion: release,
				files: [{ minifiedPath: path, storageKey: key(release, path, sha('b2')), sizeBytes: 22, sha256: sha('b2') }],
				maxPerRelease: 500,
				ensureObjectsPresent: present,
			});
			// A replace is not an add: the count stays 1 (§4.1's release_file_count); the key the
			// row carried under the lock is the one displaced (BUG-05).
			expect(replaced).toEqual({ releaseFileCount: 1, displacedKeys: [key(release, path, sha('a1'))] });

			const afterReplace = await sql`
				select id, storage_key, size_bytes, sha256, uploaded_at
				from sourcemaps where project_id = ${projectId} and release_version = ${release}
			`;
			expect(afterReplace).toHaveLength(1);
			expect(afterReplace[0].id).toBe(before.id);
			expect(afterReplace[0].storage_key).toBe(key(release, path, sha('b2')));
			expect(Number(afterReplace[0].size_bytes)).toBe(22);
			expect(afterReplace[0].sha256).toBe(sha('b2'));
			expect(new Date(afterReplace[0].uploaded_at).getTime()).toBeGreaterThan(new Date(before.uploaded_at).getTime());

			// unchanged
			const state = await readReleaseState(projectId, release, [path, '/assets/never-uploaded.js']);
			expect(state.current).toBe(1);
			expect(state.existing.get(path)).toEqual({ sha256: sha('b2'), storageKey: key(release, path, sha('b2')) });
			expect(state.existing.has('/assets/never-uploaded.js')).toBe(false);
		});
	});

	describe('BUG-05: displaced keys come from the transaction, and a referenced key is never a delete candidate', () => {
		// The review's scenario at the query layer: row {S0,K0}; A replaces it with K1; C (a CI retry
		// of the older artifact) replaces it back with K0 -- content-addressed, so the SAME key.
		// After C, K1 is unreferenced and K0 is referenced: A's cleanup, had it been deferred to
		// this moment, must delete K1 and keep K0 whatever its phase-1 snapshot said. The three
		// assertions read the state the sequence leaves, so they are one case.
		const release = 'r-displace-' + suffix;
		const path = '/assets/cart-oQLsOyVD.js';
		const K0 = () => key(release, path, sha('s0'));
		const K1 = () => key(release, path, sha('s1'));

		it('each replace reports the key the row carried at commit time; findReferencedStorageKeys answers with the keys rows still point at; a rewrite with the key the row already has displaces nothing', async () => {
			const { upsertReleaseSourcemaps, findReferencedStorageKeys } = await import('./sourcemaps');
			const file = (k: string, seed: string) => ({ minifiedPath: path, storageKey: k, sizeBytes: 1, sha256: sha(seed) });

			// create → A → C: each replace reports the key the row carried at commit time, not the
			// first one ever written.
			const create = await upsertReleaseSourcemaps({ projectId, releaseVersion: release, files: [file(K0(), 's0')], maxPerRelease: 500, ensureObjectsPresent: present });
			expect(create.displacedKeys).toEqual([]);
			const a = await upsertReleaseSourcemaps({ projectId, releaseVersion: release, files: [file(K1(), 's1')], maxPerRelease: 500, ensureObjectsPresent: present });
			expect(a.displacedKeys).toEqual([K0()]);
			const c = await upsertReleaseSourcemaps({ projectId, releaseVersion: release, files: [file(K0(), 's0')], maxPerRelease: 500, ensureObjectsPresent: present });
			expect(c.displacedKeys).toEqual([K1()]);

			// The reference check reads the whole table: K0 is referenced, K1 no longer is.
			const referenced = await findReferencedStorageKeys([K0(), K1(), 'sourcemaps/nobody/has/this.map']);
			expect(referenced).toEqual(new Set([K0()]));
			expect(await findReferencedStorageKeys([])).toEqual(new Set());

			// An upsert that rewrites the row with the key it already has displaces nothing.
			const again = await upsertReleaseSourcemaps({ projectId, releaseVersion: release, files: [file(K0(), 's0')], maxPerRelease: 500, ensureObjectsPresent: present });
			expect(again.displacedKeys).toEqual([]);
		});
	});

	describe('BUG-07 (decision 43): every key a row is committed to is confirmed under the lock, before the row is written', () => {
		// The review's interleaving, driven by hand against a simulated bucket (a Set of keys; the
		// route's real `ensureObjectsPresent` HEADs and re-puts against S3, and the route test pins
		// that -- what a mock cannot show is WHEN the transaction runs it, which is this file's job).
		//
		//   row {S0,K0}.
		//   A commits row→K1 and deletes K0 (unreferenced -- correct).
		//   B read phase 1 before A and is mid-phase-2: it puts K1 (same bytes, same key as A's).
		//   C (a CI retry of the older artifact) puts K0, commits row→K0 (displaced = [K1]); its
		//     reference check finds no row on K1 -- B has not committed -- so C deletes K1.
		//   B commits row→K1.
		//
		// Before this fix B's row pointed at an object C had removed. With it, B's transaction
		// asks the bucket for K1 under the lock, finds it gone, puts it back, and only then writes
		// the row: at commit the row's key exists.
		const release = 'r-bug07-' + suffix;
		const path = '/assets/checkout-CT5Yxj3h.js';
		const K0 = () => key(release, path, sha('b7-s0'));
		const K1 = () => key(release, path, sha('b7-s1'));
		const file = (k: string, seed: string) => ({ minifiedPath: path, storageKey: k, sizeBytes: 1, sha256: sha(seed) });
		const lockId = `sourcemaps:${projectId}:${release}`;

		it('the A/B/C interleaving: B re-puts K1 inside its transaction and its committed row points at an object that exists', async () => {
			const { upsertReleaseSourcemaps, findReferencedStorageKeys } = await import('./sourcemaps');
			const bucket = new Set<string>();
			const repairs: string[] = [];
			/** The route's `ensureObjectsPresent`, over the simulated bucket: HEAD, and re-put when gone. */
			const ensure = async (keys: readonly string[]) => {
				for (const k of keys) {
					if (bucket.has(k)) continue;
					bucket.add(k);
					repairs.push(k);
				}
			};

			// row {S0,K0}
			bucket.add(K0());
			await upsertReleaseSourcemaps({ projectId, releaseVersion: release, files: [file(K0(), 'b7-s0')], maxPerRelease: 500, ensureObjectsPresent: ensure });
			// A: put K1, commit row→K1, delete K0 (no row references it).
			bucket.add(K1());
			const a = await upsertReleaseSourcemaps({ projectId, releaseVersion: release, files: [file(K1(), 'b7-s1')], maxPerRelease: 500, ensureObjectsPresent: ensure });
			expect(a.displacedKeys).toEqual([K0()]);
			expect(await findReferencedStorageKeys(a.displacedKeys)).toEqual(new Set());
			bucket.delete(K0());
			// B, mid-phase-2: puts K1 (content-addressed: A's key).
			bucket.add(K1());
			// C: puts K0, commits row→K0, and its post-commit check finds no row on K1 -- B has not
			// committed -- so C deletes K1.
			bucket.add(K0());
			const c = await upsertReleaseSourcemaps({ projectId, releaseVersion: release, files: [file(K0(), 'b7-s0')], maxPerRelease: 500, ensureObjectsPresent: ensure });
			expect(c.displacedKeys).toEqual([K1()]);
			expect(await findReferencedStorageKeys(c.displacedKeys)).toEqual(new Set());
			bucket.delete(K1());
			expect(bucket.has(K1())).toBe(false);
			expect(repairs).toEqual([]);

			// B commits row→K1.
			const b = await upsertReleaseSourcemaps({ projectId, releaseVersion: release, files: [file(K1(), 'b7-s1')], maxPerRelease: 500, ensureObjectsPresent: ensure });
			expect(b.displacedKeys).toEqual([K0()]);

			// The row's key exists: B's transaction put K1 back before it wrote the row.
			const [row] = await sql`
				select storage_key from sourcemaps
				where project_id = ${projectId} and release_version = ${release} and minified_path = ${path}
			`;
			expect(row.storage_key).toBe(K1());
			expect(bucket.has(K1())).toBe(true);
			expect(repairs).toEqual([K1()]);
			// And B's own cleanup, run through the same guard, keeps K0 only if a row references it
			// -- none does now -- so it goes; no row points at nothing either way.
			expect(await findReferencedStorageKeys(b.displacedKeys)).toEqual(new Set());
		});

		it('the check runs UNDER the release lock and BEFORE the row is written: it waits for a hand-held lock, and at check time another connection still sees the old key', async () => {
			const { upsertReleaseSourcemaps } = await import('./sourcemaps');
			// Its own row, on its own path: this case does not depend on the one above having run.
			const path = '/assets/lock-b7.js';
			const K1 = key(release, path, sha('b7-lock-1'));
			const K2 = key(release, path, sha('b7-lock-2'));
			await upsertReleaseSourcemaps({ projectId, releaseVersion: release, files: [{ minifiedPath: path, storageKey: K1, sizeBytes: 1, sha256: sha('b7-lock-1') }], maxPerRelease: 500, ensureObjectsPresent: present });

			// T1 holds the release lock (the same id the function computes) and does not commit.
			let releaseT1!: () => void;
			const holdT1 = new Promise<void>((r) => (releaseT1 = r));
			let t1Reached = false;
			const t1 = sql.begin(async (tx) => {
				await tx`select pg_advisory_xact_lock(hashtextextended(${lockId}, 0))`;
				t1Reached = true;
				await holdT1;
			});
			while (!t1Reached) await new Promise((r) => setTimeout(r, 5));

			const seenAtCheck: { lockFree: boolean | null; keyAtCheck: string | null } = { lockFree: null, keyAtCheck: null };
			const ensure = async (keys: readonly string[]) => {
				expect(keys).toEqual([K2]);
				// Another connection cannot take the lock now: this transaction holds it.
				const [{ free }] = await sql`select pg_try_advisory_lock(hashtextextended(${lockId}, 0)) as free`;
				if (free) await sql`select pg_advisory_unlock(hashtextextended(${lockId}, 0))`;
				seenAtCheck.lockFree = Boolean(free);
				// And nothing has been written yet: outside the transaction the row still carries K1.
				const [row] = await sql`
					select storage_key from sourcemaps
					where project_id = ${projectId} and release_version = ${release} and minified_path = ${path}
				`;
				seenAtCheck.keyAtCheck = row.storage_key;
			};

			let settled = false;
			const t2 = upsertReleaseSourcemaps({
				projectId,
				releaseVersion: release,
				files: [{ minifiedPath: path, storageKey: K2, sizeBytes: 1, sha256: sha('b7-lock-2') }],
				maxPerRelease: 500,
				ensureObjectsPresent: ensure,
			}).finally(() => (settled = true));
			await new Promise((r) => setTimeout(r, 250));
			// Blocked on the lock, so the check has not run either.
			expect(settled).toBe(false);
			expect(seenAtCheck.lockFree).toBeNull();

			releaseT1();
			await t1;
			await expect(t2).resolves.toMatchObject({ displacedKeys: [K1] });
			expect(seenAtCheck).toEqual({ lockFree: false, keyAtCheck: K1 });
			const [row] = await sql`
				select storage_key from sourcemaps
				where project_id = ${projectId} and release_version = ${release} and minified_path = ${path}
			`;
			expect(row.storage_key).toBe(K2);
		});

		it('a check that throws rolls the transaction back: the row keeps its previous key (D18)', async () => {
			const { upsertReleaseSourcemaps } = await import('./sourcemaps');
			const path = '/assets/throw-b7.js';
			const K1 = key(release, path, sha('b7-throw-1'));
			await upsertReleaseSourcemaps({ projectId, releaseVersion: release, files: [{ minifiedPath: path, storageKey: K1, sizeBytes: 1, sha256: sha('b7-throw-1') }], maxPerRelease: 500, ensureObjectsPresent: present });
			const K3 = key(release, path, sha('b7-throw-3'));
			await expect(
				upsertReleaseSourcemaps({
					projectId,
					releaseVersion: release,
					files: [{ minifiedPath: path, storageKey: K3, sizeBytes: 1, sha256: sha('b7-throw-3') }],
					maxPerRelease: 500,
					ensureObjectsPresent: async () => {
						throw new Error('object gone and the re-put failed');
					},
				})
			).rejects.toThrow('object gone and the re-put failed');
			const [row] = await sql`
				select storage_key from sourcemaps
				where project_id = ${projectId} and release_version = ${release} and minified_path = ${path}
			`;
			expect(row.storage_key).toBe(K1);
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
			await upsertReleaseSourcemaps({ projectId, releaseVersion: release, files, maxPerRelease: CAP, ensureObjectsPresent: present });
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
					ensureObjectsPresent: present,
				});
			const results = await Promise.allSettled([attempt('racer-a'), attempt('racer-b')]);

			const fulfilled = results.filter((r) => r.status === 'fulfilled');
			const rejected = results.filter((r) => r.status === 'rejected');
			expect(fulfilled).toHaveLength(1);
			expect(rejected).toHaveLength(1);
			expect((fulfilled[0] as PromiseFulfilledResult<{ releaseFileCount: number }>).value).toMatchObject({ releaseFileCount: CAP });
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
				ensureObjectsPresent: present,
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
				ensureObjectsPresent: present,
			});

			const out = await upsertReleaseSourcemaps({
				projectId,
				releaseVersion: release,
				files: [{ minifiedPath: '/last.js', storageKey: key(release, '/last.js', sha('l2')), sizeBytes: 2, sha256: sha('l2') }],
				maxPerRelease: CAP,
				ensureObjectsPresent: present,
			});
			expect(out).toEqual({ releaseFileCount: CAP, displacedKeys: [key(release, '/last.js', sha('l1'))] });
			const [row] = await sql`
				select storage_key from sourcemaps
				where project_id = ${projectId} and release_version = ${release} and minified_path = ${'/last.js'}
			`;
			expect(row.storage_key).toBe(key(release, '/last.js', sha('l2')));
		});
	});
});
