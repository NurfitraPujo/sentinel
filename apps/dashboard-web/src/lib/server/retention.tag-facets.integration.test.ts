import { describe, it, expect, beforeAll, afterAll, afterEach, vi } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// sentinel-issue-tag-facets spec §5 and §10.2 (TASK-01 "retention", "retention pacing" suite half,
// "retention route" real-Postgres half, "drift is repaired" dashboard half): the real cleanupRetainedData
// and the real POST /api/cron/retention against a migrated Postgres.
//
// THIS SUITE MUST NOT SKIP UNDER THE GATE (contract decision 5). `task test-integration` runs it through
// scripts/test-dashboard-pg.sh with TAG_FACETS_INTEGRATION_REQUIRED=1 against a disposable, migrated
// Postgres; CI's dashboard job sets the same flag. With the flag set, no database or no issue_tag_values
// table is a failure at module load, never a skip.
//
// Every exactness assertion runs the shared check query SCOPED to the issues this suite created ($1), so
// it asserts on the state the suite made and nothing else. The suite holds the retention family's
// advisory lock (`sentinel.retention.integration`) for its whole run, so it never runs the real sweep
// beside another retention suite.

const connectionString = process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.TAG_FACETS_INTEGRATION_REQUIRED === '1';

let dbReachable = false;
let migrated = false;
const probeSql = postgres(connectionString, { connect_timeout: 3, max: 1, onnotice: () => {} });
try {
	await probeSql`select 1`;
	dbReachable = true;
	const [{ t }] = await probeSql`select to_regclass('issue_tag_values')::text as t`;
	migrated = t !== null;
} catch {
	dbReachable = false;
} finally {
	await probeSql.end({ timeout: 1 }).catch(() => {});
}
if (required && !(dbReachable && migrated)) {
	throw new Error(
		`TAG_FACETS_INTEGRATION_REQUIRED=1 but ${dbReachable ? 'issue_tag_values does not exist in' : 'no database answered at'} ` +
			`${connectionString}. This suite is the gate's never-skip proof that retention keeps the tag rollup exact ` +
			'(sentinel-issue-tag-facets contract decision 5); skipping it would leave that unproven.'
	);
}

// Tunable constants, overridden per case through live getters (retention.ts and the route read them at
// every use).
const overrides: Record<string, number> = {};
vi.mock('$lib/server/retention-batch', async (importOriginal) => {
	const actual = await importOriginal<Record<string, unknown>>();
	const mod: Record<string, unknown> = {};
	for (const [k, v] of Object.entries(actual)) {
		if (typeof v === 'number') Object.defineProperty(mod, k, { get: () => overrides[k] ?? v, enumerable: true });
		else mod[k] = v;
	}
	return mod;
});
const CRON_SECRET = 'tag-facets-integration-cron-secret';
vi.mock('$env/dynamic/private', () => ({ env: { ...process.env, CRON_SECRET } }));

const { cleanupRetainedData, rebuildPendingTagRollups } = await import('./retention');
const { openRetentionRunClient } = await import('./db');
const { openRetentionRun } = await import('./retention-run');
const { retentionRunOutcome } = await import('./retention-batch');
const { log } = await import('./observability/log');
const { TAG_FACETS_CHECK_SQL } = await import('./tag-facets-check.sql');
const { POST } = await import('../../routes/api/cron/retention/+server');

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();
const createdIssues = new Set<string>();

function hoursAgo(days: number, extraHours: number): Date {
	return new Date(Date.now() - days * 86_400_000 - extraHours * 3_600_000);
}

type Sql = ReturnType<typeof postgres>;
let sql: Sql;

async function seedIssue(id: string = randomUUID(), opts: { firstSeen?: Date } = {}): Promise<string> {
	const firstSeen = opts.firstSeen ?? new Date();
	await sql`
		insert into issues (id, project_id, environment, fingerprint, message, error_class, status, first_seen, last_seen, count)
		values (${id}, ${projectId}, 'production', ${'fp-tfr-' + id}, 'tag facets retention', 'TagFacetsError',
		        'unresolved', ${firstSeen}, ${firstSeen}, 0)`;
	createdIssues.add(id);
	return id;
}

interface Occ {
	issueId: string;
	createdAt: Date | string;
	release?: string;
	tags?: Record<string, string>;
	userId?: string;
}

async function insertOccs(rows: Occ[]): Promise<string[]> {
	const ids: string[] = [];
	for (let off = 0; off < rows.length; off += 500) {
		const chunk = rows.slice(off, off + 500).map((r) => {
			const id = randomUUID();
			ids.push(id);
			return {
				id,
				issue_id: r.issueId,
				project_id: projectId,
				environment: 'production',
				platform: 'go',
				release_version: r.release ?? '',
				created_at: typeof r.createdAt === 'string' ? r.createdAt : r.createdAt.toISOString(),
				tags: r.tags ? JSON.stringify(r.tags) : null,
				event_user: r.userId ? JSON.stringify({ id: r.userId }) : null,
			};
		});
		await sql`
			insert into error_occurrences (id, issue_id, project_id, environment, platform, release_version,
			                               stacktrace, metadata, created_at, tags, event_user)
			select x.id::uuid, x.issue_id::uuid, x.project_id::uuid, x.environment, x.platform, x.release_version,
			       '[]'::jsonb, '{}'::jsonb, x.created_at::timestamptz, x.tags::jsonb, x.event_user::jsonb
			  from json_to_recordset(${JSON.stringify(chunk)}::text::json)
			       as x(id text, issue_id text, project_id text, environment text, platform text, release_version text,
			            created_at text, tags text, event_user text)`;
		await sql`insert into error_search_index (occurrence_id) select unnest(${chunk.map((c) => c.id)}::uuid[])`;
	}
	return ids;
}

