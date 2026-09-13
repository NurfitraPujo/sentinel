import { describe, it, expect, beforeAll, afterAll, afterEach } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';
import type { SQL } from 'drizzle-orm';
import { db, type Tx } from '$lib/server/db';

/**
 * sentinel-affected-users §4.5 (decisions 12, 23, 28; flows R1–R4) -- contract §11's
 * `cleanupRetainedData` boundary, against a REAL migrated Postgres, because every claim below is a
 * property of live SQL inside one transaction: that the side rows with `last_seen < cutoff` go,
 * that each touched issue's `affected_user_count` / `affected_tenant_count` is recomputed to what
 * REMAINS (a separate statement in the same transaction -- a CTE would have counted the pre-delete
 * snapshot), that an untouched issue's count is not rewritten, that an orphaned issue's side rows
 * go by `ON DELETE CASCADE`, that `deletedAffectedUsers` / `deletedAffectedTenants` are side ROWS
 * deleted and not issues touched, that an ingest for a touched issue queues on the `issues` row
 * lock and completes without `40P01`, and that a failure part-way rolls the whole step back. A
 * mock-chained Drizzle test (retention.attachments.test.ts's style) can prove none of it -- only
 * the sweep can prove the sweep.
 *
 * The concurrent and the fault-injected cases need a seam INSIDE the transaction, and retention.ts
 * deliberately has none: this file wraps the real `db.transaction` (the module singleton
 * retention.ts imports) so that each `tx.execute` is recorded and an optional hook runs after it.
 * The hook is where the ingest is fired while the sweep holds its locks, and where the fault is
 * thrown after the DELETE. Nothing in production code knows about it; `afterEach` restores the
 * real method.
 *
 * retention.tombstones.integration.test.ts's style: seeds its OWN organization/project/issues
 * under a per-run suffix and deletes them again, so nothing here depends on ambient data. The
 * sweep itself is database-wide (it is the real one), so every assertion on a count is scoped to
 * this file's issues and the "rows, not issues" assertion is taken against the whole table's aged
 * rows as they stood immediately before the call.
 *
 * Point DATABASE_URL at a DISPOSABLE database -- never the shared dev one (CLAUDE.md):
 *
 *   docker run -d --name sentinel-affected-pg -p 15705:5432 \
 *     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && GOWORK=off \
 *     DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15705/sentinel?sslmode=disable \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15705/sentinel \
 *     AFFECTED_RETENTION_INTEGRATION_REQUIRED=1 \
 *     pnpm vitest run src/lib/server/retention.affected.integration.test.ts
 *
 * Skipped when nothing answers, so a machine with no Postgres is not broken by it. Set
 * SCHEMA_DRIFT_REQUIRED=1 (the switch CI's dashboard job sets for the DB-backed suites) or
 * AFFECTED_RETENTION_INTEGRATION_REQUIRED=1 to make "no reachable database" a hard failure, so
 * the only proof that §4.5 holds cannot be skipped silently.
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required =
	process.env.SCHEMA_DRIFT_REQUIRED === '1' ||
	process.env.AFFECTED_RETENTION_INTEGRATION_REQUIRED === '1';

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
		`SCHEMA_DRIFT_REQUIRED=1 / AFFECTED_RETENTION_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}. ` +
			'This test is the only coverage that runs the §4.5 side-table sweep against real Postgres; ' +
			'skipping it silently would leave R1-R4 and the decision-12 invariant unguarded.'
	);
}

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();

function daysAgo(d: number): Date {
	const date = new Date();
	date.setDate(date.getDate() - d);
	return date;
}

function hoursAgo(h: number): Date {
	const date = new Date();
	date.setHours(date.getHours() - h);
	return date;
}

// Either side of the sweep's cutoff (`retentionDays` = 30 in every call below). AGED is well past
// it. FRESH is one HOUR, not one day: retention.claims.integration.test.ts sweeps with a ONE-day
// cutoff, and a "fresh" row at exactly one day would be aged from its point of view.
const AGED_DAYS = 40;
const FRESH_HOURS = 1;
const aged = () => daysAgo(AGED_DAYS);
const fresh = () => hoursAgo(FRESH_HOURS);

// The retention.*.integration family runs `cleanupRetainedData` / `reapStaleClaims`, which sweep
// the WHOLE database, and vitest runs files in parallel workers -- so one file's sweep can delete
// another file's fixture in the milliseconds between its seed and its own sweep (this file's
// concurrent case even holds the issues row lock for 400ms on purpose). The family serialises
// itself on one session-level advisory lock, taken in `beforeAll` and released in `afterAll`; the
// key is the same in every member. `reports.detail.integration.test.ts` also sweeps and does NOT
// take it (its recorded parallel-sweep failure is not this item's to repair), so the residual
// exposure is one sweep in that file, the same exposure the tombstones test has always carried.
const RETENTION_FAMILY_LOCK = 'sentinel.retention.integration';

// ---------------------------------------------------------------------------------------------
// The test-side seam: wrap the real `db.transaction` so every `tx.execute` inside it is recorded
// (its rendered SQL, in order) and an optional hook runs AFTER it. The sweep is the only caller of
// `db.transaction` on the `cleanupRetainedData` path, so the record IS the step's statement order.
// ---------------------------------------------------------------------------------------------
type ExecuteHook = (statementIndex: number, sqlText: string) => Promise<void>;
type TransactionMethod = typeof db.transaction;

const realTransaction: TransactionMethod = db.transaction.bind(db);
let executeHook: ExecuteHook | null = null;
// One entry per transaction the sweep opened; each is that transaction's statements in order.
let recordedTransactions: string[][] = [];

function renderSql(tx: Tx, query: SQL): string {
	// `dialect` is `@internal` on PgDatabase, so it is absent from the types but present at runtime.
	const { dialect } = tx as unknown as { dialect: { sqlToQuery(sql: SQL): { sql: string } } };
	return dialect.sqlToQuery(query).sql.replace(/\s+/g, ' ').trim();
}

function installTransactionSeam(): void {
	const wrapped: TransactionMethod = (callback, config) =>
		realTransaction(async (tx) => {
			const statements: string[] = [];
			recordedTransactions.push(statements);
			const realExecute = tx.execute.bind(tx);
			const execute = (async (query: SQL) => {
				const result = await realExecute(query);
				statements.push(renderSql(tx, query));
				if (executeHook) await executeHook(statements.length, statements[statements.length - 1]);
				return result;
			}) as unknown as typeof tx.execute;
			(tx as { execute: typeof tx.execute }).execute = execute;
			return callback(tx);
		}, config);
	(db as { transaction: TransactionMethod }).transaction = wrapped;
}

function removeTransactionSeam(): void {
	(db as { transaction: TransactionMethod }).transaction = realTransaction;
	executeHook = null;
	recordedTransactions = [];
}

describe.skipIf(!dbReachable)('retention side-table sweep (§4.5, integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		// Family mutex (see RETENTION_FAMILY_LOCK): blocks until no sibling file is mid-sweep. The
		// explicit hook timeout is the sum of the siblings' worst-case durations under a full run.
		await sql`select pg_advisory_lock(hashtext(${RETENTION_FAMILY_LOCK}))`;
		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'affected-retention-it-' + suffix}, ${'affected-retention-it-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'affected-retention-it-' + suffix}, ${orgId},
			        ${'k_' + suffix}, ${'h_' + suffix})
		`;
	}, 120_000);

	afterEach(() => {
		removeTransactionSeam();
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issue_tombstones where organization_id = ${orgId}`;
			// The side tables and error_occurrences cascade from issues.
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
		users: Array<{ id: string; lastSeen: Date }>;
		tenants: Array<{ id: string; lastSeen: Date }>;
		// The counts as seeded. Normally the cardinality; the untouched issue seeds a deliberately
		// WRONG value so "unchanged" is distinguishable from "recomputed to the same number".
		userCount: number;
		tenantCount: number;
	}

	async function seedIssue(seed: SeedIssue): Promise<string> {
		const id = randomUUID();
		await sql`
			insert into issues (
				id, project_id, environment, fingerprint, message, error_class, status, issue_type,
				first_seen, last_seen, affected_user_count, affected_tenant_count
			) values (
				${id}, ${projectId}, ${'production'}, ${seed.fingerprint + '-' + suffix},
				${'affected seed ' + seed.fingerprint}, ${'SeedError'}, ${'unresolved'}, ${'system_error'},
				${seed.firstSeen}, ${seed.occurrenceAt ?? seed.firstSeen}, ${seed.userCount}, ${seed.tenantCount}
			)
		`;
		if (seed.occurrenceAt) {
			await sql`
				insert into error_occurrences (issue_id, environment, platform, created_at)
				values (${id}, ${'production'}, ${'go'}, ${seed.occurrenceAt})
			`;
		}
		for (const user of seed.users) {
			await sql`
				insert into issue_affected_users (issue_id, user_id, first_seen, last_seen)
				values (${id}, ${user.id}, ${user.lastSeen}, ${user.lastSeen})
			`;
		}
		for (const tenant of seed.tenants) {
			await sql`
				insert into issue_affected_tenants (issue_id, tenant_id, first_seen, last_seen)
				values (${id}, ${tenant.id}, ${tenant.lastSeen}, ${tenant.lastSeen})
			`;
		}
		return id;
	}

	async function countsOf(issueId: string) {
		const rows = await sql<
			{ affected_user_count: number; affected_tenant_count: number; users: string; tenants: string }[]
		>`
			select i.affected_user_count, i.affected_tenant_count,
			       (select count(*) from issue_affected_users a where a.issue_id = i.id) as users,
			       (select count(*) from issue_affected_tenants t where t.issue_id = i.id) as tenants
			from issues i where i.id = ${issueId}
		`;
		expect(rows).toHaveLength(1);
		return {
			userCount: rows[0].affected_user_count,
			tenantCount: rows[0].affected_tenant_count,
			userRows: Number(rows[0].users),
			tenantRows: Number(rows[0].tenants),
		};
	}

	async function remainingUserIds(issueId: string): Promise<string[]> {
		const rows = await sql<{ user_id: string }[]>`
			select user_id from issue_affected_users where issue_id = ${issueId} order by user_id
		`;
		return rows.map((r) => r.user_id);
	}

	async function remainingTenantIds(issueId: string): Promise<string[]> {
		const rows = await sql<{ tenant_id: string }[]>`
			select tenant_id from issue_affected_tenants where issue_id = ${issueId} order by tenant_id
		`;
		return rows.map((r) => r.tenant_id);
	}

	async function agedRowsInTable(table: 'issue_affected_users' | 'issue_affected_tenants') {
		const rows = await sql<{ n: string }[]>`
			select count(*) as n from ${sql(table)} where last_seen < ${daysAgo(30)}
		`;
		return Number(rows[0].n);
	}

	it('R1–R3: deletes the aged side rows, recomputes the touched counts to the remaining cardinality, leaves an untouched issue alone, cascades an orphan, and reports side rows not issues', async () => {
		const { cleanupRetainedData } = await import('./retention');
		installTransactionSeam();

		// A: touched. Two of three users and one of two tenants are aged. A fresh occurrence keeps
		// it out of the orphan rule.
		const issueA = await seedIssue({
			fingerprint: 'touched-a',
			firstSeen: daysAgo(60),
			occurrenceAt: fresh(),
			users: [
				{ id: 'a-old-1', lastSeen: aged() },
				{ id: 'a-old-2', lastSeen: aged() },
				{ id: 'a-new', lastSeen: fresh() },
			],
			tenants: [
				{ id: 'a-t-old', lastSeen: aged() },
				{ id: 'a-t-new', lastSeen: fresh() },
			],
			userCount: 3,
			tenantCount: 2,
		});
		// D: touched, and loses EVERY user (the count goes to 0 -- decision 12, a windowed count
		// may go all the way down) while keeping one tenant.
		const issueD = await seedIssue({
			fingerprint: 'touched-d',
			firstSeen: daysAgo(60),
			occurrenceAt: fresh(),
			users: [
				{ id: 'd-old-1', lastSeen: aged() },
				{ id: 'd-old-2', lastSeen: aged() },
				{ id: 'd-old-3', lastSeen: aged() },
			],
			tenants: [
				{ id: 'd-t-old-1', lastSeen: aged() },
				{ id: 'd-t-old-2', lastSeen: aged() },
				{ id: 'd-t-new', lastSeen: fresh() },
			],
			userCount: 3,
			tenantCount: 3,
		});
		// B: untouched -- every side row is fresh, so statement 1 never locks it. Its counts are
		// seeded at 7, which is NOT its cardinality (2 / 1): if the recompute ran over every issue
		// instead of the locked set, B would read 2 / 1 afterwards and this test would say so.
		const issueB = await seedIssue({
			fingerprint: 'untouched-b',
			firstSeen: daysAgo(60),
			occurrenceAt: fresh(),
			users: [
				{ id: 'b-new-1', lastSeen: fresh() },
				{ id: 'b-new-2', lastSeen: fresh() },
			],
			tenants: [{ id: 'b-t-new', lastSeen: fresh() }],
			userCount: 7,
			tenantCount: 7,
		});
		// C: orphaned by the existing rule (no occurrences, first_seen older than the cutoff). Its
		// side rows are FRESH, so the side-table step does not touch them and they cannot be in
		// deletedAffected*; only the FK cascade from the orphan delete can remove them (R2).
		const issueC = await seedIssue({
			fingerprint: 'orphan-c',
			firstSeen: daysAgo(60),
			occurrenceAt: null,
			users: [{ id: 'c-new', lastSeen: fresh() }],
			tenants: [{ id: 'c-t-new', lastSeen: fresh() }],
			userCount: 1,
			tenantCount: 1,
		});

		// The whole table's aged rows as they stand right before the call: this file's 5 users /
		// 3 tenants, plus anything ambient. The result must equal THIS -- rows -- and not the number
		// of issues those rows belong to (2 here, for both tables).
		const agedUsersBefore = await agedRowsInTable('issue_affected_users');
		const agedTenantsBefore = await agedRowsInTable('issue_affected_tenants');
		expect(agedUsersBefore).toBeGreaterThanOrEqual(5);
		expect(agedTenantsBefore).toBeGreaterThanOrEqual(3);

		const result = await cleanupRetainedData(30, 365, 30);

		// R3: side rows deleted, not issues touched.
		expect(result.deletedAffectedUsers).toBe(agedUsersBefore);
		expect(result.deletedAffectedTenants).toBe(agedTenantsBefore);
		expect(result.deletedAffectedUsers).not.toBe(2);
		expect(result.deletedAffectedTenants).not.toBe(2);

		// R1: every aged row is gone, every fresh row remains.
		expect(await remainingUserIds(issueA)).toEqual(['a-new']);
		expect(await remainingTenantIds(issueA)).toEqual(['a-t-new']);
		expect(await remainingUserIds(issueD)).toEqual([]);
		expect(await remainingTenantIds(issueD)).toEqual(['d-t-new']);

		// The decision-12 invariant on every touched issue: count = remaining cardinality.
		const a = await countsOf(issueA);
		expect(a).toEqual({ userCount: 1, tenantCount: 1, userRows: 1, tenantRows: 1 });
		const d = await countsOf(issueD);
		expect(d).toEqual({ userCount: 0, tenantCount: 1, userRows: 0, tenantRows: 1 });

		// The untouched issue keeps its (deliberately wrong) seeded counts: not recomputed.
		const b = await countsOf(issueB);
		expect(b).toEqual({ userCount: 7, tenantCount: 7, userRows: 2, tenantRows: 1 });

		// R2: the orphan is gone and took its side rows with it by cascade.
		expect(result.deletedOrphanedIssues).toBeGreaterThanOrEqual(1);
		expect(await sql`select id from issues where id = ${issueC}`).toHaveLength(0);
		expect(await sql`select user_id from issue_affected_users where issue_id = ${issueC}`).toHaveLength(0);
		expect(await sql`select tenant_id from issue_affected_tenants where issue_id = ${issueC}`).toHaveLength(0);

		// Decision 23's statement order, read off the transactions the sweep actually opened: one
		// per side table, three statements each, the lock SELECT first, then the DELETE, then the
		// recompute UPDATE -- and the DELETE and UPDATE both scoped to the locked ids (decision 28).
		expect(recordedTransactions).toHaveLength(2);
		for (const statements of recordedTransactions) {
			expect(statements).toHaveLength(3);
			expect(statements[0]).toMatch(
				/^select "issues"\."id" as id from "issues" where "issues"\."id" in \( ?select distinct issue_id from "issue_affected_(users|tenants)" where last_seen < \$\d+::timestamptz ?\) order by "issues"\."id" for update$/i
			);
			expect(statements[1]).toMatch(
				/^delete from "issue_affected_(users|tenants)" where last_seen < \$\d+::timestamptz and issue_id = any\(\$\d+::uuid\[\]\) returning issue_id$/i
			);
			expect(statements[2]).toMatch(
				/^update "issues" set "affected_(user|tenant)_count" = \( ?select count\(\*\) from "issue_affected_(users|tenants)" a where a\.issue_id = "issues"\."id" ?\) where "issues"\."id" = any\(\$\d+::uuid\[\]\)$/i
			);
		}
		expect(recordedTransactions[0][1]).toContain('"issue_affected_users"');
		expect(recordedTransactions[1][1]).toContain('"issue_affected_tenants"');
	});

	it('R1 (concurrent): an ingest for a touched issue waits on the issues row lock and completes without 40P01', async () => {
		const { cleanupRetainedData } = await import('./retention');
		installTransactionSeam();

		// One aged user, one fresh. The ingest below upserts THE AGED user again (a returning user
		// whose new occurrence is inside the window) -- the row the sweep is deleting. Under the
		// contract's lock order the ingest queues on the issues row before it touches that side row;
		// under the first draft's order (side rows first, issues second) this exact interleaving is
		// a cycle, and Postgres would resolve it with 40P01 on one side.
		const issueId = await seedIssue({
			fingerprint: 'concurrent',
			firstSeen: daysAgo(60),
			occurrenceAt: fresh(),
			users: [
				{ id: 'k-old', lastSeen: aged() },
				{ id: 'k-new', lastSeen: fresh() },
			],
			tenants: [{ id: 'k-t-new', lastSeen: fresh() }],
			userCount: 2,
			tenantCount: 1,
		});

		let ingest: Promise<void> | null = null;
		let ingestSettled = false;
		let ingestError: unknown = null;

		// Fired once, after the users transaction's FIRST statement -- i.e. while the sweep holds
		// the issues row lock and has deleted nothing yet. StoreEvent's order, on a separate
		// connection: the issues row first (its upsert / regression UPDATE), then the side-table
		// upsert, then the count bump when the upsert INSERTed (§4.3).
		executeHook = async (statementIndex) => {
			if (statementIndex !== 1) return;
			executeHook = null;
			ingest = sql
				.begin(async (s) => {
					await s`update issues set last_seen = now() where id = ${issueId}`;
					const upserted = await s<{ inserted: boolean }[]>`
						insert into issue_affected_users (issue_id, user_id, first_seen, last_seen)
						values (${issueId}, ${'k-old'}, now(), now())
						on conflict (issue_id, user_id) do update
						   set last_seen = greatest(issue_affected_users.last_seen, excluded.last_seen),
						       first_seen = least(issue_affected_users.first_seen, excluded.first_seen)
						returning (xmax = 0) as inserted
					`;
					if (upserted[0].inserted) {
						await s`update issues set affected_user_count = affected_user_count + 1 where id = ${issueId}`;
					}
				})
				.then(
					() => {
						ingestSettled = true;
					},
					(err: unknown) => {
						ingestSettled = true;
						ingestError = err;
					}
				);
			// Give the ingest time to reach the lock and prove it is genuinely waiting on it: if
			// the sweep did not hold the issues row, this would already be settled.
			await new Promise((resolve) => setTimeout(resolve, 400));
			expect(ingestSettled).toBe(false);
		};

		const result = await cleanupRetainedData(30, 365, 30);
		expect(ingest).not.toBeNull();
		await ingest;

		// The ingest went through -- no deadlock, no error of any kind.
		expect(ingestError).toBeNull();
		expect(ingestSettled).toBe(true);
		expect(result.deletedAffectedUsers).toBeGreaterThanOrEqual(1);

		// And the invariant holds across the two commits: the sweep deleted k-old and recomputed
		// to 1 (k-new); the ingest, released after the commit, found no k-old row, inserted it and
		// bumped -- 2 rows, count 2.
		expect(await remainingUserIds(issueId)).toEqual(['k-new', 'k-old']);
		expect(await countsOf(issueId)).toEqual({
			userCount: 2,
			tenantCount: 1,
			userRows: 2,
			tenantRows: 1,
		});
	});

	it('R4: a failure after the DELETE rolls the step back -- rows and counts exactly as before', async () => {
		const { cleanupRetainedData } = await import('./retention');
		installTransactionSeam();

		const issueId = await seedIssue({
			fingerprint: 'fault',
			firstSeen: daysAgo(60),
			occurrenceAt: fresh(),
			users: [
				{ id: 'f-old-1', lastSeen: aged() },
				{ id: 'f-old-2', lastSeen: aged() },
				{ id: 'f-new', lastSeen: fresh() },
			],
			tenants: [
				{ id: 'f-t-old', lastSeen: aged() },
				{ id: 'f-t-new', lastSeen: fresh() },
			],
			userCount: 3,
			tenantCount: 2,
		});
		const before = await countsOf(issueId);
		expect(before).toEqual({ userCount: 3, tenantCount: 2, userRows: 3, tenantRows: 2 });

		// After the DELETE has run and returned its rows -- the exact window in which a non-atomic
		// step would leave "rows gone, count stale".
		let faultsInjected = 0;
		executeHook = async (statementIndex, sqlText) => {
			if (statementIndex !== 2) return;
			expect(sqlText).toMatch(/^delete from "issue_affected_users"/i);
			faultsInjected += 1;
			throw new Error('injected fault after the side-table DELETE');
		};

		await expect(cleanupRetainedData(30, 365, 30)).rejects.toThrow(
			'injected fault after the side-table DELETE'
		);
		expect(faultsInjected).toBe(1);
		// The tenants transaction was never opened: the throw left cleanupRetainedData entirely.
		expect(recordedTransactions).toHaveLength(1);

		// Nothing moved: not a row, not a count.
		expect(await remainingUserIds(issueId)).toEqual(['f-new', 'f-old-1', 'f-old-2']);
		expect(await remainingTenantIds(issueId)).toEqual(['f-t-new', 'f-t-old']);
		expect(await countsOf(issueId)).toEqual(before);

		// And with the seam removed the next run does the same work and lands it (the contract's
		// "the next hourly run does the same work").
		removeTransactionSeam();
		const retry = await cleanupRetainedData(30, 365, 30);
		expect(retry.deletedAffectedUsers).toBeGreaterThanOrEqual(2);
		expect(retry.deletedAffectedTenants).toBeGreaterThanOrEqual(1);
		expect(await remainingUserIds(issueId)).toEqual(['f-new']);
		expect(await countsOf(issueId)).toEqual({
			userCount: 1,
			tenantCount: 1,
			userRows: 1,
			tenantRows: 1,
		});
	});
});
