import { describe, it, expect, beforeAll, afterAll, afterEach, vi } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';
import { getTableName, type SQL } from 'drizzle-orm';
import { db, type Tx } from '$lib/server/db';

// Since sentinel-issue-tag-facets (#55, dde0ea2) the sweep does not use the module singleton at all:
// `cleanupRetainedData` opens a retention run on its own per-run client and sends every statement
// through `run.db` (§5 "One run, one connection, one deadline"). A seam on the singleton's methods
// therefore recorded nothing -- which is how this file went red on every push to main from that
// commit on. The seam now wraps each run's `db` as `openRetentionRun` hands it out: the real
// function, called with the real arguments, on the real per-run client; only its methods are
// wrapped while a seam is installed.
const runSeam = vi.hoisted(() => ({ onOpen: null as ((runDb: never) => void) | null }));
vi.mock('$lib/server/retention-run', async (importOriginal) => {
	const actual = await importOriginal<typeof import('$lib/server/retention-run')>();
	return {
		...actual,
		openRetentionRun: (...args: Parameters<typeof actual.openRetentionRun>) => {
			const run = actual.openRetentionRun(...args);
			runSeam.onOpen?.(run.db as never);
			return run;
		},
	};
});

/**
 * sentinel-issue-trend §4.5 (flows R1–R3; decision 4) -- contract §11's `cleanupRetainedData`
 * boundary for the hourly bucket rollup, against a REAL migrated Postgres, because every claim
 * below is a property of live SQL: that bucket rows with `bucket_start < cutoff` go in ONE
 * autocommit statement (the straddling bucket -- the hour the cutoff falls in -- removed, its
 * successor kept), that rows `>= cutoff` survive, that an issue the EXISTING orphan rule deletes
 * loses its buckets by `ON DELETE CASCADE` and not by any statement of this feature's, that
 * `deletedOccurrenceBuckets` is the number of bucket ROWS the delete removed, and that the
 * statement sits after both side-table sweeps and before the orphan candidate select, in no
 * transaction and taking no `FOR UPDATE` on `issues`. A mock-chained Drizzle test can prove none
 * of it -- only the sweep can prove the sweep.
 *
 * The ORDER claim needs to see every statement `cleanupRetainedData` issues, not only the ones
 * inside a transaction, so this file's seam is retention.affected.integration.test.ts's (wrap the
 * module singleton's `db.transaction`, record each `tx.execute`) extended to the top-level
 * `db.execute`, `db.select(...).from(table)` and `db.delete(table)` calls, recorded in call order
 * with a stopwatch on each. Nothing in production code knows about it; `afterEach` restores the
 * real methods.
 *
 * retention.affected.integration.test.ts's style: seeds its OWN organization/project/issues under a
 * per-run suffix and deletes them again, so nothing here depends on ambient data. The sweep itself
 * is database-wide (it is the real one), so the "rows removed" assertion is taken against the
 * whole table's aged rows as they stood immediately before the call, and the survival assertions
 * are scoped to this file's issues.
 *
 * Point DATABASE_URL at a DISPOSABLE database -- never the shared dev one (CLAUDE.md):
 *
 *   docker run -d --name sentinel-trend-pg -p 15542:5432 \
 *     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && GOWORK=off \
 *     DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15542/sentinel?sslmode=disable \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15542/sentinel \
 *     BUCKETS_RETENTION_INTEGRATION_REQUIRED=1 \
 *     pnpm vitest run src/lib/server/retention.buckets.integration.test.ts
 *
 * Skipped when nothing answers, so a machine with no Postgres is not broken by it. Set
 * SCHEMA_DRIFT_REQUIRED=1 (the switch CI's dashboard job sets for the DB-backed suites) or
 * BUCKETS_RETENTION_INTEGRATION_REQUIRED=1 to make "no reachable database" a hard failure, so the
 * only proof that §4.5's bucket step holds cannot be skipped silently.
 *
 * The SECOND describe block is §13 M4 -- the LABELLED FALLBACK take of the same statement on the
 * committed seed (`scripts/issue-trend-seed.sql`, ~50 issues with pre-cutoff bucket rows), timed in
 * the test that drives it, with the DELETE's `EXPLAIN (ANALYZE, BUFFERS)` pasted and one line
 * stating whether `idx_issue_occurrence_buckets_bucket_start` is used. Evidence about plan shape on
 * a local disposable Postgres; never the criterion met (decision 15), and never taken on a rig
 * (the sweep is destructive on shared data). It is opt-in and destructive by design:
 * BUCKETS_M4_MEASURE=1, DATABASE_URL at a disposable database with the seed loaded IMMEDIATELY
 * before (the seed is now()-relative), and DATA_RETENTION_DAYS=30 (§13's data condition). Without
 * the switch it skips; with the switch and no seed it FAILS, so a measurement of an empty table
 * cannot be mistaken for the measurement:
 *
 *   psql postgres://sentinel:changeme@localhost:15542/sentinel -f apps/dashboard-web/scripts/issue-trend-seed.sql
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15542/sentinel \
 *     DATA_RETENTION_DAYS=30 BUCKETS_M4_MEASURE=1 \
 *     pnpm vitest run src/lib/server/retention.buckets.integration.test.ts
 *
 * Re-running M4 needs the seed re-loaded: the first run swept it. Under BUCKETS_M4_MEASURE=1 the
 * R1–R3 block is skipped (its own sweep would consume the seed first); run the two configurations
 * separately.
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required =
	process.env.SCHEMA_DRIFT_REQUIRED === '1' ||
	process.env.BUCKETS_RETENTION_INTEGRATION_REQUIRED === '1';
const measure = process.env.BUCKETS_M4_MEASURE === '1';

let dbReachable = false;
const probeSql = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	await probeSql`select 1`;
	dbReachable = true;
} catch {
	dbReachable = false;
} finally {
	await probeSql.end({ timeout: 1 }).catch(() => {});
}

if (required && !dbReachable) {
	throw new Error(
		`SCHEMA_DRIFT_REQUIRED=1 / BUCKETS_RETENTION_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}. ` +
			'This test is the only coverage that runs the §4.5 bucket sweep against real Postgres; ' +
			'skipping it silently would leave R1-R3 unguarded.'
	);
}
if (measure && !dbReachable) {
	throw new Error(
		`BUCKETS_M4_MEASURE=1 but no database answered at ${connectionString}. ` +
			'M4 is a measurement on the committed seed; it cannot be taken without one.'
	);
}

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();

// The seed's fixed ids (issue-trend-seed.sql's header).
const SEED_ORG_ID = '00000000-0000-4000-8000-0000000000b1';
const SEED_PROJECT_IDS = [
	'00000000-0000-4000-8000-0000000000b2',
	'00000000-0000-4000-8000-0000000000b3',
];

// The retention.*.integration family runs `cleanupRetainedData` / `reapStaleClaims`, which sweep
// the WHOLE database, and vitest runs files in parallel workers -- so one file's sweep can delete
// another file's fixture between its seed and its own sweep. The family serialises itself on one
// session-level advisory lock, taken in `beforeAll` and released in `afterAll`; the key is the
// same in every member.
const RETENTION_FAMILY_LOCK = 'sentinel.retention.integration';

const RETENTION_DAYS = 30;

// The cutoff exactly as cleanupRetainedData computes it: setDate(-N) on now. Taken immediately
// before each call, so the milliseconds between the two cannot move an hour-aligned row across it
// (the straddling bucket below is the hour the cutoff falls IN, so it is < both instants; its
// successor is the next hour, so it is > both unless the two instants straddle an hour boundary).
function cutoffNow(): Date {
	const date = new Date();
	date.setDate(date.getDate() - RETENTION_DAYS);
	return date;
}

function floorToHour(date: Date): Date {
	const floored = new Date(date);
	floored.setUTCMinutes(0, 0, 0);
	return floored;
}

function hoursFrom(date: Date, h: number): Date {
	return new Date(date.getTime() + h * 3_600_000);
}

function daysAgo(d: number): Date {
	const date = new Date();
	date.setDate(date.getDate() - d);
	return date;
}

// ---------------------------------------------------------------------------------------------
// The test-side seam: record, in call order and with a stopwatch, every statement the sweep issues
// through the module singleton -- `tx.execute` inside each `db.transaction` (grouped per
// transaction, as in retention.affected.integration.test.ts), and the top-level `db.execute`,
// `db.select(...).from(table)` and `db.delete(table)` calls around them. The record IS
// cleanupRetainedData's statement order.
// ---------------------------------------------------------------------------------------------
type Recorded =
	| { kind: 'transaction'; statements: string[]; ms: number }
	| { kind: 'execute'; sql: string; ms: number }
	| { kind: 'select'; table: string }
	| { kind: 'delete'; table: string };

type TransactionMethod = typeof db.transaction;
type ExecuteMethod = typeof db.execute;
type SelectMethod = typeof db.select;
type DeleteMethod = typeof db.delete;

let recorded: Recorded[] = [];

function renderSql(session: Tx | typeof db, query: SQL): string {
	// `dialect` is `@internal` on PgDatabase, so it is absent from the types but present at runtime.
	const { dialect } = session as unknown as { dialect: { sqlToQuery(sql: SQL): { sql: string } } };
	return dialect.sqlToQuery(query).sql.replace(/\s+/g, ' ').trim();
}

function tableNameOf(source: unknown): string {
	try {
		return getTableName(source as Parameters<typeof getTableName>[0]);
	} catch {
		return '(subquery)';
	}
}

function wrapRunDb(target: typeof db): void {
	const realTransaction: TransactionMethod = target.transaction.bind(target);
	const realExecute: ExecuteMethod = target.execute.bind(target);
	const realSelect: SelectMethod = target.select.bind(target);
	const realDelete: DeleteMethod = target.delete.bind(target);
	const transaction: TransactionMethod = (callback, config) => {
		const started = performance.now();
		const record: Recorded = { kind: 'transaction', statements: [], ms: 0 };
		recorded.push(record);
		return realTransaction(async (tx) => {
			const realTxExecute = tx.execute.bind(tx);
			const execute = (async (query: SQL) => {
				const result = await realTxExecute(query);
				record.statements.push(renderSql(tx, query));
				return result;
			}) as unknown as typeof tx.execute;
			(tx as { execute: typeof tx.execute }).execute = execute;
			return callback(tx);
		}, config).finally(() => {
			record.ms = performance.now() - started;
		});
	};
	const execute = (async (query: SQL) => {
		const record: Recorded = { kind: 'execute', sql: renderSql(target, query), ms: 0 };
		recorded.push(record);
		const t0 = performance.now();
		const result = await realExecute(query);
		record.ms = performance.now() - t0;
		return result;
	}) as unknown as ExecuteMethod;
	const select = ((...args: unknown[]) => {
		const builder = (realSelect as unknown as (...a: unknown[]) => { from: (s: unknown) => unknown })(
			...args
		);
		const realFrom = builder.from.bind(builder);
		builder.from = (source: unknown) => {
			recorded.push({ kind: 'select', table: tableNameOf(source) });
			return realFrom(source);
		};
		return builder;
	}) as unknown as SelectMethod;
	const del = ((table: unknown) => {
		recorded.push({ kind: 'delete', table: tableNameOf(table) });
		return (realDelete as unknown as (t: unknown) => unknown)(table);
	}) as unknown as DeleteMethod;

	const methods = target as {
		transaction: TransactionMethod;
		execute: ExecuteMethod;
		select: SelectMethod;
		delete: DeleteMethod;
	};
	methods.transaction = transaction;
	methods.execute = execute;
	methods.select = select;
	methods.delete = del;
}

// Applies to every run opened until removeSeam(); each run's client is fresh, so nothing to restore.
function installSeam(): void {
	runSeam.onOpen = wrapRunDb;
}

function removeSeam(): void {
	runSeam.onOpen = null;
	recorded = [];
}

const BUCKET_DELETE_SQL =
	/^delete from "issue_occurrence_buckets" where "issue_occurrence_buckets"\."bucket_start" < \$1::timestamptz returning issue_id$/i;

function describeRecord(r: Recorded): string {
	switch (r.kind) {
		case 'transaction':
			return `transaction[${r.statements.map((s) => s.split(' ')[0]).join(',')}]`;
		case 'execute':
			return `execute(${r.sql.slice(0, 40)}…)`;
		case 'select':
			return `select.from(${r.table})`;
		case 'delete':
			return `delete(${r.table})`;
	}
}

// sentinel-issue-tag-facets (#55) added two steps of its own to the sweep, both database-wide and both
// dependent on what other files left behind: the paced occurrence delete, FIRST, whose batches are
// transactions taking `FOR UPDATE` on `issues` (the lock, then the DELETE), and the deferred tag-rollup
// rebuild, LAST -- after the `next` catch-up, deliberately (retention.ts: "LAST, after the orphan sweep
// and the catch-up") -- which reads the drift queue and then, per drifted issue, runs a lock-free size
// probe and the rebuild, each in its own transaction. Neither is this file's subject. `sweepRecords`
// checks both by their exact statements and sets them aside, so the assertions below are about the
// same records they were written against: the catch-up is the last statement BEFORE the rebuild step.
const OCCURRENCE_BATCH_SQL = [
	/^select id from issues where id = any\(\$1::uuid\[\]\) order by id for update$/i,
	/^delete from error_occurrences where id = any\(\$1::uuid\[\]\) and created_at < \$2::timestamptz returning id$/i,
];
const TAG_REBUILD_QUEUE_SQL = /^select issue_id from issue_tag_values_drift where rebuilt_at is null /i;
const TAG_REBUILD_SQL =
	/^select (count\(\*\)::int as n from \( ?select 1 from error_occurrences where issue_id = \$1::uuid limit \$2\) s|issue_tag_values_rebuild\(\$1::uuid\))$/i;

function isOccurrenceBatch(r: Recorded): boolean {
	return r.kind === 'transaction' && r.statements.length > 0 && OCCURRENCE_BATCH_SQL[0].test(r.statements[0]);
}

function sweepRecords(all: Recorded[]): Recorded[] {
	const order = all.map(describeRecord).join(' → ');
	const rebuildAt = all.findIndex((r) => r.kind === 'execute' && TAG_REBUILD_QUEUE_SQL.test(r.sql));
	expect(rebuildAt, `tag-rollup rebuild step not found in: ${order}`).toBeGreaterThan(-1);
	for (const r of all.slice(rebuildAt)) {
		if (r.kind === 'execute') expect(r.sql, order).toContain('issue_tag_values_drift');
		else if (r.kind === 'transaction') {
			expect(r.statements, order).toHaveLength(1);
			expect(r.statements[0], order).toMatch(TAG_REBUILD_SQL);
		} else expect.fail(`unexpected ${describeRecord(r)} in the rebuild step: ${order}`);
	}
	const body = all.slice(0, rebuildAt);
	const firstOtherTransaction = body.findIndex((r) => r.kind === 'transaction' && !isOccurrenceBatch(r));
	body.forEach((r, i) => {
		if (isOccurrenceBatch(r) && firstOtherTransaction >= 0) expect(i, order).toBeLessThan(firstOtherTransaction);
	});
	for (const r of body.filter(isOccurrenceBatch)) {
		expect((r as Extract<Recorded, { kind: 'transaction' }>).statements[1], order).toMatch(OCCURRENCE_BATCH_SQL[1]);
	}
	return body.filter((r) => !isOccurrenceBatch(r));
}

const round = (ms: number) => Math.round(ms * 1000) / 1000;

// Skipped under BUCKETS_M4_MEASURE=1 as well: describe blocks run in file order, and this block's
// own sweep would consume the seed's aged rows before M4 below could measure them.
describe.skipIf(!dbReachable || measure)('retention bucket sweep (sentinel-issue-trend §4.5, integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		// Family mutex (see RETENTION_FAMILY_LOCK): blocks until no sibling file is mid-sweep.
		await sql`select pg_advisory_lock(hashtext(${RETENTION_FAMILY_LOCK}))`;
		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'buckets-retention-it-' + suffix}, ${'buckets-retention-it-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'buckets-retention-it-' + suffix}, ${orgId},
			        ${'k_' + suffix}, ${'h_' + suffix})
		`;
	}, 120_000);

	afterEach(() => {
		removeSeam();
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issue_tombstones where organization_id = ${orgId}`;
			// The bucket rows and error_occurrences cascade from issues.
			await sql`delete from issues where project_id = ${projectId}`;
			await sql`delete from projects where id = ${projectId}`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql`select pg_advisory_unlock_all()`.catch(() => {});
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	interface SeedIssue {
		fingerprint: string;
		firstSeen: Date;
		// `null` = no occurrence row at all (the orphan rule's precondition).
		occurrenceAt: Date | null;
		buckets: Array<{ start: Date; count: number }>;
	}

	async function seedIssue(seed: SeedIssue): Promise<string> {
		const id = randomUUID();
		await sql`
			insert into issues (
				id, project_id, environment, fingerprint, message, error_class, status, issue_type,
				first_seen, last_seen
			) values (
				${id}, ${projectId}, ${'production'}, ${seed.fingerprint + '-' + suffix},
				${'bucket seed ' + seed.fingerprint}, ${'SeedError'}, ${'unresolved'}, ${'system_error'},
				${seed.firstSeen}, ${seed.occurrenceAt ?? seed.firstSeen}
			)
		`;
		if (seed.occurrenceAt) {
			await sql`
				insert into error_occurrences (issue_id, environment, platform, created_at)
				values (${id}, ${'production'}, ${'go'}, ${seed.occurrenceAt})
			`;
		}
		for (const bucket of seed.buckets) {
			await sql`
				insert into issue_occurrence_buckets (issue_id, bucket_start, count)
				values (${id}, ${bucket.start}, ${bucket.count})
			`;
		}
		return id;
	}

	async function remainingBucketStarts(issueId: string): Promise<string[]> {
		const rows = await sql<{ bucket_start: Date }[]>`
			select bucket_start from issue_occurrence_buckets where issue_id = ${issueId} order by bucket_start
		`;
		return rows.map((r) => r.bucket_start.toISOString());
	}

	async function agedBucketRows(cutoff: Date): Promise<number> {
		const [row] = await sql<{ n: number }[]>`
			select count(*)::int as n from issue_occurrence_buckets where bucket_start < ${cutoff}
		`;
		return row.n;
	}

	it('R1–R3: deletes every bucket row before the cutoff (the straddling hour included, its successor kept), keeps every row at or after it, lets the orphan rule cascade, reports rows removed, and runs after both side-table sweeps and before the orphan select with no issues lock', async () => {
		const { cleanupRetainedData } = await import('./retention');

		const cutoff = cutoffNow();
		// The straddling bucket: the UTC hour the cutoff instant falls in. Its start is < cutoff
		// (§4.5: removed up to an hour early), and the next hour's start is >= cutoff (kept).
		const straddling = floorToHour(cutoff);
		const successor = hoursFrom(straddling, 1);
		expect(straddling.getTime()).toBeLessThan(cutoff.getTime());
		expect(successor.getTime()).toBeGreaterThan(cutoff.getTime());
		// Taken once, so an hour boundary crossed mid-test cannot move a fixture between the seed
		// and the assertion on what remains.
		const now = new Date();
		const aged40d = floorToHour(daysAgo(40));
		const tenDaysAgo = floorToHour(daysAgo(10));
		const threeHoursAgo = floorToHour(hoursFrom(now, -3));
		const twoHoursAgo = floorToHour(hoursFrom(now, -2));
		const oneHourAgo = floorToHour(hoursFrom(now, -1));

		// A: touched. Two well-aged buckets and the straddling one go; the successor and two fresh
		// ones stay. A fresh occurrence keeps it out of the orphan rule.
		const issueA = await seedIssue({
			fingerprint: 'touched-a',
			firstSeen: daysAgo(60),
			occurrenceAt: hoursFrom(now, -1),
			buckets: [
				{ start: aged40d, count: 3 },
				{ start: hoursFrom(aged40d, 1), count: 1 },
				{ start: straddling, count: 5 },
				{ start: successor, count: 7 },
				{ start: twoHoursAgo, count: 2 },
				{ start: oneHourAgo, count: 4 },
			],
		});
		// B: untouched -- every bucket is inside the window.
		const issueB = await seedIssue({
			fingerprint: 'untouched-b',
			firstSeen: daysAgo(60),
			occurrenceAt: hoursFrom(now, -1),
			buckets: [
				{ start: tenDaysAgo, count: 1 },
				{ start: oneHourAgo, count: 1 },
			],
		});
		// C: orphaned by the EXISTING rule (no occurrences, first_seen older than the cutoff). Its
		// bucket rows are FRESH, so the bucket delete does not touch them and they cannot be in
		// deletedOccurrenceBuckets; only the FK cascade from the orphan delete can remove them (R2).
		const issueC = await seedIssue({
			fingerprint: 'orphan-c',
			firstSeen: daysAgo(60),
			occurrenceAt: null,
			buckets: [
				{ start: threeHoursAgo, count: 1 },
				{ start: oneHourAgo, count: 1 },
			],
		});

		// The whole table's aged rows as they stand right before the call: this file's 3, plus
		// anything ambient. The result must equal THIS -- rows -- and must exclude C's fresh rows.
		const agedBefore = await agedBucketRows(cutoff);
		expect(agedBefore).toBeGreaterThanOrEqual(3);

		installSeam();
		const result = await cleanupRetainedData(RETENTION_DAYS, 365, 30);

		// R3: bucket rows deleted.
		expect(result.deletedOccurrenceBuckets).toBe(agedBefore);

		// R1: no row before the sweep's own cutoff remains anywhere; every row at or after it on
		// this file's issues survives -- the straddling hour is gone, its successor is not.
		expect(await agedBucketRows(result.cutoffDate)).toBe(0);
		expect(await remainingBucketStarts(issueA)).toEqual(
			[successor, twoHoursAgo, oneHourAgo].map((d) => d.toISOString())
		);
		expect(await remainingBucketStarts(issueB)).toEqual(
			[tenDaysAgo, oneHourAgo].map((d) => d.toISOString())
		);

		// R2: the orphan is gone and took its bucket rows with it by cascade -- and those two rows
		// were not counted above (agedBefore excludes them; the equality already held).
		expect(result.deletedOrphanedIssues).toBeGreaterThanOrEqual(1);
		expect(await sql`select id from issues where id = ${issueC}`).toHaveLength(0);
		expect(await remainingBucketStarts(issueC)).toEqual([]);

		// The statement order, read off what the sweep actually issued (decision 4): the occurrence
		// delete, then the two side-table transactions (three statements each), then the bucket
		// delete as ONE top-level autocommit statement, then the orphan candidate select on
		// `issues` -- and no `FOR UPDATE` anywhere outside the two side-table transactions.
		const sweep = sweepRecords(recorded);
		const order = sweep.map(describeRecord);
		const bucketDeleteIndex = sweep.findIndex(
			(r) => r.kind === 'execute' && BUCKET_DELETE_SQL.test(r.sql)
		);
		expect(bucketDeleteIndex, `bucket delete not found in: ${order.join(' → ')}`).toBeGreaterThan(-1);
		const transactionIndexes = sweep
			.map((r, i) => (r.kind === 'transaction' ? i : -1))
			.filter((i) => i >= 0);
		// Three transactions: the two side-table steps and -- LAST, after everything else the sweep
		// does -- sentinel-releases-snooze §4.3's one-statement `next` catch-up, proven by
		// retention.next-catchup.integration.test.ts. Only the first two are the side-table steps the
		// bucket delete is ordered against.
		expect(transactionIndexes, `expected three transactions in: ${order.join(' → ')}`).toHaveLength(3);
		expect(transactionIndexes[2]).toBe(sweep.length - 1);
		const orphanSelectIndex = sweep.findIndex((r) => r.kind === 'select' && r.table === 'issues');
		expect(orphanSelectIndex, `orphan candidate select not found in: ${order.join(' → ')}`).toBeGreaterThan(-1);

		// After both side-table sweeps …
		expect(bucketDeleteIndex).toBeGreaterThan(transactionIndexes[1]);
		// … and before the orphan select (with nothing in between).
		expect(bucketDeleteIndex).toBeLessThan(orphanSelectIndex);
		expect(orphanSelectIndex - bucketDeleteIndex).toBe(1);
		expect(bucketDeleteIndex - transactionIndexes[1]).toBe(1);

		// Not inside any transaction (it is a top-level execute, so by construction), and it takes
		// no lock: the only `FOR UPDATE` the sweep issues is the side-table step's, inside its
		// transactions; nothing outside them mentions it, least of all the bucket delete.
		const bucketDelete = sweep[bucketDeleteIndex] as Extract<Recorded, { kind: 'execute' }>;
		expect(bucketDelete.sql).not.toMatch(/for update/i);
		for (const r of sweep) {
			if (r.kind === 'execute') expect(r.sql).not.toMatch(/for update/i);
		}
		for (const [i, r] of sweep.entries()) {
			if (r.kind !== 'transaction') continue;
			if (i === transactionIndexes[2]) {
				// The last transaction is sentinel-releases-snooze §4.3's `next` catch-up: one
				// statement, no lock, proven in retention.next-catchup.integration.test.ts.
				expect(r.statements).toHaveLength(1);
				expect(r.statements[0]).toMatch(/^update "issues" i set resolved_in_version = r\.version/i);
				expect(r.statements[0]).not.toMatch(/for update/i);
				continue;
			}
			expect(r.statements).toHaveLength(3);
			expect(r.statements[0]).toMatch(/for update$/i);
			expect(r.statements.some((s) => BUCKET_DELETE_SQL.test(s))).toBe(false);
		}

		// The order, pasted (criterion 1 asks for the recorded order in the output).
		console.log(
			[
				'',
				'== §4.5 bucket sweep — recorded cleanupRetainedData statement order',
				...order.map((o, i) => `  ${i + 1}. ${o}`),
				`bucket delete SQL: ${bucketDelete.sql}`,
				`deletedOccurrenceBuckets ${result.deletedOccurrenceBuckets} (= aged rows before ${agedBefore}), deletedOrphanedIssues ${result.deletedOrphanedIssues}, cutoff ${result.cutoffDate.toISOString()}`,
				'',
			].join('\n')
		);
		// The sweep locks every touched `issues` row and waits on any held by another file's
		// transaction, so under the full parallel run this case can sit well past vitest's 5 s
		// default; 120 s, like the family's hooks.
	}, 120_000);
});

describe.skipIf(!measure)(
	'§13 M4 — the bucket sweep on the committed seed (labelled fallback)',
	() => {
		let sql: ReturnType<typeof postgres>;

		beforeAll(async () => {
			sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
			await sql`select pg_advisory_lock(hashtext(${RETENTION_FAMILY_LOCK}))`;
		}, 120_000);

		afterEach(() => {
			removeSeam();
		});

		afterAll(async () => {
			if (!sql) return;
			await sql`select pg_advisory_unlock_all()`.catch(() => {});
			await sql.end({ timeout: 5 }).catch(() => {});
		});

		it('times the bucket delete, pastes its plan, and holds the predicate: no row < cutoff after, every row >= cutoff kept, deletedOccurrenceBuckets = rows removed', async () => {
			const { cleanupRetainedData } = await import('./retention');
			const { getRetentionDays } = await import('./retention-window');

			// §13's data condition, read through the same helper the loaders and the cron route use.
			const retentionDays = getRetentionDays();
			expect(retentionDays, 'M4 is taken with DATA_RETENTION_DAYS=30 (§13)').toBe(30);

			// The seed must be there, or this measures an empty table and says so loudly.
			const [{ present }] = await sql<{ present: number }[]>`
				select count(*)::int as present from organizations where id = ${SEED_ORG_ID}
			`;
			expect(
				present,
				'BUCKETS_M4_MEASURE=1 but the seed organization is absent — load scripts/issue-trend-seed.sql first'
			).toBe(1);

			const cutoff = cutoffNow();
			const cutoffIso = cutoff.toISOString();

			// ---- before: what the DELETE will remove, and what must survive
			const before = async () => {
				const [row] = await sql<
					{ aged: number; aged_issues: number; kept: number; total: number }[]
				>`
					select count(*) filter (where bucket_start < ${cutoff})::int                 as aged,
					       count(distinct issue_id) filter (where bucket_start < ${cutoff})::int as aged_issues,
					       count(*) filter (where bucket_start >= ${cutoff})::int                as kept,
					       count(*)::int                                                     as total
					  from issue_occurrence_buckets
				`;
				return row;
			};
			const stats = await before();
			expect(stats.aged, 'the seed puts ~50 issues’ rows past a 30-day cutoff').toBeGreaterThan(0);
			expect(stats.aged_issues).toBeGreaterThanOrEqual(40);
			// Every (issue, bucket_start) at or after the cutoff, on the seed's projects: the set that
			// must be identical afterwards.
			const keptBefore = await sql<{ issue_id: string; bucket_start: Date }[]>`
				select b.issue_id, b.bucket_start
				  from issue_occurrence_buckets b
				  join issues i on i.id = b.issue_id
				 where b.bucket_start >= ${cutoff}
				   and i.project_id = any(${sql.array(SEED_PROJECT_IDS)}::uuid[])
				 order by 1, 2
			`;
			expect(keptBefore.length).toBe(stats.kept);

			// ---- the plan, taken in a transaction that is rolled back so the sweep below sees the
			// seed untouched. The SQL is retention.ts's, as Postgres receives it.
			let plan = '';
			await sql
				.begin(async (tx) => {
					const rows = await tx.unsafe(
						`EXPLAIN (ANALYZE, BUFFERS) DELETE FROM "issue_occurrence_buckets" WHERE "issue_occurrence_buckets"."bucket_start" < $1::timestamptz RETURNING issue_id`,
						[cutoffIso]
					);
					plan = rows.map((r) => String(Object.values(r)[0])).join('\n');
					throw new Error('m4: plan taken, roll back');
				})
				.catch((e: unknown) => {
					if (!(e instanceof Error) || !e.message.startsWith('m4: plan taken')) throw e;
				});
			// Rolled back: the aged rows are all still there.
			expect((await before()).aged).toBe(stats.aged);

			// ---- the sweep, timed through the seam
			installSeam();
			const wall0 = performance.now();
			const result = await cleanupRetainedData(retentionDays, 365, 30);
			const wallMs = performance.now() - wall0;
			const bucketDelete = recorded.find(
				(r): r is Extract<Recorded, { kind: 'execute' }> =>
					r.kind === 'execute' && BUCKET_DELETE_SQL.test(r.sql)
			);
			expect(bucketDelete, 'the bucket delete was issued as one top-level statement').toBeDefined();

			// ---- after: the predicate (§13 M4 secondary), against the sweep's own cutoff
			expect(result.deletedOccurrenceBuckets).toBe(stats.aged);
			const [after] = await sql<{ aged: number; kept: number }[]>`
				select count(*) filter (where bucket_start < ${result.cutoffDate})::int  as aged,
				       count(*) filter (where bucket_start >= ${result.cutoffDate})::int as kept
				  from issue_occurrence_buckets
			`;
			expect(after.aged, 'no row with bucket_start < cutoff survives the sweep').toBe(0);
			const keptAfter = await sql<{ issue_id: string; bucket_start: Date }[]>`
				select b.issue_id, b.bucket_start
				  from issue_occurrence_buckets b
				  join issues i on i.id = b.issue_id
				 where b.bucket_start >= ${cutoff}
				   and i.project_id = any(${sql.array(SEED_PROJECT_IDS)}::uuid[])
				 order by 1, 2
			`;
			expect(keptAfter.length, 'every row >= cutoff survives').toBe(keptBefore.length);
			expect(
				keptAfter.map((r) => `${r.issue_id}@${r.bucket_start.toISOString()}`)
			).toEqual(keptBefore.map((r) => `${r.issue_id}@${r.bucket_start.toISOString()}`));

			const usesIndex = plan.includes('idx_issue_occurrence_buckets_bucket_start');

			// The report. Printed, not asserted: §13 M4's plan and timing are pasted into status/be.md.
			console.log(
				[
					'',
					'== §13 M4 (labelled fallback) — retention_bucket_sweep_ms on the committed seed, DATA_RETENTION_DAYS=30',
					`cutoff ${cutoffIso} (the sweep's own: ${result.cutoffDate.toISOString()})`,
					`before: total ${stats.total}, aged ${stats.aged} rows over ${stats.aged_issues} issues, kept ${stats.kept}`,
					`retention_bucket_sweep_ms (the one DELETE statement): ${round(bucketDelete!.ms)} ms`,
					`whole cleanupRetainedData call (occurrences, side tables, buckets, orphans, tombstones, …): ${round(wallMs)} ms`,
					`result: deletedOccurrences ${result.deletedOccurrences}, deletedAffectedUsers ${result.deletedAffectedUsers}, deletedAffectedTenants ${result.deletedAffectedTenants}, ` +
						`deletedOccurrenceBuckets ${result.deletedOccurrenceBuckets} (= aged rows ${stats.aged}), deletedOrphanedIssues ${result.deletedOrphanedIssues}`,
					`after: rows < cutoff ${after.aged}, rows >= cutoff ${after.kept} (seed-project set identical: ${keptAfter.length} of ${keptBefore.length})`,
					'',
					`VERDICT: idx_issue_occurrence_buckets_bucket_start ${usesIndex ? 'IS' : 'is NOT'} used by the DELETE's plan`,
					'',
					"-- the DELETE (the criterion's plan)",
					plan,
					'',
				].join('\n')
			);
		}, 300_000);
	}
);
