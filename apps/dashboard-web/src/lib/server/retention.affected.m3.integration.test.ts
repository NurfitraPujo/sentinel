import { describe, it, expect, beforeAll, afterAll, afterEach } from 'vitest';
import postgres from 'postgres';
import type { SQL } from 'drizzle-orm';
import { db, type Tx } from '$lib/server/db';

/**
 * sentinel-affected-users §13 M3 — the LABELLED FALLBACK take of the §4.5 sweep step on the
 * committed seed (decision 20): `retention_affected_sweep_ms`, the three statements (lock, delete,
 * recompute) inside `cleanupRetainedData`, timed in the test that drives them, with the DELETE's
 * `EXPLAIN (ANALYZE, BUFFERS)` pasted and one line stating whether
 * `idx_issue_affected_users_last_seen` is used. Evidence about plan shape on a local disposable
 * Postgres; never the criterion met, and M3 is never taken on a rig (§13: destructive on shared
 * data).
 *
 * This is the measurement companion of retention.affected.integration.test.ts (R1–R4, the
 * correctness boundary). That file seeds its own handful of rows; this one runs against the
 * committed seed — `scripts/affected-users-seed.sql`, 2,200 issues, 68,173 user rows of which
 * 33,982 are older than 30 days — because a plan on five rows says nothing about the index. It
 * asserts what §13 M3 names as its secondary criterion: the §4.6 invariant on every touched issue
 * afterwards, and `deletedAffectedUsers` / `deletedAffectedTenants` equal to the side rows the
 * DELETE removed (counted from the table immediately before the call, never from the issues
 * touched). The plan and the timings are REPORTED, not gated (§13: "timing reported, not gated").
 *
 * It is destructive by design (it IS the sweep) and it is opt-in: AFFECTED_M3_MEASURE=1 with
 * DATABASE_URL pointing at a disposable database that has the seed loaded, and
 * DATA_RETENTION_DAYS=30 (§13's data condition — the seed's 60-day spread puts half the side rows
 * past that cutoff). Without the switch it skips; with the switch and no seed it FAILS, so a
 * measurement of an empty table cannot be mistaken for the measurement:
 *
 *   docker run -d --name sentinel-affected-pg -p 15707:5432 \
 *     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && GOWORK=off \
 *     DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15707/sentinel?sslmode=disable \
 *     go run ./cmd/migrate up -target dashboard
 *   psql postgres://sentinel:changeme@localhost:15707/sentinel -f apps/dashboard-web/scripts/affected-users-seed.sql
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15707/sentinel \
 *     DATA_RETENTION_DAYS=30 AFFECTED_M3_MEASURE=1 \
 *     pnpm vitest run src/lib/server/retention.affected.m3.integration.test.ts
 *
 * Re-running needs the seed re-loaded: the first run swept it.
 */
const measure = process.env.AFFECTED_M3_MEASURE === '1';
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';

// The seed's fixed ids (affected-users-seed.sql's header).
const SEED_ORG_ID = '00000000-0000-4000-8000-0000000000a1';
const SEED_PROJECT_IDS = [
	'00000000-0000-4000-8000-0000000000a2',
	'00000000-0000-4000-8000-0000000000a3',
];
const RETENTION_FAMILY_LOCK = 'sentinel.retention.integration';

// ---------------------------------------------------------------------------------------------
// The same seam retention.affected.integration.test.ts uses (wrap the real `db.transaction`, record
// every `tx.execute`), extended with a stopwatch: each statement's wall clock, and each
// transaction's, so `retention_affected_sweep_ms` is the step's own duration and not the whole
// `cleanupRetainedData` call around it.
// ---------------------------------------------------------------------------------------------
interface TimedStatement {
	sql: string;
	ms: number;
}
interface TimedTransaction {
	statements: TimedStatement[];
	ms: number;
}
type TransactionMethod = typeof db.transaction;

const realTransaction: TransactionMethod = db.transaction.bind(db);
let recorded: TimedTransaction[] = [];

