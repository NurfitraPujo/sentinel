import { describe, it, expect, beforeAll, afterAll, afterEach, vi } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';
import { getTableName, type SQL } from 'drizzle-orm';
import { db, type Tx } from '$lib/server/db';
import { renderMetrics } from './metrics';

/**
 * sentinel-releases-snooze §4.3 "Hourly" (flow I19; decisions 8, 15, 27, 30, 42) -- contract §11's
 * boundary for the `next` catch-up, against a REAL migrated Postgres, because every claim below is a
 * property of live SQL: that after `cleanupRetainedData` a `next` issue with two releases newer than
 * its `resolved_at` reads the EARLIER of them (ties on `first_seen` broken by `version`); that a
 * `next` issue whose only releases are older is untouched; that a `next` with `resolved_at IS NULL`
 * is untouched (decision 27 -- `first_seen > NULL` joins nothing); that a `next` in another project
 * is rewritten by THAT project's release and never by this one's; that no `issue_activity` row is
 * written (decision 8); that the sweep's other results are unchanged by the statement's presence;
 * and -- the failure half -- that a statement that throws does NOT fail the sweep, is logged as
 * `retention_cron.next_catchup_failed` with the SQLSTATE, and moves
 * `sentinel_release_next_rewrite_failures_total` in this process's registry by exactly one. A
 * mock-chained Drizzle test can prove none of it -- only the sweep can prove the sweep.
 *
 * **Lock-order note (§4.3, beside the case as §11 asks).** The catch-up runs AFTER the side-table
 * steps, in its own transaction, locking `issues` rows only and reading `releases` without a lock.
 * The side-table steps' `SELECT id … ORDER BY id FOR UPDATE` protocol is NOT extended to it and does
 * not need to be: it holds nothing those steps wait on, because it runs after they have committed,
 * in a transaction of its own; and against an ingest -- which holds its `issues` row FIRST and wants
 * the `releases` row AFTER it -- there is no cycle, because the catch-up wants no `releases` row.
 * The one writer it CAN deadlock with is `batchUpdateIssues` over the same `next` rows in the same
 * second (decision 42): Postgres kills one side with `40P01`, and if it is this one the log line
 * names that SQLSTATE and the next hourly run redoes the work -- which is exactly what the failure
 * case below simulates, with a trigger raising `40P01`. The order and the own-transaction claims are
 * asserted here off the statements the sweep actually issued, through the same seam
 * retention.buckets.integration.test.ts uses.
 *
 * retention.affected.integration.test.ts's style: seeds its OWN organization/projects/issues/releases
 * under a per-run suffix and deletes them again, so nothing here depends on ambient data. Every
 * seeded issue has a recent `first_seen`, so the sweep's orphan rule never touches the fixture.
 *
 * Point DATABASE_URL at a DISPOSABLE database -- never the shared dev one (CLAUDE.md):
 *
 *   docker run -d --name sentinel-catchup-pg -p 15599:5432 \
 *     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && GOWORK=off \
 *     DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15599/sentinel?sslmode=disable \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15599/sentinel \
 *     NEXT_CATCHUP_INTEGRATION_REQUIRED=1 \
 *     pnpm vitest run src/lib/server/retention.next-catchup.integration.test.ts
 *
 * Skipped when nothing answers, so a machine with no Postgres is not broken by it. Set
 * SCHEMA_DRIFT_REQUIRED=1 (the switch CI's dashboard job sets for the DB-backed suites) or
 * NEXT_CATCHUP_INTEGRATION_REQUIRED=1 to make "no reachable database" a hard failure, so the only
 * proof that §4.3's catch-up holds cannot be skipped silently.
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required =
	process.env.SCHEMA_DRIFT_REQUIRED === '1' ||
	process.env.NEXT_CATCHUP_INTEGRATION_REQUIRED === '1';

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
		`SCHEMA_DRIFT_REQUIRED=1 / NEXT_CATCHUP_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}. ` +
			'This test is the only coverage that runs the §4.3 next catch-up against real Postgres; ' +
			'skipping it silently would leave I19 unguarded.'
	);
}

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectA = randomUUID();
const projectB = randomUUID();

// The retention.*.integration family runs `cleanupRetainedData` / `reapStaleClaims`, which sweep
// the WHOLE database, and vitest runs files in parallel workers -- so one file's sweep can delete
// another file's fixture between its seed and its own sweep. The family serialises itself on one
// session-level advisory lock, taken in `beforeAll` and released in `afterAll`; the key is the
// same in every member.
const RETENTION_FAMILY_LOCK = 'sentinel.retention.integration';

function hoursFrom(date: Date, h: number): Date {
	return new Date(date.getTime() + h * 3_600_000);
}
function minutesFrom(date: Date, m: number): Date {
	return new Date(date.getTime() + m * 60_000);
}

// ---------------------------------------------------------------------------------------------
// The test-side seam (retention.buckets.integration.test.ts's, verbatim in shape): record, in call
// order, every statement the sweep issues through the module singleton -- `tx.execute` inside each
// `db.transaction` (grouped per transaction) and the top-level `db.execute`, `db.select(...).from()`
// and `db.delete()` calls around them. The record IS cleanupRetainedData's statement order, which
// is how "LAST" and "its own transaction" are asserted rather than read off the source.
// ---------------------------------------------------------------------------------------------
type Recorded =
	| { kind: 'transaction'; statements: string[] }
	| { kind: 'execute'; sql: string }
	| { kind: 'select'; table: string }
	| { kind: 'delete'; table: string };

type TransactionMethod = typeof db.transaction;
type ExecuteMethod = typeof db.execute;
type SelectMethod = typeof db.select;
type DeleteMethod = typeof db.delete;

const realTransaction: TransactionMethod = db.transaction.bind(db);
const realExecute: ExecuteMethod = db.execute.bind(db);
const realSelect: SelectMethod = db.select.bind(db);
const realDelete: DeleteMethod = db.delete.bind(db);
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

function installSeam(): void {
	const transaction: TransactionMethod = (callback, config) => {
		const record: Recorded = { kind: 'transaction', statements: [] };
		recorded.push(record);
		return realTransaction(async (tx) => {
			const realTxExecute = tx.execute.bind(tx);
			const execute = (async (query: SQL) => {
				// Rendered BEFORE the statement runs, so a statement that throws is still on record.
				record.statements.push(renderSql(tx, query));
				return realTxExecute(query);
			}) as unknown as typeof tx.execute;
			(tx as { execute: typeof tx.execute }).execute = execute;
			return callback(tx);
		}, config);
	};
	const execute = (async (query: SQL) => {
		recorded.push({ kind: 'execute', sql: renderSql(db, query) });
		return realExecute(query);
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

	const target = db as {
		transaction: TransactionMethod;
		execute: ExecuteMethod;
		select: SelectMethod;
		delete: DeleteMethod;
	};
	target.transaction = transaction;
	target.execute = execute;
	target.select = select;
	target.delete = del;
}

function removeSeam(): void {
	const target = db as {
		transaction: TransactionMethod;
		execute: ExecuteMethod;
		select: SelectMethod;
		delete: DeleteMethod;
	};
	target.transaction = realTransaction;
	target.execute = realExecute;
	target.select = realSelect;
	target.delete = realDelete;
	recorded = [];
}

// §4.3's statement, as Drizzle renders it through the seam (table names quoted, whitespace folded).
const CATCHUP_SQL =
	/^update "issues" i set resolved_in_version = r\.version from \(select distinct on \(n\.id\) n\.id, r2\.version from "issues" n join "releases" r2 on r2\.project_id = n\.project_id and r2\.first_seen > n\.resolved_at where n\.status = 'resolved' and n\.resolved_in_version = 'next' order by n\.id, r2\.first_seen asc, r2\.version asc\) r where i\.id = r\.id and i\.status = 'resolved' and i\.resolved_in_version = 'next' returning i\.id$/i;

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

function parseMetrics(body: string): Map<string, number> {
	const out = new Map<string, number>();
	for (const line of body.split('\n')) {
		if (!line || line.startsWith('#')) continue;
		const match = line.match(/^(\S+)\s+(\S+)$/);
		if (match) out.set(match[1], Number(match[2]));
	}
	return out;
}

const COUNTER = 'sentinel_release_next_rewrite_failures_total';

describe.skipIf(!dbReachable)('retention next catch-up (sentinel-releases-snooze §4.3, integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		// Family mutex (see RETENTION_FAMILY_LOCK): blocks until no sibling file is mid-sweep.
		await sql`select pg_advisory_lock(hashtext(${RETENTION_FAMILY_LOCK}))`;
		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'next-catchup-it-' + suffix}, ${'next-catchup-it-' + suffix})
		`;
		for (const [id, name] of [
			[projectA, 'a'],
			[projectB, 'b'],
		] as const) {
			await sql`
				insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${id}, ${'next-catchup-it-' + name + '-' + suffix}, ${orgId},
				        ${'k_' + name + '_' + suffix}, ${'h_' + name + '_' + suffix})
			`;
		}
	}, 120_000);

	afterEach(() => {
		removeSeam();
		vi.restoreAllMocks();
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issue_tombstones where organization_id = ${orgId}`;
			await sql`delete from releases where project_id in (${projectA}, ${projectB})`;
			// issue_activity cascades from issues.
			await sql`delete from issues where project_id in (${projectA}, ${projectB})`;
			await sql`delete from projects where id in (${projectA}, ${projectB})`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql`select pg_advisory_unlock_all()`.catch(() => {});
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	interface SeedIssue {
		project: string;
		fingerprint: string;
		// 'next' | a concrete version | null (an unresolved issue).
		resolvedInVersion: string | null;
		// `null` = resolved_at IS NULL (a pre-feature batch resolve, decision 27).
		resolvedAt: Date | null;
		status?: 'resolved' | 'unresolved';
	}

	async function seedIssue(seed: SeedIssue): Promise<string> {
		const id = randomUUID();
		const status = seed.status ?? 'resolved';
		await sql`
			insert into issues (
				id, project_id, environment, fingerprint, message, error_class, status, issue_type,
				first_seen, last_seen, resolved_in_version, resolved_at, resolved_by_type, resolved_by
			) values (
				${id}, ${seed.project}, ${'production'}, ${seed.fingerprint + '-' + suffix},
				${'catch-up seed ' + seed.fingerprint}, ${'SeedError'}, ${status}, ${'system_error'},
				${hoursFrom(new Date(), -1)}, ${hoursFrom(new Date(), -1)},
				${seed.resolvedInVersion}, ${seed.resolvedAt},
				${status === 'resolved' ? 'user' : null}, ${status === 'resolved' ? 'seed' : null}
			)
		`;
		return id;
	}

	async function seedRelease(project: string, version: string, firstSeen: Date): Promise<void> {
		await sql`
			insert into releases (project_id, version, first_seen, last_seen, occurrence_count)
			values (${project}, ${version}, ${firstSeen}, ${firstSeen}, 1)
		`;
	}

	async function resolvedInVersionOf(issueId: string): Promise<{ status: string; version: string | null }> {
		const [row] = await sql<{ status: string; resolved_in_version: string | null }[]>`
			select status, resolved_in_version from issues where id = ${issueId}
		`;
		return { status: row.status, version: row.resolved_in_version };
	}

	async function activityRows(issueIds: string[]): Promise<number> {
		const [row] = await sql<{ n: number }[]>`
			select count(*)::int as n from issue_activity where issue_id in ${sql(issueIds)}
		`;
		return row.n;
	}

	it('I19: rewrites each `next` issue to the EARLIEST release newer than its resolved_at (ties by version), leaves older-only and NULL-resolved_at issues alone, scopes by project, writes no activity row, runs LAST in its own transaction and leaves the sweep\'s other results unchanged', async () => {
		const { cleanupRetainedData } = await import('./retention');

		const now = new Date();
		const T = hoursFrom(now, -2); // the I19 resolution instant
		const T2 = minutesFrom(now, -10); // resolved after every seeded release
		const T3 = hoursFrom(now, -5); // resolved before the tie pair

		// Project A's releases: one older than T, two newer (the earlier must win), and a tie pair
		// with identical first_seen just after T3 whose version order must decide.
		await seedRelease(projectA, 'v1.0.0', hoursFrom(T, -1));
		await seedRelease(projectA, 'v2.0.0', minutesFrom(T, 10));
		await seedRelease(projectA, 'v3.0.0', minutesFrom(T, 20));
		await seedRelease(projectA, 'tie-b', minutesFrom(T3, 1));
		await seedRelease(projectA, 'tie-a', minutesFrom(T3, 1));
		// Project B's one release, newer than T -- for B's issue, never for A's.
		await seedRelease(projectB, 'b-1', minutesFrom(T, 5));

		// (1) two newer releases → the earlier of them.
		const earliest = await seedIssue({ project: projectA, fingerprint: 'earliest', resolvedInVersion: 'next', resolvedAt: T });
		// (2) only older releases → untouched.
		const olderOnly = await seedIssue({ project: projectA, fingerprint: 'older-only', resolvedInVersion: 'next', resolvedAt: T2 });
		// (3) resolved_at IS NULL → untouched (decision 27).
		const nullResolvedAt = await seedIssue({ project: projectA, fingerprint: 'null-at', resolvedInVersion: 'next', resolvedAt: null });
		// (4) another project's `next` → rewritten by THAT project's release.
		const otherProject = await seedIssue({ project: projectB, fingerprint: 'other-project', resolvedInVersion: 'next', resolvedAt: T });
		// The tie: two releases at one first_seen → the smaller version.
		const tie = await seedIssue({ project: projectA, fingerprint: 'tie', resolvedInVersion: 'next', resolvedAt: T3 });
		// Not waiting on `next`: an explicit version, and an unresolved row -- neither is touched.
		const explicit = await seedIssue({ project: projectA, fingerprint: 'explicit', resolvedInVersion: 'v1.0.0', resolvedAt: T });
		const unresolved = await seedIssue({ project: projectA, fingerprint: 'unresolved', resolvedInVersion: null, resolvedAt: null, status: 'unresolved' });

		const fixture = [earliest, olderOnly, nullResolvedAt, otherProject, tie, explicit, unresolved];
		expect(await activityRows(fixture)).toBe(0);

		installSeam();
		const result = await cleanupRetainedData(30, 365, 30);

		// The rewrites.
		expect(await resolvedInVersionOf(earliest)).toEqual({ status: 'resolved', version: 'v2.0.0' });
		expect(await resolvedInVersionOf(tie)).toEqual({ status: 'resolved', version: 'tie-a' });
		expect(await resolvedInVersionOf(otherProject)).toEqual({ status: 'resolved', version: 'b-1' });
		// The rows left alone.
		expect(await resolvedInVersionOf(olderOnly)).toEqual({ status: 'resolved', version: 'next' });
		expect(await resolvedInVersionOf(nullResolvedAt)).toEqual({ status: 'resolved', version: 'next' });
		expect(await resolvedInVersionOf(explicit)).toEqual({ status: 'resolved', version: 'v1.0.0' });
		expect(await resolvedInVersionOf(unresolved)).toEqual({ status: 'unresolved', version: null });

		// (5) decision 8: no issue_activity row for any of them.
		expect(await activityRows(fixture)).toBe(0);

		// (6) the sweep's other results are unchanged by the statement's presence: RetentionResult has
		// exactly the fields it had, the fixture (all recent) survived the orphan rule, and nothing of
		// this file's was deleted or tombstoned.
		expect(Object.keys(result).sort()).toEqual(
			[
				'cutoffDate',
				'deletedAffectedTenants',
				'deletedAffectedUsers',
				'deletedExternalReporters',
				'deletedOccurrenceBuckets',
				'deletedOccurrences',
				'deletedOrphanedIssues',
				'deletedTombstones',
				'manualRetentionDays',
				'retentionDays',
				'tombstoneRetentionDays',
				'tombstonesWritten',
			].sort()
		);
		expect(result.retentionDays).toBe(30);
		expect(await sql`select id from issues where id in ${sql(fixture)}`).toHaveLength(fixture.length);
		expect(await sql`select issue_id from issue_tombstones where organization_id = ${orgId}`).toHaveLength(0);

		// LAST, in its OWN transaction, one statement, verbatim §4.3, no lock on `releases` -- read
		// off what the sweep actually issued (the lock-order note in this file's header).
		const order = recorded.map(describeRecord);
		const last = recorded[recorded.length - 1];
		expect(last.kind, `expected the catch-up transaction last in: ${order.join(' → ')}`).toBe('transaction');
		const catchUp = last as Extract<Recorded, { kind: 'transaction' }>;
		expect(catchUp.statements).toHaveLength(1);
		expect(catchUp.statements[0]).toMatch(CATCHUP_SQL);
		expect(catchUp.statements[0]).not.toMatch(/for (update|share)/i);
		// The two side-table transactions come before it; nothing of the sweep comes after it.
		const transactionIndexes = recorded
			.map((r, i) => (r.kind === 'transaction' ? i : -1))
			.filter((i) => i >= 0);
		expect(transactionIndexes, order.join(' → ')).toHaveLength(3);
		expect(transactionIndexes[2]).toBe(recorded.length - 1);
		// And it is not the side-table steps' shape: it takes no `FOR UPDATE` and reads `releases`.
		for (const i of transactionIndexes.slice(0, 2)) {
			const t = recorded[i] as Extract<Recorded, { kind: 'transaction' }>;
			expect(t.statements).toHaveLength(3);
			expect(t.statements[0]).toMatch(/for update/i);
		}
	});

	it('failure: a catch-up that throws does not fail the sweep, is logged as retention_cron.next_catchup_failed with the SQLSTATE, and moves sentinel_release_next_rewrite_failures_total by one', async () => {
		const { cleanupRetainedData } = await import('./retention');

		const T = hoursFrom(new Date(), -3);
		await seedRelease(projectA, 'v9.0.0', minutesFrom(T, 5));
		const waiting = await seedIssue({ project: projectA, fingerprint: 'fails', resolvedInVersion: 'next', resolvedAt: T });

		// Decision 42's accepted failure, simulated where it would land: a BEFORE UPDATE trigger on
		// `issues` that raises 40P01 for exactly the write the catch-up performs on THIS project's
		// rows (resolved_in_version leaving 'next' while the row stays resolved). Every other writer
		// -- the sweep's own steps included -- is untouched.
		const fn = 'next_catchup_boom_' + suffix;
		await sql.unsafe(`
			CREATE OR REPLACE FUNCTION ${fn}() RETURNS trigger AS $fn$
			BEGIN
				IF OLD.project_id = '${projectA}'::uuid AND OLD.resolved_in_version = 'next'
				   AND NEW.resolved_in_version IS DISTINCT FROM 'next' AND NEW.status = 'resolved' THEN
					RAISE EXCEPTION 'simulated next catch-up deadlock (decision 42)' USING ERRCODE = '40P01';
				END IF;
				RETURN NEW;
			END;
			$fn$ LANGUAGE plpgsql
		`);
		await sql.unsafe(`CREATE TRIGGER ${fn} BEFORE UPDATE ON issues FOR EACH ROW EXECUTE FUNCTION ${fn}()`);
		const dropTrigger = async () => {
			await sql.unsafe(`DROP TRIGGER IF EXISTS ${fn} ON issues`);
			await sql.unsafe(`DROP FUNCTION IF EXISTS ${fn}()`);
		};

		try {
			const before = parseMetrics((await renderMetrics()).body).get(COUNTER) ?? 0;
			// The structured logger's sink is console.log (one JSON line); capture it.
			const lines: Array<Record<string, unknown>> = [];
			vi.spyOn(console, 'log').mockImplementation((line: unknown) => {
				try {
					lines.push(JSON.parse(String(line)));
				} catch {
					// not one of ours
				}
			});

			installSeam();
			// Resolves -- the sweep's deletes and recomputes are unaffected by the last statement.
			const result = await cleanupRetainedData(30, 365, 30);
			expect(result.retentionDays).toBe(30);

			// The waiting issue keeps `next` -- the next hourly run's input.
			expect(await resolvedInVersionOf(waiting)).toEqual({ status: 'resolved', version: 'next' });

			// Logged with the SQLSTATE.
			const failed = lines.filter((l) => l.event === 'retention_cron.next_catchup_failed');
			expect(failed, JSON.stringify(lines)).toHaveLength(1);
			expect(failed[0].level).toBe('error');
			expect(failed[0].sqlstate).toBe('40P01');
			expect((failed[0].error as { message: string }).message).toContain('simulated next catch-up deadlock');

			// Counted, once, under the contract's name, with no label, in THIS process's registry.
			const body = (await renderMetrics()).body;
			expect(body).toContain(`# TYPE ${COUNTER} counter`);
			expect(parseMetrics(body).get(COUNTER)).toBe(before + 1);
			expect(body).not.toMatch(new RegExp(`^${COUNTER}\\{`, 'm'));

			// The statement was issued, last, in its own transaction -- and it was the one that threw.
			const last = recorded[recorded.length - 1];
			expect(last.kind).toBe('transaction');
			expect((last as Extract<Recorded, { kind: 'transaction' }>).statements).toHaveLength(1);
			expect((last as Extract<Recorded, { kind: 'transaction' }>).statements[0]).toMatch(CATCHUP_SQL);
		} finally {
			await dropTrigger();
		}

		// With the trigger gone the next run does the same work and lands it (idempotent by
		// construction), and the counter does not move again.
		removeSeam();
		vi.restoreAllMocks();
		const before = parseMetrics((await renderMetrics()).body).get(COUNTER) ?? 0;
		await cleanupRetainedData(30, 365, 30);
		expect(await resolvedInVersionOf(waiting)).toEqual({ status: 'resolved', version: 'v9.0.0' });
		expect(parseMetrics((await renderMetrics()).body).get(COUNTER)).toBe(before);
	});
});