async function check(issueIds: string[]) {
	const [row] = await sql.unsafe(TAG_FACETS_CHECK_SQL, [issueIds]);
	return Object.fromEntries(Object.entries(row).map(([k, v]) => [k, Number(v)]));
}
async function expectExact(issueIds: string[]) {
	expect(await check(issueIds)).toEqual({ named_mismatch: 0, key_total_mismatch: 0, value_nowhere: 0, zero_rows: 0, over_cap: 0 });
}
async function present(ids: string[]): Promise<number> {
	const [{ n }] = await sql`select count(*)::int as n from error_occurrences where id = any(${ids}::uuid[])`;
	return n;
}
async function rollupCount(issueId: string, key: string, value: string): Promise<number> {
	const rows = await sql`select count from issue_tag_values where issue_id = ${issueId} and key = ${key} and value = ${value}`;
	return rows.length ? Number(rows[0].count) : 0;
}

// Drains aged rows other suites (or earlier cases) left, so a case's batch counts are its own.
async function drain(retentionDays = 30) {
	await cleanupRetainedData(retentionDays, 365, 30);
}

const events = (spy: ReturnType<typeof vi.spyOn>, name: string) =>
	spy.mock.calls.filter((c) => c[0] === name).map((c) => (c[1] ?? {}) as Record<string, unknown>);

const noSleep = { sleep: async () => {} };

function cronPost() {
	return POST({
		request: new Request('http://x/api/cron/retention', { method: 'POST', headers: { 'x-cron-secret': CRON_SECRET } }),
	} as never) as Promise<Response>;
}

async function runBackends(): Promise<{ pid: number; blocking: number[] }[]> {
	return (await sql`
		select pid, pg_blocking_pids(pid) as blocking from pg_stat_activity
		 where application_name = 'sentinel.retention.run' and datname = current_database()`) as never;
}
async function routeLockHeld(): Promise<boolean> {
	const [{ n }] = await sql`
		select count(*)::int as n from pg_locks
		 where locktype = 'advisory' and granted and objsubid = 1
		   and ((classid::bigint << 32) | objid::bigint) = hashtext('sentinel.retention.route')::bigint`;
	return n > 0;
}
async function pollUntil(what: string, cond: () => Promise<boolean>, timeoutMs = 10_000) {
	const deadline = Date.now() + timeoutMs;
	while (Date.now() < deadline) {
		if (await cond()) return;
		await new Promise((r) => setTimeout(r, 20));
	}
	throw new Error(`timed out waiting for: ${what}`);
}

// Issue ids whose uuid order the lock cases choose: ...01 < ... < ...50 under one random prefix.
function orderedIds(): (n: number) => string {
	const prefix = randomUUID().slice(0, 8);
	return (n: number) => `${prefix}-0000-4000-8000-${n.toString().padStart(12, '0')}`;
}

// The per-batch-lock fixture: 50 aged rows over 50 issues, the 7 oldest on issues ...10-...16 and a
// younger row on ...01.
async function lockFixture(retentionDays = 30) {
	const id = orderedIds();
	const order = [10, 11, 12, 13, 14, 15, 16, 1, 2, 3, 4, 5, 6, 7, 8, 9, ...Array.from({ length: 34 }, (_, i) => 17 + i)];
	for (const n of order) await seedIssue(id(n));
	const base = hoursAgo(retentionDays, 3);
	const occIds = await insertOccs(order.map((n, k) => ({ issueId: id(n), createdAt: new Date(base.getTime() + k * 1000), release: 'r-lock' })));
	return { id, occIds, issueIds: order.map((n) => id(n)) };
}