function renderSql(tx: Tx, query: SQL): string {
	const { dialect } = tx as unknown as {
		dialect: { sqlToQuery(sql: SQL): { sql: string } };
	};
	return dialect.sqlToQuery(query).sql.replace(/\s+/g, ' ').trim();
}

function installTimingSeam(): void {
	const wrapped: TransactionMethod = (callback, config) => {
		const started = performance.now();
		const record: TimedTransaction = { statements: [], ms: 0 };
		recorded.push(record);
		return realTransaction(async (tx) => {
			const realExecute = tx.execute.bind(tx);
			const execute = (async (query: SQL) => {
				const t0 = performance.now();
				const result = await realExecute(query);
				record.statements.push({
					sql: renderSql(tx, query),
					ms: performance.now() - t0,
				});
				return result;
			}) as unknown as typeof tx.execute;
			(tx as { execute: typeof tx.execute }).execute = execute;
			return callback(tx);
		}, config).finally(() => {
			record.ms = performance.now() - started;
		});
	};
	(db as { transaction: TransactionMethod }).transaction = wrapped;
}

function removeTimingSeam(): void {
	(db as { transaction: TransactionMethod }).transaction = realTransaction;
	recorded = [];
}

const round = (ms: number) => Math.round(ms * 1000) / 1000;

