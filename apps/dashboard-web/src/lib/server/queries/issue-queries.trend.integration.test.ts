import { describe, it, expect, beforeAll, afterAll, beforeEach, afterEach } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';
import { db } from '$lib/server/db';
import { detailTrendWindow, listTrendWindow } from '$lib/server/trend-window';

/**
 * sentinel-issue-trend §4.1 / §4.2 — contract §11's `listIssues` / `countIssues` boundary for the
 * two new orders, the `new=24h` filter and the two projection fields (decisions 5, 6, 22), and the
 * two bucket readers' boundary (`trendForIssues`, `trendForIssue`), against a REAL migrated
 * Postgres. Every claim below is a claim about a query's SQL — the `ORDER BY` `sort=count` produces
 * (`count DESC, last_seen DESC, id DESC`, with a seed that ties on BOTH leading keys), that
 * `sort=first_seen` is `first_seen DESC NULLS LAST, id DESC` with a null-`first_seen` row SEEDED and
 * asserted last (Drizzle's `desc()` is a bare `DESC`, which PostgreSQL reads as `NULLS FIRST`), that
 * `new=24h` narrows the list AND the count through the shared builder with the project and
 * environment guards still in the `WHERE`, and that both readers are dense, keyed on the ids they
 * were given, and bounded at BOTH ends. A mocked query layer cannot tell a composed predicate from
 * a constructed-and-dropped one; only the rows can say.
 *
 * `issue-queries.sort.integration.test.ts`'s style (the affected-users member of this family):
 * seeds its OWN organizations, projects, users, grants, issues and bucket rows under a per-run
 * suffix and deletes them again, so nothing here depends on ambient data and nothing it asserts
 * can be satisfied or masked by another suite's rows.
 *
 * Point DATABASE_URL at a DISPOSABLE database migrated by `packages/db-migrations` through
 * 1724800000 — never the shared dev one (CLAUDE.md):
 *
 *   docker run -d --name sentinel-trend-pg -p 15803:5432 \
 *     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && GOWORK=off \
 *     DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15803/sentinel?sslmode=disable \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15803/sentinel \
 *     pnpm vitest run src/lib/server/queries/issue-queries.trend.integration.test.ts
 *
 * Skipped when nothing answers, so a machine with no Postgres is not broken by it. Set
 * SCHEMA_DRIFT_REQUIRED=1 (the same switch CI's dashboard job sets for the DB-backed suites) or
 * READ_GUARDS_INTEGRATION_REQUIRED=1 to make "no reachable database" a hard failure.
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required =
	process.env.SCHEMA_DRIFT_REQUIRED === '1' ||
	process.env.READ_GUARDS_INTEGRATION_REQUIRED === '1';

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
		`SCHEMA_DRIFT_REQUIRED=1 but no database answered at ${connectionString}. ` +
			'This test is the only coverage that runs sort=count, sort=first_seen, new=24h and the two ' +
			'bucket readers against real Postgres; skipping it silently would leave §4.1/§4.2 unguarded.'
	);
}

// The retention.*.integration family sweeps the WHOLE database — retention.claims with a ONE-day
// cutoff — and vitest runs files in parallel workers. This seed has issues whose `first_seen` is
// older than a day with no occurrences behind them (the orphan rule's shape) and bucket rows 29
// days old (the §4.5 sweep's shape, once it lands), so it takes the family's session-level
// advisory lock for its whole run: no sweep runs while these rows must still exist. The key is the
// one every member of that family uses.
const RETENTION_FAMILY_LOCK = 'sentinel.retention.integration';

const HOUR = 60 * 60 * 1000;
const DAY = 24 * HOUR;

// ONE instant for the whole file, the way the loaders take one per request. Every seeded timestamp
// and every window below derives from it, so the assertions do not depend on the wall clock
// crossing an hour or a UTC midnight while the file runs.
const fixtureNow = new Date();
const list = listTrendWindow(fixtureNow); // [trunc(now) − 23h … trunc(now)]
const detail = detailTrendWindow(fixtureNow); // [00:00Z of (UTC today − 29d) … trunc(now)]
const truncNow = list.windowEnd;
const at = (ms: number) => new Date(ms);
const hoursAgo = (h: number) => at(fixtureNow.getTime() - h * HOUR);

const suffix = randomUUID().slice(0, 8);

const orgOwn = randomUUID();
const orgForeign = randomUUID();
const projectOwn = randomUUID();
// A project in a different organization: its issue carries the LARGEST count and a `first_seen`
// inside the 24-hour window, so if the tenancy predicate ever fell out of either query it would land
// at the TOP of `sort=count`, at the top of `sort=first_seen`, and inside `new=24h`.
const projectForeign = randomUUID();

// The unrestricted member: an org `engineer` with no grant row — sees every environment of
// `projectOwn`. The narrowed member is granted `{production}` only, so the `staging` issue — the
// largest count they could reach, and the newest `first_seen` — must never appear for them.
const openUser = randomUUID();
const narrowedUser = randomUUID();

// ---------------------------------------------------------------------------------------------
// The list/count seed. `count`, `last_seen` and `first_seen` per issue, relative to `fixtureNow`.
//
//   sort=count (open user):  staging(500) > D(40) > C(10, −1h) > {A, B}(10, −3h: tie on BOTH
//                            leading keys, id DESC decides) > F(3) > E(1)
//   sort=first_seen (open):  staging(−30m) > A(−1h) > C(−2h) > E(−20h) > B(−30h) > F(−48h) > D(NULL)
//   new=24h (open):          {staging, A, C, E} — first_seen within 24h; B, F outside; D null
//
// D is the second-highest count and has NO first_seen (inserted as an explicit NULL — the column
// defaults to NOW(), so an omitted value would silently make it the newest); under `sort=first_seen`
// it must be LAST, which is what NULLS LAST means and what a bare DESC gets wrong. F is the one
// regressed issue (decision 22's projection fields).
// ---------------------------------------------------------------------------------------------
const issueStagingBig = randomUUID();
const issueA = randomUUID();
const issueB = randomUUID();
const issueC = randomUUID();
const issueD = randomUUID();
const issueE = randomUUID();
const issueF = randomUUID();
const issueForeignHuge = randomUUID();

type SeedRow = [
	id: string,
	project: string,
	environment: string,
	count: number,
	lastSeen: Date,
	firstSeen: Date | null,
];
// The tie pair is inserted with the SMALLER id first, so the heap order is the OPPOSITE of
// `id DESC`: an `ORDER BY count DESC, last_seen DESC` with the third key missing returns the
// tied rows in heap order on a table this small, which is exactly the order the assertion
// rejects. Inserted in a random order the mutation passes half the time (it did).
const tiePair: SeedRow[] = (
	[
		[issueA, projectOwn, 'production', 10, hoursAgo(3), hoursAgo(1)],
		[issueB, projectOwn, 'production', 10, hoursAgo(3), hoursAgo(30)],
	] as SeedRow[]
).sort((x, y) => (x[0] < y[0] ? -1 : 1));
const SEED: SeedRow[] = [
	[issueStagingBig, projectOwn, 'staging', 500, hoursAgo(2), hoursAgo(0.5)],
	...tiePair,
	[issueC, projectOwn, 'production', 10, hoursAgo(1), hoursAgo(2)],
	[issueD, projectOwn, 'production', 40, hoursAgo(10), null],
	[issueE, projectOwn, 'production', 1, hoursAgo(0.5), hoursAgo(20)],
	[issueF, projectOwn, 'production', 3, hoursAgo(6), hoursAgo(48)],
	[issueForeignHuge, projectForeign, 'production', 100_000, hoursAgo(1), hoursAgo(1)],
];
const regressedAt = hoursAgo(1);

// The tie-broken pair, in `id DESC` order: uuids compare as their 128-bit value, which for the
// `postgres` driver's text form is the same as a byte-wise comparison of the canonical lowercase
// hex string.
const [tieFirst, tieSecond] = issueA > issueB ? [issueA, issueB] : [issueB, issueA];

const COUNT_ORDER_OPEN = [issueStagingBig, issueD, issueC, tieFirst, tieSecond, issueF, issueE];
const COUNT_ORDER_NARROWED = [issueD, issueC, tieFirst, tieSecond, issueF, issueE];
const FIRST_SEEN_ORDER_OPEN = [issueStagingBig, issueA, issueC, issueE, issueB, issueF, issueD];
const FIRST_SEEN_ORDER_NARROWED = [issueA, issueC, issueE, issueB, issueF, issueD];
const NEW_24H_OPEN = [issueStagingBig, issueA, issueC, issueE].sort();
const NEW_24H_NARROWED = [issueA, issueC, issueE].sort();
const VISIBLE_TO_OPEN = [...COUNT_ORDER_OPEN].sort();
const VISIBLE_TO_NARROWED = [...COUNT_ORDER_NARROWED].sort();

// ---------------------------------------------------------------------------------------------
// The bucket seed, for the two readers. Three issues in `projectOwn`: `busy` has rows placed on
// the window edges; `quiet` has none; `outsider` has a row at the current hour and is never in
// `ids`. Every `bucket_start` is a UTC hour start, as the processor writes them.
//
//   busy:  trunc(now)          5   → list[23], hourly[23], daily[29]
//          trunc(now) − 3h     2   → list[20], hourly[20]
//          trunc(now) − 23h    1   → list[0],  hourly[0]
//          trunc(now) − 24h    9   → NOT in the 24-hour series (one hour before its start); in daily
//          trunc(now) + 1h  1000   → in NOTHING: decision 18's future-dated row, past windowEnd
//          windowStart          7   → daily[0]  (00:00Z of the first day)
//          windowStart + 23h    3   → daily[0]  (23:00Z of the first day — same UTC day)
//          windowStart + 24h    4   → daily[1]  (00:00Z of the second day — the boundary case)
//          windowStart − 1h   100   → in NOTHING: before the 30-day window
// ---------------------------------------------------------------------------------------------
const issueBusy = randomUUID();
const issueQuiet = randomUUID();
const issueOutsider = randomUUID();

const BUCKETS: Array<[issueId: string, bucketStart: Date, count: number]> = [
	[issueBusy, truncNow, 5],
	[issueBusy, at(truncNow.getTime() - 3 * HOUR), 2],
	[issueBusy, at(truncNow.getTime() - 23 * HOUR), 1],
	[issueBusy, at(truncNow.getTime() - 24 * HOUR), 9],
	[issueBusy, at(truncNow.getTime() + 1 * HOUR), 1000],
	[issueBusy, detail.windowStart, 7],
	[issueBusy, at(detail.windowStart.getTime() + 23 * HOUR), 3],
	[issueBusy, at(detail.windowStart.getTime() + 24 * HOUR), 4],
	[issueBusy, at(detail.windowStart.getTime() - 1 * HOUR), 100],
	[issueOutsider, truncNow, 77],
];
// What the 30-day window holds for `busy`: everything except the −1h-before-window and the +1h rows.
const BUSY_30D_TOTAL = 5 + 2 + 1 + 9 + 7 + 3 + 4;
// What the 24-hour window holds: the three rows in [trunc(now) − 23h … trunc(now)].
const BUSY_24H_TOTAL = 5 + 2 + 1;

const ids = (rows: { id: string }[]) => rows.map((r) => r.id);
const iso = (d: Date) => d.toISOString().slice(0, 19) + 'Z';

// ---------------------------------------------------------------------------------------------
// The recorded db layer: `db.session.logger` is what every Drizzle query — `select`, `execute`,
// a transaction's statements — reports its SQL to (retention.affected.integration.test.ts records
// `tx.execute` the same way, one layer up). Replacing it here is how "issued no query" becomes an
// assertion rather than an absence of an error: a reader that queried with `inArray([])` would
// throw, but a reader that queried `WHERE false` would not, and only the log can tell the two
// apart from "did not query".
// ---------------------------------------------------------------------------------------------
type QueryLogger = { logQuery(query: string, params: unknown[]): void };
const session = (db as unknown as { session: { logger: QueryLogger } }).session;
const realLogger = session.logger;
let recordedQueries: string[] = [];

describe.skipIf(!dbReachable)('listIssues / countIssues / bucket readers (sentinel-issue-trend, integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		await sql`select pg_advisory_lock(hashtext(${RETENTION_FAMILY_LOCK}))`;

		for (const [id, label] of [
			[orgOwn, 'own'],
			[orgForeign, 'foreign'],
		] as const) {
			await sql`
				insert into organizations (id, name, slug)
				values (${id}, ${`trend-${label}-${suffix}`}, ${`trend-${label}-${suffix}`})
			`;
		}
		for (const [id, org, label] of [
			[projectOwn, orgOwn, 'own'],
			[projectForeign, orgForeign, 'foreign'],
		] as const) {
			await sql`
				insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${id}, ${`trend-${label}-${suffix}`}, ${org},
				        ${`k_trend_${label}_${suffix}`}, ${`h_trend_${label}_${suffix}`})
			`;
		}
		for (const [id, label] of [
			[openUser, 'open'],
			[narrowedUser, 'narrowed'],
		] as const) {
			await sql`
				insert into "user" (id, name, email)
				values (${id}, ${label}, ${`trend-${label}-${suffix}@example.test`})
			`;
			await sql`
				insert into organization_members (organization_id, user_id, role)
				values (${orgOwn}, ${id}, ${'engineer'})
			`;
		}
		await sql`
			insert into project_environment_grants (user_id, project_id, environments, created_by)
			values (${narrowedUser}, ${projectOwn}, ${sql.array(['production'])}, ${openUser})
		`;

		// `count` and `first_seen` are written directly: this file asserts the ORDER and the FILTER the
		// columns produce, not the write path behind them (that is `tests/integration`'s). `first_seen`
		// is set explicitly on every row, NULL included — the column's default is NOW().
		for (const [id, projectId, environment, count, lastSeen, firstSeen] of SEED) {
			await sql`
				insert into issues (id, project_id, environment, fingerprint, message, error_class,
				                    count, last_seen, first_seen)
				values (${id}, ${projectId}, ${environment}, ${`fp-${id}`},
				        ${`trend seed ${suffix}`}, ${'TrendSeedError'},
				        ${count}, ${lastSeen.toISOString()},
				        ${firstSeen ? firstSeen.toISOString() : null})
			`;
		}
		await sql`
			update issues set regression_status = 'regressed', last_regressed_at = ${regressedAt.toISOString()}
			where id = ${issueF}
		`;

		for (const [id, label] of [
			[issueBusy, 'busy'],
			[issueQuiet, 'quiet'],
			[issueOutsider, 'outsider'],
		] as const) {
			await sql`
				insert into issues (id, project_id, environment, fingerprint, message, error_class, count)
				values (${id}, ${projectOwn}, ${'production'}, ${`fp-${label}-${id}`},
				        ${`trend bucket seed ${suffix}`}, ${'TrendBucketSeedError'}, ${1})
			`;
		}
		for (const [issueId, bucketStart, count] of BUCKETS) {
			await sql`
				insert into issue_occurrence_buckets (issue_id, bucket_start, count)
				values (${issueId}, ${bucketStart.toISOString()}, ${count})
			`;
		}
		// 120 s, as every other holder of RETENTION_FAMILY_LOCK sets it: under the full parallel run
		// this hook queues behind a sibling file's entire seed-and-sweep run, and vitest's default
		// 10 s hook timeout turned that wait into "Hook timed out" one run in two.
	}, 120_000);

	afterAll(async () => {
		if (!sql) return;
		try {
			// Buckets go with their issues (ON DELETE CASCADE); listed for the reader.
			await sql`delete from issues where project_id in (${projectOwn}, ${projectForeign})`;
			await sql`delete from project_environment_grants where user_id = ${narrowedUser}`;
			await sql`delete from organization_members where organization_id = ${orgOwn}`;
			await sql`delete from projects where organization_id in (${orgOwn}, ${orgForeign})`;
			await sql`delete from "user" where id in (${openUser}, ${narrowedUser})`;
			await sql`delete from organizations where id in (${orgOwn}, ${orgForeign})`;
		} finally {
			await sql`select pg_advisory_unlock_all()`.catch(() => {});
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	beforeEach(() => {
		recordedQueries = [];
		session.logger = {
			logQuery(query) {
				recordedQueries.push(query.replace(/\s+/g, ' ').trim());
			},
		};
	});

	afterEach(() => {
		session.logger = realLogger;
	});

	/** The filters the `/` loader builds, resolved the way the loader resolves them. */
	async function loaderFilters(userId: string, overrides: Record<string, unknown> = {}) {
		const { getAccessibleProjects } = await import('../issue-access');
		const { resolveEnvironmentScopes } = await import('../environment-access');
		return {
			status: 'all',
			projectId: 'all',
			projectIds: (await getAccessibleProjects(userId)).map((p) => p.id),
			environment: 'all',
			environmentScopes: await resolveEnvironmentScopes(userId),
			...overrides,
		};
	}

	function isNonIncreasing(values: number[]) {
		return values.every((v, i) => i === 0 || values[i - 1] >= v);
	}

	// The three bucket-seed issues are in `projectOwn` too; the list/count assertions scope to the
	// seven `SEED` rows by id so the two seeds do not count each other.
	const listSeedOnly = <T extends { id: string }>(rows: T[]) =>
		rows.filter((r) => SEED.some(([id]) => id === r.id));

	// ---------------------------------------------------------------------------------------------
	// The population first: with no `sort` and no `new`, the open user sees exactly the seven
	// own-project issues. Asserted before any order claim, because an order over an empty result
	// is true of anything.
	// ---------------------------------------------------------------------------------------------

	it('the open member sees exactly the seven own-project seed issues, and the foreign one never', async () => {
		const { issueQueries } = await import('./issue-queries');

		const rows = listSeedOnly(await issueQueries.listIssues(await loaderFilters(openUser), 1, 100));

		expect(ids(rows).sort()).toEqual(VISIBLE_TO_OPEN);
		expect(ids(rows)).not.toContain(issueForeignHuge);
		// The seed is what the file says it is: D's first_seen is NULL, not a default NOW().
		expect(rows.find((r) => r.id === issueD)?.firstSeen).toBeNull();
	});

	// ---------------------------------------------------------------------------------------------
	// §4.1's `count` order — all three keys (decision 5).
	// ---------------------------------------------------------------------------------------------

	it('sort=count orders count DESC, last_seen DESC, id DESC — the tie on both leading keys broken by id', async () => {
		const { issueQueries } = await import('./issue-queries');

		const rows = listSeedOnly(
			await issueQueries.listIssues(await loaderFilters(openUser, { sort: 'count' }), 1, 100)
		);

		expect(ids(rows)).toEqual(COUNT_ORDER_OPEN);
		// Said in the columns' terms too: counts never go up down the page; C (10, −1h) precedes the
		// two 10s at −3h; those two tie on last_seen and are broken by id, descending.
		expect(isNonIncreasing(rows.map((r) => r.count))).toBe(true);
		const tied = rows.filter((r) => r.id === issueA || r.id === issueB);
		expect(tied[0].lastSeen?.getTime()).toBe(tied[1].lastSeen?.getTime());
		expect(tied[0].count).toBe(tied[1].count);
		expect(tied[0].id > tied[1].id).toBe(true);
	});

	it('sort=count never returns a foreign-project issue or an out-of-scope environment — the guards are in the WHERE', async () => {
		const { issueQueries } = await import('./issue-queries');

		for (const user of [openUser, narrowedUser]) {
			const rows = await issueQueries.listIssues(await loaderFilters(user, { sort: 'count' }), 1, 100);
			expect(ids(rows)).not.toContain(issueForeignHuge);
		}
		const narrowed = listSeedOnly(
			await issueQueries.listIssues(await loaderFilters(narrowedUser, { sort: 'count' }), 1, 100)
		);
		expect(ids(narrowed)).toEqual(COUNT_ORDER_NARROWED);
		expect(ids(narrowed)).not.toContain(issueStagingBig);
	});

	// ---------------------------------------------------------------------------------------------
	// §4.1's `first_seen` order — `DESC NULLS LAST, id DESC`, with the null row SEEDED (§3).
	// ---------------------------------------------------------------------------------------------

	it('sort=first_seen orders first_seen DESC NULLS LAST, id DESC — the null-first_seen row is LAST', async () => {
		const { issueQueries } = await import('./issue-queries');

		const rows = listSeedOnly(
			await issueQueries.listIssues(await loaderFilters(openUser, { sort: 'first_seen' }), 1, 100)
		);

		expect(ids(rows)).toEqual(FIRST_SEEN_ORDER_OPEN);
		// The property a bare DESC breaks: the row with no first_seen is at the END, and every row
		// before it is newer than the one after it.
		expect(rows.at(-1)?.id).toBe(issueD);
		expect(rows.at(-1)?.firstSeen).toBeNull();
		const dated = rows.slice(0, -1).map((r) => {
			if (!r.firstSeen) throw new Error('a dated row came back null');
			return r.firstSeen.getTime();
		});
		expect(isNonIncreasing(dated)).toBe(true);
	});

	it('sort=first_seen keeps the guards in the WHERE — foreign never, staging never for the narrowed member', async () => {
		const { issueQueries } = await import('./issue-queries');

		const rows = listSeedOnly(
			await issueQueries.listIssues(await loaderFilters(narrowedUser, { sort: 'first_seen' }), 1, 100)
		);

		expect(ids(rows)).toEqual(FIRST_SEEN_ORDER_NARROWED);
		expect(ids(rows)).not.toContain(issueForeignHuge);
		expect(ids(rows)).not.toContain(issueStagingBig);
		expect(rows.at(-1)?.id).toBe(issueD);
	});

	// ---------------------------------------------------------------------------------------------
	// `new=24h` (decision 6): a FILTER through the shared builder, so the list and the count narrow
	// to the same set, with the project and environment guards still in the WHERE.
	// ---------------------------------------------------------------------------------------------

	it('new=24h narrows listIssues AND countIssues to the same set, guards intact, for the open member', async () => {
		const { issueQueries } = await import('./issue-queries');
		const firstSeenAfter = hoursAgo(24);

		const rows = listSeedOnly(
			await issueQueries.listIssues(await loaderFilters(openUser, { firstSeenAfter }), 1, 100)
		);
		const total = await issueQueries.countIssues(await loaderFilters(openUser, { firstSeenAfter }));

		expect(ids(rows).sort()).toEqual(NEW_24H_OPEN);
		// The count is over the WHOLE guarded set for this user — the seven seed rows plus the three
		// bucket-seed issues (first_seen NOW(), so inside the window) — and it must equal what the
		// list returns for the same filters, unpaged.
		const listedAll = await issueQueries.listIssues(await loaderFilters(openUser, { firstSeenAfter }), 1, 100);
		expect(total).toBe(listedAll.length);
		expect(ids(listedAll)).not.toContain(issueForeignHuge);
		expect(ids(listedAll)).not.toContain(issueB);
		expect(ids(listedAll)).not.toContain(issueF);
		expect(ids(listedAll)).not.toContain(issueD);
	});

	it('new=24h keeps the environment guard: the narrowed member does not get the new staging issue', async () => {
		const { issueQueries } = await import('./issue-queries');
		const firstSeenAfter = hoursAgo(24);

		const rows = listSeedOnly(
			await issueQueries.listIssues(await loaderFilters(narrowedUser, { firstSeenAfter }), 1, 100)
		);
		const total = await issueQueries.countIssues(await loaderFilters(narrowedUser, { firstSeenAfter }));
		const listedAll = await issueQueries.listIssues(await loaderFilters(narrowedUser, { firstSeenAfter }), 1, 100);

		expect(ids(rows).sort()).toEqual(NEW_24H_NARROWED);
		expect(ids(listedAll)).not.toContain(issueStagingBig);
		expect(ids(listedAll)).not.toContain(issueForeignHuge);
		expect(total).toBe(listedAll.length);
	});

	it('new=24h composes with the sorts and with the environment FILTER', async () => {
		const { issueQueries } = await import('./issue-queries');
		const firstSeenAfter = hoursAgo(24);

		const byCount = listSeedOnly(
			await issueQueries.listIssues(await loaderFilters(openUser, { firstSeenAfter, sort: 'count' }), 1, 100)
		);
		expect(ids(byCount)).toEqual([issueStagingBig, issueC, issueA, issueE]);

		const byFirstSeen = listSeedOnly(
			await issueQueries.listIssues(await loaderFilters(openUser, { firstSeenAfter, sort: 'first_seen' }), 1, 100)
		);
		expect(ids(byFirstSeen)).toEqual([issueStagingBig, issueA, issueC, issueE]);

		const staging = await issueQueries.listIssues(
			await loaderFilters(openUser, { firstSeenAfter, projectId: projectOwn, environment: 'staging' }),
			1,
			100
		);
		expect(ids(staging)).toEqual([issueStagingBig]);
		expect(
			await issueQueries.countIssues(
				await loaderFilters(openUser, { firstSeenAfter, projectId: projectOwn, environment: 'staging' })
			)
		).toBe(1);
	});

	// An unknown `new` is coerced to `all` by the loader, which passes NO bound: the query layer's
	// side of that rule is that an absent `firstSeenAfter` adds no predicate — the full population.
	it('no firstSeenAfter (an unknown or absent new, coerced to all) is the full guarded population', async () => {
		const { issueQueries } = await import('./issue-queries');

		const rows = listSeedOnly(await issueQueries.listIssues(await loaderFilters(openUser), 1, 100));
		const narrowed = listSeedOnly(await issueQueries.listIssues(await loaderFilters(narrowedUser), 1, 100));

		expect(ids(rows).sort()).toEqual(VISIBLE_TO_OPEN);
		expect(ids(narrowed).sort()).toEqual(VISIBLE_TO_NARROWED);
		expect(await issueQueries.countIssues(await loaderFilters(openUser))).toBe(
			(await issueQueries.listIssues(await loaderFilters(openUser), 1, 100)).length
		);
	});

	// ---------------------------------------------------------------------------------------------
	// Decision 22: the two existing columns ride on every listed row.
	// ---------------------------------------------------------------------------------------------

	it('every listIssues row carries regressionStatus and lastRegressedAt', async () => {
		const { issueQueries } = await import('./issue-queries');

		for (const sort of ['last_seen', 'affected_users', 'count', 'first_seen']) {
			const rows = listSeedOnly(await issueQueries.listIssues(await loaderFilters(openUser, { sort }), 1, 100));
			expect(rows).toHaveLength(7);
			for (const row of rows) {
				expect(row).toHaveProperty('regressionStatus');
				expect(row).toHaveProperty('lastRegressedAt');
				expect(['none', 'regressed']).toContain(row.regressionStatus);
			}
			const regressed = rows.find((r) => r.id === issueF);
			expect(regressed?.regressionStatus).toBe('regressed');
			expect(regressed?.lastRegressedAt?.getTime()).toBe(regressedAt.getTime());
			const plain = rows.find((r) => r.id === issueA);
			expect(plain?.regressionStatus).toBe('none');
			expect(plain?.lastRegressedAt).toBeNull();
		}
	});

	// ---------------------------------------------------------------------------------------------
	// §4.1's reader: `trendForIssues(ids, windowStart, windowEnd)` — dense, keyed, bounded.
	// ---------------------------------------------------------------------------------------------

	it('trendForIssues returns 24 dense entries per id: 24 zeros for a quiet id, 3 populated slots for busy', async () => {
		const { issueQueries } = await import('./issue-queries');

		const series = await issueQueries.trendForIssues([issueBusy, issueQuiet], list.windowStart, list.windowEnd);

		expect([...series.keys()].sort()).toEqual([issueBusy, issueQuiet].sort());
		const busy = series.get(issueBusy)!;
		const quiet = series.get(issueQuiet)!;
		expect(busy).toHaveLength(24);
		expect(quiet).toHaveLength(24);
		expect(quiet).toEqual(new Array(24).fill(0));
		expect(busy[23]).toBe(5);
		expect(busy[20]).toBe(2);
		expect(busy[0]).toBe(1);
		expect(busy.filter((n) => n !== 0)).toHaveLength(3);
		expect(busy.reduce((a, b) => a + b, 0)).toBe(BUSY_24H_TOTAL);
		expect(busy.every((n) => Number.isInteger(n))).toBe(true);
	});

	it('trendForIssues returns nothing for an id outside ids, even though it has rows in the window', async () => {
		const { issueQueries } = await import('./issue-queries');

		const series = await issueQueries.trendForIssues([issueBusy, issueQuiet], list.windowStart, list.windowEnd);

		expect(series.has(issueOutsider)).toBe(false);
		expect(series.size).toBe(2);
		// And the outsider's 77 did not leak into anyone else's current hour.
		expect(series.get(issueBusy)![23]).toBe(5);
		expect(series.get(issueQuiet)![23]).toBe(0);
	});

	it('trendForIssues([]) returns an empty Map and issues NO query', async () => {
		const { issueQueries } = await import('./issue-queries');
		recordedQueries = [];

		const series = await issueQueries.trendForIssues([], list.windowStart, list.windowEnd);

		expect(series).toBeInstanceOf(Map);
		expect(series.size).toBe(0);
		expect(recordedQueries).toEqual([]);
	});

	it('trendForIssues issues ONE query for a page of ids, and the recorder sees it', async () => {
		const { issueQueries } = await import('./issue-queries');
		recordedQueries = [];

		await issueQueries.trendForIssues([issueBusy, issueQuiet], list.windowStart, list.windowEnd);

		// The seam is live — so the `[]` case's empty record above means "no query", not "no logger".
		expect(recordedQueries).toHaveLength(1);
		expect(recordedQueries[0]).toMatch(/issue_occurrence_buckets/);
	});

	it('trendForIssues is bounded at both ends: the row at trunc(now) + 1h and the row at trunc(now) − 24h are in no series and no sum', async () => {
		const { issueQueries } = await import('./issue-queries');

		const series = await issueQueries.trendForIssues([issueBusy], list.windowStart, list.windowEnd);
		const busy = series.get(issueBusy)!;

		expect(busy).toHaveLength(24);
		expect(busy.reduce((a, b) => a + b, 0)).toBe(BUSY_24H_TOTAL);
		expect(busy).not.toContain(1000);
		expect(busy).not.toContain(9);
	});

	// ---------------------------------------------------------------------------------------------
	// §4.2's reader: `trendForIssue(id, windowStart, windowEnd)` — hourly (24) and daily (30, UTC
	// calendar days, decision 11) from one query over the 30-day window.
	// ---------------------------------------------------------------------------------------------

	it('trendForIssue returns hourly of length 24 with [23].start = trunc(now) and [0].start = trunc(now) − 23h', async () => {
		const { issueQueries } = await import('./issue-queries');

		const trend = await issueQueries.trendForIssue(issueBusy, detail.windowStart, detail.windowEnd);

		expect(trend.hourly).toHaveLength(24);
		expect(trend.hourly[23].start).toBe(iso(truncNow));
		expect(trend.hourly[0].start).toBe(iso(at(truncNow.getTime() - 23 * HOUR)));
		for (let i = 0; i < 24; i++) {
			expect(trend.hourly[i].start).toBe(iso(at(truncNow.getTime() - (23 - i) * HOUR)));
		}
		expect(trend.hourly[23].count).toBe(5);
		expect(trend.hourly[20].count).toBe(2);
		expect(trend.hourly[0].count).toBe(1);
		expect(trend.hourly.reduce((a, p) => a + p.count, 0)).toBe(BUSY_24H_TOTAL);
	});

	it('trendForIssue returns daily of length 30 with daily[i].start = windowStart + i days, ending today (UTC)', async () => {
		const { issueQueries } = await import('./issue-queries');

		const trend = await issueQueries.trendForIssue(issueBusy, detail.windowStart, detail.windowEnd);

		expect(trend.daily).toHaveLength(30);
		for (let i = 0; i < 30; i++) {
			expect(trend.daily[i].start).toBe(iso(at(detail.windowStart.getTime() + i * DAY)));
		}
		// Today's UTC midnight is the last entry; every start is a UTC midnight.
		expect(trend.daily[29].start).toBe(iso(at(Math.floor(truncNow.getTime() / DAY) * DAY)));
		expect(trend.daily.every((p) => p.start.endsWith('T00:00:00Z'))).toBe(true);
		expect(trend.daily.every((p) => Number.isInteger(p.count) && p.count >= 0)).toBe(true);
	});

	it('trendForIssue sums across a UTC day boundary into the right day: 00:00 + 23:00 in day 0, the next 00:00 in day 1', async () => {
		const { issueQueries } = await import('./issue-queries');

		const trend = await issueQueries.trendForIssue(issueBusy, detail.windowStart, detail.windowEnd);

		expect(trend.daily[0].count).toBe(7 + 3);
		expect(trend.daily[1].count).toBe(4);
		expect(trend.daily[2].count).toBe(0);
		// Today's entry holds the current hour's 5 — whatever else of the last 24 hours falls on this
		// side of midnight — and never the +1h row.
		expect(trend.daily[29].count).toBeGreaterThanOrEqual(5);
		expect(trend.daily[29].count).toBeLessThan(1000);
	});

	it('trendForIssue is bounded at both ends: the row before windowStart and the row at trunc(now) + 1h are in no series and no sum', async () => {
		const { issueQueries } = await import('./issue-queries');

		const trend = await issueQueries.trendForIssue(issueBusy, detail.windowStart, detail.windowEnd);

		expect(trend.daily.reduce((a, p) => a + p.count, 0)).toBe(BUSY_30D_TOTAL);
		expect(trend.daily.map((p) => p.count)).not.toContain(100);
		expect(trend.daily.map((p) => p.count)).not.toContain(1000);
		expect(trend.hourly.map((p) => p.count)).not.toContain(1000);
		// The −24h row (9) is inside the 30-day window but outside the 24-hour one: in daily's sum,
		// in none of hourly's slots.
		expect(trend.hourly.map((p) => p.count)).not.toContain(9);
	});

	it('trendForIssue for a quiet issue is 24 + 30 zeros with every start still filled in', async () => {
		const { issueQueries } = await import('./issue-queries');

		const trend = await issueQueries.trendForIssue(issueQuiet, detail.windowStart, detail.windowEnd);

		expect(trend.hourly).toHaveLength(24);
		expect(trend.daily).toHaveLength(30);
		expect(trend.hourly.every((p) => p.count === 0)).toBe(true);
		expect(trend.daily.every((p) => p.count === 0)).toBe(true);
		expect(trend.hourly[23].start).toBe(iso(truncNow));
		expect(trend.daily[0].start).toBe(iso(detail.windowStart));
	});

	it('trendForIssue reads only the named issue — the outsider’s current-hour row is not in busy’s series', async () => {
		const { issueQueries } = await import('./issue-queries');
		recordedQueries = [];

		const trend = await issueQueries.trendForIssue(issueBusy, detail.windowStart, detail.windowEnd);

		expect(trend.hourly[23].count).toBe(5);
		expect(recordedQueries).toHaveLength(1);
	});
});