describe.skipIf(!(dbReachable && migrated))('retention keeps the tag rollup exact (sentinel-issue-tag-facets, real Postgres)', () => {
	let warn: ReturnType<typeof vi.spyOn>;
	let info: ReturnType<typeof vi.spyOn>;
	let error: ReturnType<typeof vi.spyOn>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 4, onnotice: () => {} });
		// The retention family mutex: never run the real sweep beside another retention suite.
		await sql`select pg_advisory_lock(hashtext('sentinel.retention.integration'))`;
		await sql`insert into organizations (id, name, slug) values (${orgId}, ${'tfr-' + suffix}, ${'tfr-' + suffix})`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'tfr-' + suffix}, ${orgId}, ${'k_' + suffix}, ${'h_' + suffix})`;
	}, 180_000);

	afterEach(async () => {
		for (const k of Object.keys(overrides)) delete overrides[k];
		vi.restoreAllMocks();
		const ids = [...createdIssues];
		if (ids.length) {
			await sql`delete from issue_tag_values_drift where issue_id = any(${ids}::uuid[])`;
			await sql`delete from issues where id = any(${ids}::uuid[])`;
		}
		createdIssues.clear();
	});

	afterAll(async () => {
		if (!sql) return;
		await sql`delete from projects where id = ${projectId}`.catch(() => {});
		await sql`delete from organizations where id = ${orgId}`.catch(() => {});
		await sql`select pg_advisory_unlock(hashtext('sentinel.retention.integration'))`.catch(() => {});
		await sql.end({ timeout: 5 });
	});

	function spies() {
		warn = vi.spyOn(log, 'warn');
		info = vi.spyOn(log, 'info');
		error = vi.spyOn(log, 'error');
	}

	// ---- TASK-01 retention: the sweep equals GROUP BY over the survivors; 0-rows deleted ----
	it('sweep: the rollup equals GROUP BY over the survivors; aged-only value, aged overflow and an orphaned issue go', async () => {
		await drain();
		const aged = hoursAgo(30, 2);
		const fresh = new Date();
		const a = await seedIssue();
		const capIssue = await seedIssue();
		const orphan = await seedIssue(undefined, { firstSeen: hoursAgo(40, 0) });
		const agedIds = await insertOccs([
			...Array.from({ length: 5 }, () => ({ issueId: a, createdAt: aged, release: 'only-aged' })),
			{ issueId: a, createdAt: aged, release: 'both' },
			// capIssue: 1000 fresh values named first, then aged values past the cap (the overflow row)
			...Array.from({ length: 3 }, (_, i) => ({ issueId: orphan, createdAt: aged, release: `o${i}` })),
		]);
		await insertOccs([
			...Array.from({ length: 4 }, () => ({ issueId: a, createdAt: fresh, release: 'both' })),
			...Array.from({ length: 1000 }, (_, i) => ({ issueId: capIssue, createdAt: fresh, userId: `u${i.toString().padStart(4, '0')}` })),
		]);
		const agedOverflow = await insertOccs(Array.from({ length: 30 }, (_, i) => ({ issueId: capIssue, createdAt: aged, userId: `z${i}` })));
		expect(await rollupCount(capIssue, 'user.id', '')).toBe(30);
		await expectExact([a, capIssue, orphan]);

		const result = await cleanupRetainedData(30, 365, 30, undefined, noSleep);

		await expectExact([a, capIssue, orphan]);
		expect(await rollupCount(a, 'release', 'only-aged')).toBe(0);
		expect(await rollupCount(a, 'release', 'both')).toBe(4);
		expect(await rollupCount(capIssue, 'user.id', '')).toBe(0); // the overflow row went with its last occurrence
		const [{ n: orphanRows }] = await sql`select count(*)::int as n from issue_tag_values where issue_id = ${orphan}`;
		expect(orphanRows).toBe(0);
		const [{ n: orphanIssue }] = await sql`select count(*)::int as n from issues where id = ${orphan}`;
		expect(orphanIssue).toBe(0);
		expect(result.deletedOccurrences).toBeGreaterThanOrEqual(agedIds.length + agedOverflow.length);
		expect(await present([...agedIds, ...agedOverflow])).toBe(0);
		const [{ zero }] = await sql`select count(*)::int as zero from issue_tag_values where count <= 0`;
		expect(zero).toBe(0);
	}, 60_000);

	it('lock order: the sweep waits on an issue lock before it holds any occurrence row lock', async () => {
		await drain();
		const x = await seedIssue();
		const ids = await insertOccs(Array.from({ length: 5 }, () => ({ issueId: x, createdAt: hoursAgo(30, 2), release: 'lo' })));
		const holder = postgres(connectionString, { max: 1, onnotice: () => {} });
		try {
			let heldCheck: Promise<void> = Promise.resolve();
			await holder.begin(async (h) => {
				await h`select id from issues where id = ${x} for update`;
				const run = cleanupRetainedData(30, 365, 30, undefined, noSleep);
				let waiter = 0;
				await pollUntil('the sweep to wait on the issue lock', async () => {
					const rows = await sql`
						select pid from pg_stat_activity
						 where application_name = 'sentinel.retention.run' and wait_event_type = 'Lock'
						   and query ilike '%from issues%for update%'`;
					waiter = rows.length ? rows[0].pid : 0;
					return waiter !== 0;
				});
				const locks = await sql`
					select c.relname, l.mode from pg_locks l join pg_class c on c.oid = l.relation
					 where l.pid = ${waiter} and l.granted`;
				expect(locks.filter((l) => l.relname === 'error_occurrences' && l.mode === 'RowExclusiveLock')).toEqual([]);
				heldCheck = run.then(() => undefined);
			});
			await heldCheck;
		} finally {
			await holder.end({ timeout: 1 });
		}
		await expectExact([x]);
		// The batch either waited it out or stopped at its 250 ms lock timeout; the next run finishes it.
		await cleanupRetainedData(30, 365, 30, undefined, noSleep);
		expect(await present(ids)).toBe(0);
		await expectExact([x]);
	}, 60_000);

	// ---- retention pacing (suite half) ----
	it('paced batches: 50 aged rows at batch 7 -> 8 batches, a pause after each, drained, no backlog', async () => {
		await drain();
		overrides.RETENTION_OCCURRENCE_BATCH = 7;
		const issue = await seedIssue();
		const ids = await insertOccs(Array.from({ length: 50 }, (_, i) => ({ issueId: issue, createdAt: hoursAgo(30, 1 + i / 100), release: 'p' })));
		const sleeps: number[] = [];
		const r = await cleanupRetainedData(30, 365, 30, undefined, { sleep: async (ms) => void sleeps.push(ms) });
		expect(r.occurrenceBatches).toBe(8);
		expect(sleeps).toHaveLength(8);
		for (const s of sleeps) expect(s).toBeGreaterThanOrEqual(50);
		if (r.occurrenceHeldMaxMs > 50) expect(Math.max(...sleeps)).toBeGreaterThanOrEqual(r.occurrenceHeldMaxMs - 1e-6);
		expect(r.occurrenceHeldMaxMs).toBeGreaterThan(0);
		expect(r.occurrenceHeldP95Ms).toBeGreaterThan(0);
		expect(r.occurrenceHeldP95Ms).toBeLessThanOrEqual(r.occurrenceHeldMaxMs);
		expect(r.occurrenceStopped).toBe('drained');
		expect(r.occurrenceBacklogSeconds).toBe(0);
		expect(await present(ids)).toBe(0);
		await expectExact([issue]);
	}, 60_000);

	it('budget 0: nothing deleted, stopped by the budget, the backlog reported; the next run catches up', async () => {
		await drain();
		spies();
		const issue = await seedIssue();
		const ids = await insertOccs(Array.from({ length: 20 }, () => ({ issueId: issue, createdAt: hoursAgo(30, 3), release: 'b' })));
		overrides.RETENTION_OCCURRENCE_BUDGET_MS = 0;
		const r = await cleanupRetainedData(30, 365, 30, undefined, noSleep);
		expect(await present(ids)).toBe(20);
		expect(r.occurrenceStopped).toBe('budget');
		expect(r.occurrenceBatches).toBe(0); // no batch starts with less than the minimum of the budget left
		expect(r.occurrenceBacklogSeconds).toBeGreaterThanOrEqual(10_800 - 60);
		expect(events(warn, 'retention.occurrence_budget_exhausted')).toHaveLength(1);
		expect(events(warn, 'retention.occurrence_backlog')).toHaveLength(1);
		delete overrides.RETENTION_OCCURRENCE_BUDGET_MS;
		await cleanupRetainedData(30, 365, 30, undefined, noSleep);
		expect(await present(ids)).toBe(0);
		await expectExact([issue]);
	}, 60_000);

	it('keyset cursor: equal microsecond timestamps across batches, every later read carries the cursor', async () => {
		await drain();
		overrides.RETENTION_OCCURRENCE_BATCH = 2;
		const issue = await seedIssue();
		const iso = hoursAgo(30, 2).toISOString(); // ...sss Z
		const at = (micro: string) => iso.replace(/(\.\d{3})Z$/, `$1${micro}Z`);
		const ids = await insertOccs([
			...Array.from({ length: 3 }, () => ({ issueId: issue, createdAt: at('400'), release: 'k' })),
			...Array.from({ length: 2 }, () => ({ issueId: issue, createdAt: at('401'), release: 'k' })),
		]);
		const statements: string[] = [];
		let lost = false;
		const client = openRetentionRunClient(() => {
			lost = true;
		});
		const spied = Object.assign((s: TemplateStringsArray, ...v: unknown[]) => client(s, ...(v as never[])), {
			unsafe: (q: string, p?: unknown[]) => {
				statements.push(q);
				return client.unsafe(q, p as never);
			},
			options: client.options,
		});
		try {
			const run = openRetentionRun(spied as never, { deadlineMs: 170_000, isLost: () => lost });
			const r = await cleanupRetainedData(30, 365, 30, run, noSleep);
			expect(r.occurrenceBatches).toBe(3);
		} finally {
			await client.end({ timeout: 1 });
		}
		expect(await present(ids)).toBe(0);
		const reads = statements.filter((q) => q.includes('created_at::text AS created_at_text'));
		expect(reads.length).toBe(4); // 3 batches + the empty read that drained
		expect(reads[0]).not.toContain('(created_at, id) >');
		for (const q of reads.slice(1)) expect(q).toContain('(created_at, id) >');
		await expectExact([issue]);
	}, 60_000);

	it('a row stored already aged behind the cursor leaves a drained run completed; the next run deletes it', async () => {
		await drain(7);
		overrides.RETENTION_OCCURRENCE_BATCH = 7;
		const issue = await seedIssue();
		const ids = await insertOccs(Array.from({ length: 50 }, (_, i) => ({ issueId: issue, createdAt: hoursAgo(7, 1 + i / 100), release: 'late' })));
		let late: string[] = [];
		let first = true;
		const r = await cleanupRetainedData(7, 365, 30, undefined, {
			sleep: async () => {
				if (!first) return;
				first = false;
				late = await insertOccs([{ issueId: issue, createdAt: hoursAgo(10, 0), release: 'late' }]);
			},
		});
		expect(r.occurrenceStopped).toBe('drained');
		expect(r.deletedOccurrences).toBe(50);
		expect(await present(late)).toBe(1);
		expect(r.occurrenceBacklogSeconds).toBeGreaterThanOrEqual(259_200 - 60);
		expect(retentionRunOutcome(r)).toBe('completed');
		const r2 = await cleanupRetainedData(7, 365, 30, undefined, noSleep);
		expect(await present([...ids, ...late])).toBe(0);
		expect(r2.occurrenceBacklogSeconds).toBe(0);
		await expectExact([issue]);
	}, 60_000);

	it('lock wait bounded: a held issue stops the step with lock_timeout while the holder is open; nothing is held after', async () => {
		await drain();
		spies();
		overrides.RETENTION_OCCURRENCE_BATCH = 7;
		const { id, occIds, issueIds } = await lockFixture();
		const holder = postgres(connectionString, { max: 1, onnotice: () => {} });
		let r: Awaited<ReturnType<typeof cleanupRetainedData>> | null = null;
		try {
			await holder.begin(async (h) => {
				await h`select id from issues where id = ${id(16)} for update`;
				r = await Promise.race([
					cleanupRetainedData(30, 365, 30, undefined, noSleep),
					new Promise<never>((_, rej) => setTimeout(() => rej(new Error('safety timeout: the run waited for the holder')), 10_000)),
				]);
				// The batch released what it held.
				const probe = postgres(connectionString, { max: 1, onnotice: () => {} });
				try {
					await probe.begin(async (p) => {
						await p`select id from issues where id = ${id(10)} for update nowait`;
					});
				} finally {
					await probe.end({ timeout: 1 });
				}
			});
		} finally {
			await holder.end({ timeout: 1 });
		}
		expect(r!.occurrenceStopped).toBe('lock_timeout');
		expect(events(warn, 'retention.occurrence_lock_timeout')[0]).toMatchObject({ code: '55P03' });
		expect(await present(occIds.slice(0, 7))).toBe(7);
		expect(retentionRunOutcome(r!)).toBe('behind');
		await cleanupRetainedData(30, 365, 30, undefined, noSleep);
		expect(await present(occIds)).toBe(0);
		await expectExact(issueIds);
	}, 60_000);

	it('lock wait in the DELETE phase: 55P03 is lock_timeout; a DELETE past its cap is delete_timeout', async () => {
		await drain();
		spies();
		overrides.RETENTION_OCCURRENCE_BATCH = 7;
		const { occIds, issueIds } = await lockFixture();
		const holder = postgres(connectionString, { max: 1, onnotice: () => {} });
		try {
			let r1: Awaited<ReturnType<typeof cleanupRetainedData>> | null = null;
			let r2: Awaited<ReturnType<typeof cleanupRetainedData>> | null = null;
			await holder.begin(async (h) => {
				await h`select occurrence_id from error_search_index where occurrence_id = ${occIds[6]} for update`;
				r1 = await cleanupRetainedData(30, 365, 30, undefined, noSleep);
				overrides.RETENTION_OCCURRENCE_LOCK_TIMEOUT_MS = 5_000;
				overrides.RETENTION_OCCURRENCE_DELETE_TIMEOUT_MS = 300;
				r2 = await cleanupRetainedData(30, 365, 30, undefined, noSleep);
			});
			expect(r1!.occurrenceStopped).toBe('lock_timeout');
			expect(events(warn, 'retention.occurrence_lock_timeout')[0]).toMatchObject({ code: '55P03', phase: 'delete' });
			expect(r2!.occurrenceStopped).toBe('delete_timeout');
			expect(events(warn, 'retention.occurrence_delete_timeout')).toHaveLength(1);
			expect(r2!.occurrenceHeldMaxMs).toBeGreaterThanOrEqual(300);
			expect(r2!.occurrenceHeldMaxMs).toBeLessThan(1_000);
			expect(await present(occIds.slice(0, 7))).toBe(7);
		} finally {
			await holder.end({ timeout: 1 });
		}
		for (const k of Object.keys(overrides)) delete overrides[k];
		await cleanupRetainedData(30, 365, 30, undefined, noSleep);
		expect(await present(occIds)).toBe(0);
		await expectExact(issueIds);
	}, 60_000);

	async function withSlowDeletes<T>(issueId: string, fn: () => Promise<T>): Promise<T> {
		const name = `tf_slow_delete_${suffix}`;
		await sql.unsafe(`CREATE OR REPLACE FUNCTION ${name}() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(0.05); RETURN OLD; END $$`);
		await sql.unsafe(
			`CREATE TRIGGER ${name} BEFORE DELETE ON error_occurrences FOR EACH ROW WHEN (OLD.issue_id = '${issueId}') EXECUTE FUNCTION ${name}()`
		);
		try {
			return await fn();
		} finally {
			await sql.unsafe(`DROP TRIGGER IF EXISTS ${name} ON error_occurrences`);
			await sql.unsafe(`DROP FUNCTION IF EXISTS ${name}()`);
		}
	}

	it('a DELETE timeout halves the batch down to the floor, pausing before each retry', async () => {
		await drain();
		spies();
		const h = await seedIssue();
		const ids = await insertOccs(Array.from({ length: 50 }, (_, i) => ({ issueId: h, createdAt: hoursAgo(30, 1 + i / 100), release: 'slow' })));
		Object.assign(overrides, { RETENTION_OCCURRENCE_BATCH: 7, RETENTION_OCCURRENCE_DELETE_TIMEOUT_MS: 200, RETENTION_OCCURRENCE_BATCH_FLOOR: 2 });
		const sleeps: number[] = [];
		const r = await withSlowDeletes(h, () => cleanupRetainedData(30, 365, 30, undefined, { sleep: async (ms) => void sleeps.push(ms) }));
		expect(await present(ids)).toBe(0);
		expect(r.occurrenceStopped).toBe('drained');
		expect(r.occurrenceBatchSize).toBe(3);
		expect(events(warn, 'retention.occurrence_batch_reduced')).toEqual([expect.objectContaining({ from: 7, to: 3 })]);
		expect(sleeps).toHaveLength(18);
		expect(sleeps[0]).toBeGreaterThanOrEqual(200);
		await expectExact([h]);
	}, 60_000);

	it('at the floor a DELETE timeout stops the step: nothing deleted, one pause', async () => {
		await drain();
		spies();
		const h = await seedIssue();
		const ids = await insertOccs(Array.from({ length: 50 }, (_, i) => ({ issueId: h, createdAt: hoursAgo(30, 1 + i / 100), release: 'slow' })));
		Object.assign(overrides, { RETENTION_OCCURRENCE_BATCH: 7, RETENTION_OCCURRENCE_DELETE_TIMEOUT_MS: 200, RETENTION_OCCURRENCE_BATCH_FLOOR: 5 });
		const sleeps: number[] = [];
		const r = await withSlowDeletes(h, () => cleanupRetainedData(30, 365, 30, undefined, { sleep: async (ms) => void sleeps.push(ms) }));
		expect(await present(ids)).toBe(50);
		expect(r.occurrenceStopped).toBe('delete_timeout');
		expect(events(warn, 'retention.occurrence_batch_reduced')).toEqual([expect.objectContaining({ from: 7, to: 5 })]);
		expect(events(warn, 'retention.occurrence_delete_timeout')).toEqual([expect.objectContaining({ batchSize: 5 })]);
		expect(sleeps).toHaveLength(1);
		expect(sleeps[0]).toBeGreaterThanOrEqual(200);
		for (const k of Object.keys(overrides)) delete overrides[k];
		await cleanupRetainedData(30, 365, 30, undefined, noSleep);
		expect(await present(ids)).toBe(0);
		await expectExact([h]);
	}, 60_000);

	it('no cap below the minimum: with < 100 ms of the step left after the lock phase no DELETE is sent', async () => {
		for (const advance of [1_950, 2_000]) {
			await drain();
			overrides.RETENTION_OCCURRENCE_BUDGET_MS = 2_000;
			overrides.RETENTION_OCCURRENCE_BATCH = 7;
			const issue = await seedIssue();
			const ids = await insertOccs(Array.from({ length: 50 }, () => ({ issueId: issue, createdAt: hoursAgo(30, 2), release: 'cap' })));
			let offset = 0;
			const statements: string[] = [];
			let lost = false;
			const client = openRetentionRunClient(() => {
				lost = true;
			});
			const spied = Object.assign((s: TemplateStringsArray, ...v: unknown[]) => client(s, ...(v as never[])), {
				unsafe: (q: string, p?: unknown[]) => {
					statements.push(q);
					if (/FROM issues WHERE id = ANY/i.test(q) && /FOR UPDATE/i.test(q)) offset += advance;
					return client.unsafe(q, p as never);
				},
				options: client.options,
			});
			try {
				const run = openRetentionRun(spied as never, { deadlineMs: 170_000, isLost: () => lost });
				const r = await cleanupRetainedData(30, 365, 30, run, { ...noSleep, now: () => performance.now() + offset });
				expect(r.occurrenceStopped).toBe('budget');
			} finally {
				await client.end({ timeout: 1 });
			}
			expect(statements.filter((q) => /DELETE FROM error_occurrences/i.test(q))).toEqual([]);
			expect(await present(ids)).toBe(50);
			for (const k of Object.keys(overrides)) delete overrides[k];
			await cleanupRetainedData(30, 365, 30, undefined, noSleep);
			expect(await present(ids)).toBe(0);
			await expectExact([issue]);
		}
	}, 90_000);

	it('per-batch lock scope: a batch locks only its own issues', async () => {
		await drain();
		overrides.RETENTION_OCCURRENCE_BATCH = 7;
		const { id, issueIds } = await lockFixture();
		const holder = postgres(connectionString, { max: 1, onnotice: () => {} });
		try {
			await holder.begin(async (h) => {
				await h`select id from issues where id = ${id(16)} for update`;
				const run = cleanupRetainedData(30, 365, 30, undefined, noSleep);
				await pollUntil('the batch to wait on ...16', async () => {
					const rows = await sql`
						select 1 from pg_stat_activity
						 where application_name = 'sentinel.retention.run' and wait_event_type = 'Lock'`;
					return rows.length > 0;
				});
				const probe = postgres(connectionString, { max: 1, onnotice: () => {} });
				try {
					await probe.begin(async (p) => {
						await p`select id from issues where id = ${id(1)} for update nowait`;
					});
				} finally {
					await probe.end({ timeout: 1 });
				}
				await run.catch(() => {});
			});
		} finally {
			await holder.end({ timeout: 1 });
		}
		await cleanupRetainedData(30, 365, 30, undefined, noSleep);
		await expectExact(issueIds);
	}, 60_000);

	// ---- the route: one run at a time, stale lock, deadline, terminate ----
	it('one run at a time: a held route lock -> 200 skipped; after unlock the POST deletes; nothing left behind', async () => {
		await drain();
		spies();
		const issue = await seedIssue();
		const ids = await insertOccs(Array.from({ length: 5 }, () => ({ issueId: issue, createdAt: hoursAgo(30, 2), release: 'route' })));
		const holder = postgres(connectionString, { max: 1, onnotice: () => {} });
		try {
			const [{ pid }] = await holder`select pg_backend_pid() as pid, pg_advisory_lock(hashtext('sentinel.retention.route'))`;
			const res = await cronPost();
			expect(res.status).toBe(200);
			expect(await res.json()).toMatchObject({ success: true, skipped: 'already_running' });
			expect(events(info, 'retention_cron.skipped_concurrent')[0]).toMatchObject({ holderPid: pid });
			expect(await present(ids)).toBe(5);
			await holder`select pg_advisory_unlock(hashtext('sentinel.retention.route'))`;
		} finally {
			await holder.end({ timeout: 1 });
		}
		const res2 = await cronPost();
		expect(res2.status).toBe(200);
		expect(await present(ids)).toBe(0);
		await pollUntil('the run lock and backend to be gone', async () => !(await routeLockHeld()) && (await runBackends()).length === 0, 1_000);
		await expectExact([issue]);
	}, 60_000);

	it('stale lock: a holder older than the stale bound -> 503 until it is terminated', async () => {
		await drain();
		spies();
		overrides.RETENTION_ROUTE_LOCK_STALE_MS = 1_000;
		const issue = await seedIssue();
		const ids = await insertOccs(Array.from({ length: 5 }, () => ({ issueId: issue, createdAt: hoursAgo(30, 2), release: 'stale' })));
		const holder = postgres(connectionString, { max: 1, onnotice: () => {} });
		const [{ pid }] = await holder`select pg_backend_pid() as pid, pg_advisory_lock(hashtext('sentinel.retention.route'))`;
		try {
			const res = await cronPost();
			expect(res.status).toBe(200);
			await new Promise((r) => setTimeout(r, 1_200));
			await holder`select 1`; // its state_change is now fresh; the age is its backend_start
			const res2 = await cronPost();
			expect(res2.status).toBe(503);
			expect(await res2.json()).toMatchObject({ error: 'retention_lock_stale', holderPid: pid });
			const stale = events(error, 'retention_cron.lock_stale')[0];
			expect(stale).toMatchObject({ holderPid: pid });
			expect(Number(stale.heldMs)).toBeGreaterThanOrEqual(1_000);
			expect(await present(ids)).toBe(5);
			await sql`select pg_terminate_backend(${pid})`;
		} finally {
			await holder.end({ timeout: 1 }).catch(() => {});
		}
		await pollUntil('the terminated holder to release the lock', async () => !(await routeLockHeld()), 5_000);
		const res3 = await cronPost();
		expect(res3.status).toBe(200);
		expect(await present(ids)).toBe(0);
	}, 60_000);

	it('a run cannot outlive its deadline, with a blocker present; runs do not pile up behind it', async () => {
		await drain();
		spies();
		overrides.RETENTION_ROUTE_DEADLINE_MS = 3_000;
		const b = await seedIssue();
		await sql`insert into issue_affected_users (issue_id, user_id, first_seen, last_seen)
		          values (${b}, 'aged-user', ${hoursAgo(31, 0)}, ${hoursAgo(31, 0)})`;
		const blocker = postgres(connectionString, { max: 1, onnotice: () => {} });
		const [{ pid: blockerPid }] = await blocker`select pg_backend_pid() as pid`;
		await blocker`begin`;
		await blocker`select id from issues where id = ${b} for update`;
		try {
			for (let round = 0; round < 2; round++) {
				const started = Date.now();
				const post1 = cronPost();
				let waiting = 0;
				await pollUntil('a run backend to wait on the blocker', async () => {
					const w = (await runBackends()).find((r) => r.blocking.includes(blockerPid));
					waiting = w?.pid ?? 0;
					return waiting !== 0;
				});
				if (round === 0) {
					const res2 = await cronPost();
					expect(res2.status).toBe(200);
					expect(await res2.json()).toMatchObject({ skipped: 'already_running' });
					expect(events(info, 'retention_cron.skipped_concurrent').at(-1)).toMatchObject({ holderPid: waiting });
				}
				const res1 = await Promise.race([
					post1,
					new Promise<never>((_, rej) => setTimeout(() => rej(new Error('safety timeout')), 15_000)),
				]);
				expect(Date.now() - started).toBeLessThanOrEqual(4_000 + 1_000);
				expect(res1.status).toBe(500);
				expect(await res1.json()).toEqual({ error: 'retention_deadline_exceeded' });
				expect(events(error, 'retention_cron.deadline_exceeded').length).toBeGreaterThanOrEqual(round + 1);
				await pollUntil('nothing of the run left', async () => {
					const backs = await sql`select 1 from pg_stat_activity where ${blockerPid} = any(pg_blocking_pids(pid))`;
					return !(await routeLockHeld()) && backs.length === 0 && (await runBackends()).length === 0;
				}, 2_000);
			}
		} finally {
			await blocker`commit`.catch(() => {});
			await blocker.end({ timeout: 1 });
		}
		delete overrides.RETENTION_ROUTE_DEADLINE_MS;
		const res4 = await cronPost();
		expect(res4.status).toBe(200);
		const [{ n }] = await sql`select count(*)::int as n from issue_affected_users where issue_id = ${b}`;
		expect(n).toBe(0);
		await expectExact([b]);
	}, 60_000);

	it('holder terminated: the run ends with connection_lost, never reconnects, nothing left', async () => {
		await drain();
		spies();
		const b = await seedIssue();
		await sql`insert into issue_affected_users (issue_id, user_id, first_seen, last_seen)
		          values (${b}, 'aged-user', ${hoursAgo(31, 0)}, ${hoursAgo(31, 0)})`;
		const blocker = postgres(connectionString, { max: 1, onnotice: () => {} });
		const [{ pid: blockerPid }] = await blocker`select pg_backend_pid() as pid`;
		await blocker`begin`;
		await blocker`select id from issues where id = ${b} for update`;
		try {
			const post1 = cronPost();
			let waiting = 0;
			await pollUntil('a run backend to wait on the blocker', async () => {
				const w = (await runBackends()).find((r) => r.blocking.includes(blockerPid));
				waiting = w?.pid ?? 0;
				return waiting !== 0;
			});
			const t0 = Date.now();
			await sql`select pg_terminate_backend(${waiting})`;
			const res = await post1;
			expect(Date.now() - t0).toBeLessThanOrEqual(2_000);
			expect(res.status).toBe(500);
			expect(await res.json()).toEqual({ error: 'retention_connection_lost' });
			expect(events(error, 'retention_cron.connection_lost')).toHaveLength(1);
			await new Promise((r) => setTimeout(r, 200));
			expect(await routeLockHeld()).toBe(false);
			expect(await runBackends()).toEqual([]);
		} finally {
			await blocker`commit`.catch(() => {});
			await blocker.end({ timeout: 1 });
		}
	}, 60_000);

	// ---- drift is repaired (dashboard half) ----
	async function driftFixture(n: number): Promise<string> {
		const issue = await seedIssue();
		await insertOccs(Array.from({ length: n }, () => ({ issueId: issue, createdAt: new Date(), release: 'd' })));
		return issue;
	}
	async function pendingDrift(issueId: string) {
		await sql`insert into issue_tag_values_drift (issue_id, contributions, source) values (${issueId}, 1, 'delete')`;
	}
	async function driftState(issueId: string) {
		return (await sql`
			select rebuilt_at is not null as rebuilt, operator_only_at is not null as operator_only, reported_at is not null as reported
			  from issue_tag_values_drift where issue_id = ${issueId} order by id`) as unknown as { rebuilt: boolean; operator_only: boolean; reported: boolean }[];
	}

	it('drift report: an unreported drift row is logged once, counted and marked reported', async () => {
		await drain();
		spies();
		const issue = await driftFixture(3);
		await pendingDrift(issue);
		const r = await cleanupRetainedData(30, 365, 30, undefined, noSleep);
		const ev = events(warn, 'retention.tag_rollup_drift')[0];
		expect(Number(ev.events)).toBeGreaterThanOrEqual(1);
		expect(ev.issueIds).toContain(issue);
		expect(r.tagRollupDriftEvents).toBeGreaterThanOrEqual(1);
		expect((await driftState(issue))[0].reported).toBe(true);
	}, 60_000);

	it('deferred rebuild: a pending drift row is rebuilt by the route; with a 0 ms budget it goes to the operator', async () => {
		await drain();
		spies();
		const issue = await driftFixture(5);
		await sql`delete from issue_tag_values where issue_id = ${issue} and key = 'release'`; // the drift
		await pendingDrift(issue);
		await cleanupRetainedData(30, 365, 30, undefined, noSleep);
		expect(events(info, 'retention.tag_rollup_rebuilt')[0]?.issueIds).toContain(issue);
		expect((await driftState(issue))[0].rebuilt).toBe(true);
		await expectExact([issue]);

		const other = await driftFixture(5);
		await pendingDrift(other);
		overrides.RETENTION_TAG_REBUILD_BUDGET_MS = 0;
		const r = await cleanupRetainedData(30, 365, 30, undefined, noSleep);
		const st = await driftState(other);
		expect(st[0]).toMatchObject({ rebuilt: false, operator_only: true });
		expect(events(warn, 'retention.tag_rollup_rebuild_pending').at(-1)?.operatorOnlyIssueIds).toContain(other);
		expect(r.tagRollupRebuildsPending).toBe(1);
		expect(r.tagRollupRebuildsOperatorOnly).toBe(1);
	}, 60_000);

	it('head of line: size before lock, skip instead of stop, never retry what cannot finish', async () => {
		await drain();
		spies();
		Object.assign(overrides, {
			RETENTION_TAG_REBUILD_BUDGET_MS: 450,
			RETENTION_TAG_REBUILD_FIXED_MS: 100,
			RETENTION_TAG_REBUILD_US_PER_OCCURRENCE: 1_000,
		}); // capFull = 350
		const L = await driftFixture(400);
		const A = await driftFixture(5);
		const B = await driftFixture(349); // est 449 ms
		const C = await driftFixture(5);
		for (const i of [L, A, B, C]) {
			await pendingDrift(i);
			await new Promise((r) => setTimeout(r, 5)); // detected_at order: L, A, B, C
		}
		const holder = postgres(connectionString, { max: 1, onnotice: () => {} });
		try {
			// Run 1, a holder on L throughout: L is never locked, A rebuilt, B skipped, C rebuilt.
			await holder.begin(async (h) => {
				await h`select id from issues where id = ${L} for update`;
				await Promise.race([
					cleanupRetainedData(30, 365, 30, undefined, noSleep),
					new Promise<never>((_, rej) => setTimeout(() => rej(new Error('run 1 waited on the holder of L')), 10_000)),
				]);
				expect((await driftState(L))[0]).toMatchObject({ operator_only: true, rebuilt: false });
				expect(events(warn, 'retention.tag_rollup_rebuild_operator_only')).toContainEqual(expect.objectContaining({ issueId: L, reason: 'too_large' }));
				expect((await driftState(A))[0].rebuilt).toBe(true);
				expect((await driftState(B))[0]).toMatchObject({ rebuilt: false, operator_only: false });
				expect((await driftState(C))[0].rebuilt).toBe(true);

				// Run 2: B is the first attempt, gets the whole budget, and is rebuilt.
				await cleanupRetainedData(30, 365, 30, undefined, noSleep);
				expect((await driftState(B))[0].rebuilt).toBe(true);

				// Re-drift of L while it is operator-only: not probed, not attempted, still named.
				await pendingDrift(L);
				warn.mockClear();
				await Promise.race([
					cleanupRetainedData(30, 365, 30, undefined, noSleep),
					new Promise<never>((_, rej) => setTimeout(() => rej(new Error('the re-drift run waited on L')), 10_000)),
				]);
				expect(events(warn, 'retention.tag_rollup_rebuild_pending').at(-1)?.operatorOnlyIssueIds).toContain(L);
				expect(events(warn, 'retention.tag_rollup_rebuild_operator_only').filter((e) => e.issueId === L)).toEqual([]);
			});
		} finally {
			await holder.end({ timeout: 1 });
		}
		// The operator rebuilds L: every pending row of it gets rebuilt_at, and the event stops naming it.
		await sql`select issue_tag_values_rebuild(${L}::uuid)`;
		expect((await driftState(L)).every((s) => s.rebuilt)).toBe(true);
		warn.mockClear();
		await cleanupRetainedData(30, 365, 30, undefined, noSleep);
		const last = events(warn, 'retention.tag_rollup_rebuild_pending').at(-1);
		expect(last?.operatorOnlyIssueIds ?? []).not.toContain(L);
		await expectExact([L, A, B, C]);
	}, 90_000);

	it('timeout: an attempt that times out goes to the operator and is never attempted again', async () => {
		await drain();
		spies();
		Object.assign(overrides, {
			RETENTION_TAG_REBUILD_BUDGET_MS: 450,
			RETENTION_TAG_REBUILD_FIXED_MS: 100,
			RETENTION_TAG_REBUILD_US_PER_OCCURRENCE: 1_000,
		});
		const A = await driftFixture(5);
		const C = await driftFixture(5);
		await pendingDrift(A);
		await new Promise((r) => setTimeout(r, 5));
		await pendingDrift(C);
		const holder = postgres(connectionString, { max: 1, onnotice: () => {} });
		try {
			await holder.begin(async (h) => {
				await h`select id from issues where id = ${A} for update`;
				await cleanupRetainedData(30, 365, 30, undefined, noSleep);
				expect((await driftState(A))[0]).toMatchObject({ operator_only: true, rebuilt: false });
				expect(events(warn, 'retention.tag_rollup_rebuild_operator_only')).toContainEqual(expect.objectContaining({ issueId: A, reason: 'timeout' }));
				await expectExact([A]);
				// The next run, the holder still held: completes without waiting and rebuilds C.
				await Promise.race([
					cleanupRetainedData(30, 365, 30, undefined, noSleep),
					new Promise<never>((_, rej) => setTimeout(() => rej(new Error('the next run waited on A')), 10_000)),
				]);
				expect((await driftState(C))[0].rebuilt).toBe(true);
			});
		} finally {
			await holder.end({ timeout: 1 });
		}
	}, 60_000);

	it('probe budget: before the first attempt, probes stop after the probe budget', async () => {
		await drain();
		Object.assign(overrides, {
			RETENTION_TAG_REBUILD_BUDGET_MS: 450,
			RETENTION_TAG_REBUILD_FIXED_MS: 100,
			RETENTION_TAG_REBUILD_US_PER_OCCURRENCE: 1_000,
			RETENTION_TAG_REBUILD_PROBE_BUDGET_MS: 0,
		});
		const P = [await driftFixture(400), await driftFixture(400), await driftFixture(400)];
		for (const p of P) {
			await pendingDrift(p);
			await new Promise((r) => setTimeout(r, 5));
		}
		for (let run = 0; run < 3; run++) {
			await cleanupRetainedData(30, 365, 30, undefined, noSleep);
			for (let i = 0; i < 3; i++) {
				expect((await driftState(P[i]))[0].operator_only).toBe(i <= run);
			}
		}
	}, 60_000);

	it('probe timeout: a probe that cannot count to the cap in 1 s marks the issue operator-only', async () => {
		await drain();
		spies();
		const issue = await driftFixture(3);
		await pendingDrift(issue);
		const holder = postgres(connectionString, { max: 1, onnotice: () => {} });
		let lost = false;
		const client = openRetentionRunClient(() => {
			lost = true;
		});
		try {
			await holder.begin(async (h) => {
				await h`lock table error_occurrences in access exclusive mode`;
				// rebuildPendingTagRollups called directly (spec §10.2): the holder commits only after it returns.
				const run = openRetentionRun(client as never, { deadlineMs: 170_000, isLost: () => lost });
				await Promise.race([
					rebuildPendingTagRollups(run),
					new Promise<never>((_, rej) => setTimeout(() => rej(new Error('safety timeout: the probe waited for the holder')), 10_000)),
				]);
			});
		} finally {
			await client.end({ timeout: 1 });
			await holder.end({ timeout: 1 });
		}
		expect(events(warn, 'retention.tag_rollup_rebuild_operator_only')).toContainEqual(
			expect.objectContaining({ issueId: issue, reason: 'probe_timeout' })
		);
		expect((await driftState(issue))[0]).toMatchObject({ operator_only: true, rebuilt: false });
	}, 60_000);
});