describe.skipIf(!measure)(
	'§13 M3 — the side-table sweep on the committed seed (labelled fallback)',
	() => {
		let sql: ReturnType<typeof postgres>;

		beforeAll(async () => {
			sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
			await sql`select pg_advisory_lock(hashtext(${RETENTION_FAMILY_LOCK}))`;
		}, 120_000);

		afterEach(() => {
			removeTimingSeam();
		});

		afterAll(async () => {
			if (!sql) return;
			await sql`select pg_advisory_unlock_all()`.catch(() => {});
			await sql.end({ timeout: 5 }).catch(() => {});
		});

		it('times the lock/delete/recompute step, pastes the DELETE plan, and holds the invariant with deletedAffectedUsers = rows removed', async () => {
			const { cleanupRetainedData } = await import('./retention');
			const { getRetentionDays } = await import('./retention-window');

			// §13's data condition, read through the same helper the loaders and the cron route use.
			const retentionDays = getRetentionDays();
			expect(retentionDays, 'M3 is taken with DATA_RETENTION_DAYS=30 (§13)').toBe(30);

			// The seed must be there, or this measures an empty table and says so loudly.
			const [{ present }] = await sql<{ present: number }[]>`
			select count(*)::int as present from organizations where id = ${SEED_ORG_ID}
		`;
			expect(
				present,
				'AFFECTED_M3_MEASURE=1 but the seed organization is absent — load scripts/affected-users-seed.sql first'
			).toBe(1);

			// The cutoff exactly as cleanupRetainedData computes it (setDate(−N) on now); the seed's
			// timestamps sit on hour boundaries, so the milliseconds between here and the call cannot
			// move a row across it.
			const cutoff = new Date();
			cutoff.setDate(cutoff.getDate() - retentionDays);

			// ---- before: what the DELETE will remove, and which issues the lock statement will take
			const before = async (table: 'issue_affected_users' | 'issue_affected_tenants') => {
				const [row] = await sql<{ aged: number; touched: number; total: number }[]>`
				select count(*) filter (where last_seen < ${cutoff})::int              as aged,
				       count(distinct issue_id) filter (where last_seen < ${cutoff})::int as touched,
				       count(*)::int                                                  as total
				  from ${sql(table)}
			`;
				return row;
			};
			const usersBefore = await before('issue_affected_users');
			const tenantsBefore = await before('issue_affected_tenants');
			expect(
				usersBefore.aged,
				'the seed puts about half the user rows past a 30-day cutoff'
			).toBeGreaterThan(0);

			const touchedUserIssues = (
				await sql<{ id: string }[]>`
				select distinct issue_id as id from issue_affected_users where last_seen < ${cutoff} order by 1
			`
			).map((r) => r.id);
			const touchedTenantIssues = (
				await sql<{ id: string }[]>`
				select distinct issue_id as id from issue_affected_tenants where last_seen < ${cutoff} order by 1
			`
			).map((r) => r.id);

			// ---- the plans, taken in a transaction that is rolled back so the sweep below sees the
			// seed untouched. Statement 2 is the criterion's plan (does it use
			// idx_issue_affected_users_last_seen?); statements 1 and 3 are pasted beside it because the
			// step is the three together. The SQL is retention.ts's, as Postgres receives it.
			const cutoffIso = cutoff.toISOString();
			const plans: Record<string, string> = {};
			await sql
				.begin(async (tx) => {
					const explain = async (label: string, text: string, params: unknown[]) => {
						const rows = await tx.unsafe(`EXPLAIN (ANALYZE, BUFFERS) ${text}`, params as never[]);
						plans[label] = rows.map((r) => String(Object.values(r)[0])).join('\n');
					};
					await explain(
						'1. lock',
						`SELECT "issues"."id" AS id FROM "issues" WHERE "issues"."id" IN ( SELECT DISTINCT issue_id FROM "issue_affected_users" WHERE last_seen < $1::timestamptz ) ORDER BY "issues"."id" FOR UPDATE`,
						[cutoffIso]
					);
					await explain(
						'2. delete',
						`DELETE FROM "issue_affected_users" WHERE last_seen < $1::timestamptz AND issue_id = ANY($2::uuid[]) RETURNING issue_id`,
						[cutoffIso, touchedUserIssues]
					);
					await explain(
						'3. recompute',
						`UPDATE "issues" SET "affected_user_count" = ( SELECT count(*) FROM "issue_affected_users" a WHERE a.issue_id = "issues"."id" ) WHERE "issues"."id" = ANY($1::uuid[])`,
						[touchedUserIssues]
					);
					throw new Error('m3: plans taken, roll back');
				})
				.catch((e: unknown) => {
					if (!(e instanceof Error) || !e.message.startsWith('m3: plans taken')) throw e;
				});

			// Rolled back: the aged rows are all still there.
			expect((await before('issue_affected_users')).aged).toBe(usersBefore.aged);

			// ---- the sweep, timed through the seam
			installTimingSeam();
			const wall0 = performance.now();
			const result = await cleanupRetainedData(retentionDays, 365, 30);
			const wallMs = performance.now() - wall0;

			// ---- after
			expect(recorded, 'one transaction per side table, users then tenants').toHaveLength(2);
			const [usersTx, tenantsTx] = recorded;
			expect(usersTx.statements.map((s) => s.sql.split(' ')[0])).toEqual([
				'SELECT',
				'DELETE',
				'UPDATE',
			]);
			expect(tenantsTx.statements.map((s) => s.sql.split(' ')[0])).toEqual([
				'SELECT',
				'DELETE',
				'UPDATE',
			]);

			// deletedAffectedUsers = the side rows the DELETE removed, not the issues touched (§4.5).
			expect(result.deletedAffectedUsers).toBe(usersBefore.aged);
			expect(result.deletedAffectedTenants).toBe(tenantsBefore.aged);
			expect(result.deletedAffectedUsers).not.toBe(usersBefore.touched);

			const usersAfter = await before('issue_affected_users');
			const tenantsAfter = await before('issue_affected_tenants');
			expect(usersAfter.aged, 'no aged user row survives the sweep').toBe(0);
			expect(tenantsAfter.aged, 'no aged tenant row survives the sweep').toBe(0);

			// The invariant on EVERY touched issue that still exists — the orphan rule (which runs
			// after the step) removes the issues whose only occurrence aged out with their side rows
			// cascading, so "touched" splits into "recomputed and present" and "gone by cascade".
			const invariant = async (ids: string[]) => {
				const [row] = await sql<{ present: number; violations: number }[]>`
				select count(*)::int as present,
				       count(*) filter (where
				           affected_user_count   <> (select count(*) from issue_affected_users   a where a.issue_id = i.id)
				        or affected_tenant_count <> (select count(*) from issue_affected_tenants a where a.issue_id = i.id)
				       )::int as violations
				  from issues i where i.id = any(${sql.array(ids)}::uuid[])
			`;
				return row;
			};
			const touchedUsersNow = await invariant(touchedUserIssues);
			const touchedTenantsNow = await invariant(touchedTenantIssues);
			expect(touchedUsersNow.violations, 'invariant on every touched (users) issue').toBe(0);
			expect(touchedTenantsNow.violations, 'invariant on every touched (tenants) issue').toBe(0);

			// And on every seed issue, touched or not.
			const [seedNow] = await sql<{ present: number; violations: number }[]>`
			select count(*)::int as present,
			       count(*) filter (where
			           affected_user_count   <> (select count(*) from issue_affected_users   a where a.issue_id = i.id)
			        or affected_tenant_count <> (select count(*) from issue_affected_tenants a where a.issue_id = i.id)
			       )::int as violations
			  from issues i where i.project_id = any(${sql.array(SEED_PROJECT_IDS)}::uuid[])
		`;
			expect(seedNow.violations, 'invariant on every seed issue').toBe(0);

			const usesIndex = plans['2. delete'].includes('idx_issue_affected_users_last_seen');
			const fmtTx = (label: string, t: TimedTransaction) =>
				[
					`${label}: ${round(t.ms)} ms (transaction)`,
					...t.statements.map(
						(s, i) =>
							`  ${i + 1}. ${s.sql.split(' ')[0].padEnd(6)} ${round(s.ms).toString().padStart(9)} ms`
					),
				].join('\n');

			// The report. Printed, not asserted: §13 M3's plan and timings are pasted into status/be.md.
			console.log(
				[
					'',
					'== §13 M3 (labelled fallback) — retention_affected_sweep_ms on the committed seed, DATA_RETENTION_DAYS=30',
					`cutoff ${cutoffIso}`,
					`users   before: total ${usersBefore.total}, aged ${usersBefore.aged}, touched issues ${usersBefore.touched}`,
					`tenants before: total ${tenantsBefore.total}, aged ${tenantsBefore.aged}, touched issues ${tenantsBefore.touched}`,
					'',
					fmtTx('users   step', usersTx),
					fmtTx('tenants step', tenantsTx),
					`retention_affected_sweep_ms (both side tables, the two transactions): ${round(usersTx.ms + tenantsTx.ms)} ms`,
					`whole cleanupRetainedData call (occurrences, side tables, orphans, tombstones, …): ${round(wallMs)} ms`,
					'',
					`result: deletedOccurrences ${result.deletedOccurrences}, deletedAffectedUsers ${result.deletedAffectedUsers} (= aged user rows ${usersBefore.aged}; touched issues were ${usersBefore.touched}), ` +
						`deletedAffectedTenants ${result.deletedAffectedTenants} (= aged tenant rows ${tenantsBefore.aged}), deletedOrphanedIssues ${result.deletedOrphanedIssues}`,
					`after: touched (users) issues present ${touchedUsersNow.present} of ${touchedUserIssues.length}, violations ${touchedUsersNow.violations}; ` +
						`touched (tenants) present ${touchedTenantsNow.present} of ${touchedTenantIssues.length}, violations ${touchedTenantsNow.violations}; ` +
						`seed issues present ${seedNow.present}, violations ${seedNow.violations}; aged user rows left ${usersAfter.aged}, aged tenant rows left ${tenantsAfter.aged}`,
					'',
					`VERDICT: idx_issue_affected_users_last_seen ${usesIndex ? 'IS' : 'is NOT'} used by the DELETE's plan`,
					'',
					'-- 1. lock (SELECT … FOR UPDATE)',
					plans['1. lock'],
					"-- 2. delete (the criterion's plan)",
					plans['2. delete'],
					'-- 3. recompute (UPDATE … = ANY(locked ids))',
					plans['3. recompute'],
					'',
				].join('\n')
			);
		}, 300_000);
	}
);
