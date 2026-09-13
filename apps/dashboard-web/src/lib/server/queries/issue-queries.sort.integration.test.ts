import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

/**
 * sentinel-affected-users §4.1 — contract §11's `listIssues` / `countIssues` boundary, against a
 * REAL migrated Postgres because every claim below is a claim about a query's SQL: the `ORDER BY`
 * `sort=affected_users` produces (`affected_user_count DESC, last_seen DESC, id DESC`, decisions 10
 * and 22 — with a seed where two issues tie on BOTH leading keys, so the third key is exercised), that
 * the default and an unknown `sort` produce today's `last_seen DESC` order, that `countIssues` ignores
 * `sort`, and that the project and environment guards are still in the `WHERE` under either order —
 * a sort is not a read path around a guard (§2). A mocked query layer cannot tell a composed
 * `orderBy` from a constructed-and-dropped one; only the rows can say.
 *
 * `src/lib/db/queries/issues.integration.test.ts`'s style: seeds its OWN organizations, projects,
 * users, grants and issues under a per-run suffix and deletes them again, so nothing here depends on
 * ambient data and nothing it asserts can be satisfied or masked by another suite's rows.
 *
 * Point DATABASE_URL at a DISPOSABLE database — never the shared dev one (CLAUDE.md):
 *
 *   docker run -d --name sentinel-sort-pg -p 15604:5432 \
 *     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && GOWORK=off \
 *     DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15604/sentinel?sslmode=disable \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15604/sentinel \
 *     pnpm vitest run src/lib/server/queries/issue-queries.sort.integration.test.ts
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
			'This test is the only coverage that runs listIssues({ sort }) against real Postgres; ' +
			'skipping it silently would leave the §4.1 ORDER BY unguarded.'
	);
}

const suffix = randomUUID().slice(0, 8);

const orgOwn = randomUUID();
const orgForeign = randomUUID();
const projectOwn = randomUUID();
// A project in a different organization: its issue carries the LARGEST count in the seed, so if the
// tenancy predicate ever fell out of the sorted query it would land at the TOP of the result.
const projectForeign = randomUUID();

// The unrestricted member: an org `engineer` with no grant row — sees every environment of
// `projectOwn`. The narrowed member is granted `{production}` only, so the `staging` issue — again
// the largest count they could reach — must never appear for them.
const openUser = randomUUID();
const narrowedUser = randomUUID();

// One `affected_user_count` / `last_seen` pair per issue, relative to now so the retention suite
// (which deletes by age across the whole database) cannot delete the fixture out from under this
// file when the two run together. Ordered here by the `affected_users` order the OPEN user sees:
//
//   staging(100, -2h) > D(9, -10h) > C(5, -1h) > {A, B}(5, -3h — tie on BOTH leading keys, id DESC
//   decides) > E(0, -30m)
//
// and by today's `last_seen DESC` order: E(-30m) > C(-1h) > staging(-2h) > {A, B}(-3h) > D(-10h).
// D is the highest count and the OLDEST; E is the newest and has NO users — the two orders put them
// at opposite ends, which is what tells the orders apart.
const HOUR = 60 * 60 * 1000;
const fixtureNow = Date.now();
const issueStagingBig = randomUUID();
const issueD = randomUUID();
const issueC = randomUUID();
const issueA = randomUUID();
const issueB = randomUUID();
const issueE = randomUUID();
const issueForeignHuge = randomUUID();

const SEED: Array<[id: string, project: string, environment: string, count: number, lastSeenMs: number]> = [
	[issueStagingBig, projectOwn, 'staging', 100, fixtureNow - 2 * HOUR],
	[issueD, projectOwn, 'production', 9, fixtureNow - 10 * HOUR],
	[issueC, projectOwn, 'production', 5, fixtureNow - 1 * HOUR],
	[issueA, projectOwn, 'production', 5, fixtureNow - 3 * HOUR],
	[issueB, projectOwn, 'production', 5, fixtureNow - 3 * HOUR],
	[issueE, projectOwn, 'production', 0, fixtureNow - 0.5 * HOUR],
	[issueForeignHuge, projectForeign, 'production', 100_000, fixtureNow - 1 * HOUR],
];

// The tie-broken pair, in `id DESC` order: uuids compare as their 128-bit value, which for the
// `postgres` driver's text form is the same as a byte-wise comparison of the canonical lowercase
// hex string.
const [tieFirst, tieSecond] = issueA > issueB ? [issueA, issueB] : [issueB, issueA];

const AFFECTED_ORDER_OPEN = [issueStagingBig, issueD, issueC, tieFirst, tieSecond, issueE];
const AFFECTED_ORDER_NARROWED = [issueD, issueC, tieFirst, tieSecond, issueE];
const VISIBLE_TO_OPEN = [...AFFECTED_ORDER_OPEN].sort();
const VISIBLE_TO_NARROWED = [...AFFECTED_ORDER_NARROWED].sort();

const ids = (rows: { id: string }[]) => rows.map((r) => r.id);
// `issues.last_seen` is nullable in the schema; every seeded row here sets it, so a null is a fixture
// bug, not a case to tolerate.
const lastSeenMs = (row: { lastSeen: Date | null }) => {
	if (!row.lastSeen) throw new Error('seeded issue without last_seen');
	return row.lastSeen.getTime();
};

describe.skipIf(!dbReachable)('listIssues / countIssues sort (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });

		for (const [id, label] of [
			[orgOwn, 'own'],
			[orgForeign, 'foreign'],
		] as const) {
			await sql`
				insert into organizations (id, name, slug)
				values (${id}, ${`sort-${label}-${suffix}`}, ${`sort-${label}-${suffix}`})
			`;
		}
		for (const [id, org, label] of [
			[projectOwn, orgOwn, 'own'],
			[projectForeign, orgForeign, 'foreign'],
		] as const) {
			await sql`
				insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${id}, ${`sort-${label}-${suffix}`}, ${org},
				        ${`k_${label}_${suffix}`}, ${`h_${label}_${suffix}`})
			`;
		}
		for (const [id, label] of [
			[openUser, 'open'],
			[narrowedUser, 'narrowed'],
		] as const) {
			await sql`
				insert into "user" (id, name, email)
				values (${id}, ${label}, ${`sort-${label}-${suffix}@example.test`})
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

		// `affected_user_count` is written directly: this file asserts the ORDER the column produces,
		// not the invariant behind it (that is `tests/integration`'s and the sweep suite's).
		for (const [id, projectId, environment, count, lastSeenMs] of SEED) {
			await sql`
				insert into issues (id, project_id, environment, fingerprint, message, error_class,
				                    affected_user_count, last_seen)
				values (${id}, ${projectId}, ${environment}, ${`fp-${id}`},
				        ${`sort seed ${suffix}`}, ${'SortSeedError'},
				        ${count}, ${new Date(lastSeenMs).toISOString()})
			`;
		}
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issues where project_id in (${projectOwn}, ${projectForeign})`;
			await sql`delete from project_environment_grants where user_id = ${narrowedUser}`;
			await sql`delete from organization_members where organization_id = ${orgOwn}`;
			await sql`delete from projects where organization_id in (${orgOwn}, ${orgForeign})`;
			await sql`delete from "user" where id in (${openUser}, ${narrowedUser})`;
			await sql`delete from organizations where id in (${orgOwn}, ${orgForeign})`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
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

	// ---------------------------------------------------------------------------------------------
	// The population first: with no `sort`, the open user sees exactly the six own-project issues.
	// Asserted before any order claim, because an order over an empty result is true of anything.
	// ---------------------------------------------------------------------------------------------

	it('the open member sees exactly the six own-project issues, and the foreign one never', async () => {
		const { issueQueries } = await import('./issue-queries');

		const rows = await issueQueries.listIssues(await loaderFilters(openUser));

		expect(ids(rows).sort()).toEqual(VISIBLE_TO_OPEN);
		expect(ids(rows)).not.toContain(issueForeignHuge);
	});

	// ---------------------------------------------------------------------------------------------
	// §4.1's `affected_users` order — all three keys.
	// ---------------------------------------------------------------------------------------------

	it('sort=affected_users orders affected_user_count DESC, last_seen DESC, id DESC', async () => {
		const { issueQueries } = await import('./issue-queries');

		const rows = await issueQueries.listIssues(
			await loaderFilters(openUser, { sort: 'affected_users' })
		);

		expect(ids(rows)).toEqual(AFFECTED_ORDER_OPEN);
		// Said in the column's terms too, so a reader does not have to trust the id list: counts never
		// go up down the page, and the two 5s that tie on last_seen are broken by id, descending.
		expect(isNonIncreasing(rows.map((r) => r.affectedUserCount))).toBe(true);
		const tied = rows.filter((r) => r.id === issueA || r.id === issueB);
		expect(lastSeenMs(tied[0])).toBe(lastSeenMs(tied[1]));
		expect(tied[0].id > tied[1].id).toBe(true);
	});

	it('sort=affected_users puts the highest count first even when it is the oldest, and 0 last even when newest', async () => {
		const { issueQueries } = await import('./issue-queries');

		const rows = await issueQueries.listIssues(
			await loaderFilters(narrowedUser, { sort: 'affected_users' })
		);

		expect(ids(rows)[0]).toBe(issueD);
		expect(ids(rows).at(-1)).toBe(issueE);
	});

	it('the two counts ride on every listed row as integers, 0 included', async () => {
		const { issueQueries } = await import('./issue-queries');

		const rows = await issueQueries.listIssues(
			await loaderFilters(openUser, { sort: 'affected_users' })
		);

		for (const row of rows) {
			expect(Number.isInteger(row.affectedUserCount)).toBe(true);
			expect(Number.isInteger(row.affectedTenantCount)).toBe(true);
		}
		expect(rows.find((r) => r.id === issueE)?.affectedUserCount).toBe(0);
		expect(rows.find((r) => r.id === issueStagingBig)?.affectedUserCount).toBe(100);
	});

	// ---------------------------------------------------------------------------------------------
	// The default and an unknown value: today's `last_seen DESC` order, and nothing else. A and B tie
	// on `last_seen` and today's order has no tiebreak, so the assertion is "a valid last_seen order"
	// — non-increasing timestamps — plus the two ends, which the affected_users order reverses.
	// ---------------------------------------------------------------------------------------------

	for (const [label, sort] of [
		['omitted', undefined],
		["'last_seen'", 'last_seen'],
		["'banana' (unknown, coerced)", 'banana'],
	] as const) {
		it(`sort ${label} produces today's last_seen DESC order`, async () => {
			const { issueQueries } = await import('./issue-queries');

			const rows = await issueQueries.listIssues(
				await loaderFilters(openUser, sort === undefined ? {} : { sort })
			);

			expect(ids(rows).sort()).toEqual(VISIBLE_TO_OPEN);
			expect(isNonIncreasing(rows.map(lastSeenMs))).toBe(true);
			// E (0 users, newest) first and D (9 users, oldest) last: the exact opposite of the
			// affected_users order's ends, so a coercion to the WRONG default cannot pass here.
			expect(ids(rows)[0]).toBe(issueE);
			expect(ids(rows).at(-1)).toBe(issueD);
			expect(ids(rows).slice(1, 3)).toEqual([issueC, issueStagingBig]);
		});
	}

	// ---------------------------------------------------------------------------------------------
	// `countIssues` ignores `sort`: the total does not change with the order.
	// ---------------------------------------------------------------------------------------------

	it('countIssues is identical for every sort, and equals the listed population', async () => {
		const { issueQueries } = await import('./issue-queries');

		const totals = await Promise.all(
			[undefined, 'last_seen', 'affected_users', 'banana'].map(async (sort) =>
				issueQueries.countIssues(
					await loaderFilters(openUser, sort === undefined ? {} : { sort })
				)
			)
		);

		expect(totals).toEqual([6, 6, 6, 6]);
		expect(await issueQueries.countIssues(await loaderFilters(narrowedUser, { sort: 'affected_users' }))).toBe(5);
	});

	// ---------------------------------------------------------------------------------------------
	// §2: the guards are still in the WHERE under the new order. The foreign issue and the staging
	// issue carry the largest counts in the seed, so a dropped predicate would put them FIRST.
	// ---------------------------------------------------------------------------------------------

	it('sort=affected_users never returns a foreign-project issue — the tenancy predicate is in the WHERE', async () => {
		const { issueQueries } = await import('./issue-queries');

		for (const user of [openUser, narrowedUser]) {
			const rows = await issueQueries.listIssues(
				await loaderFilters(user, { sort: 'affected_users' })
			);
			expect(ids(rows)).not.toContain(issueForeignHuge);
		}
	});

	it('sort=affected_users never returns an out-of-scope environment — the guard is in the WHERE', async () => {
		const { issueQueries } = await import('./issue-queries');

		const rows = await issueQueries.listIssues(
			await loaderFilters(narrowedUser, { sort: 'affected_users' })
		);

		expect(ids(rows)).not.toContain(issueStagingBig);
		expect(ids(rows)).toEqual(AFFECTED_ORDER_NARROWED);
		expect(ids(rows).sort()).toEqual(VISIBLE_TO_NARROWED);
	});

	it('the environment FILTER composes with the sort the same way it composes with the default', async () => {
		const { issueQueries } = await import('./issue-queries');

		const rows = await issueQueries.listIssues(
			await loaderFilters(openUser, {
				projectId: projectOwn,
				environment: 'staging',
				sort: 'affected_users',
			})
		);

		expect(ids(rows)).toEqual([issueStagingBig]);
	});
});
