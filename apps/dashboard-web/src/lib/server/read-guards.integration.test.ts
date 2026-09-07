import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

/**
 * sentinel-project-environments — contract §11's three read-path boundaries, against a REAL
 * migrated Postgres because every claim below is a claim about a query's SQL:
 *
 *   - `issue-queries.ts: listIssues` / `countIssues` (§4.4) — three claims at once: the guard
 *     applies without being asked, the filter applies when asked, and decision 22's `inArray`
 *     returns EVERY authorised project rather than the first.
 *   - `lib/server/search.ts: searchErrors` (§4.5) — that BOTH the result query and the
 *     `count(DISTINCT issues.id)` query carry the per-project scope. A guard on one and not the
 *     other is invisible at every other boundary and is a live leak.
 *   - `lib/server/issue-access.ts: requireIssueAccess` (§2.2) — `404` on the environment dimension,
 *     never `403` (decision 20), over the `project_members` ∪ `organization_members` OR.
 *
 * **Why a real database and not a mocked query layer.** A chained Drizzle double cannot tell "the
 * predicate was composed into the WHERE clause" from "the predicate object was constructed and
 * dropped", which is exactly the failure §4.5 names — `searchErrors` built its count query from a
 * COPY of the conditions, and Drizzle's `.where()` ASSIGNS rather than ANDs, so a guard can be
 * present in the code and absent from the SQL. Only the rows can say.
 *
 * **The population is asserted BEFORE the guard over it, deliberately.** `searchErrors` returned
 * `{ results: [], total: 0 }` for everybody before decision 31 widened its derivation, so a guard
 * test written first would pass vacuously — "no production hits" is true when there are no hits at
 * all. `the widened population` tests below run first and assert non-empty results for a user with
 * no `project_members` row; the guard tests then narrow that same population.
 *
 * Point DATABASE_URL at a DISPOSABLE database — never the shared dev one (CLAUDE.md):
 *
 *   docker run -d --name sentinel-readguards-pg -p 15453:5432 \
 *     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15453/sentinel?sslmode=disable \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15453/sentinel \
 *     pnpm vitest run src/lib/server/read-guards.integration.test.ts
 *
 * Skipped when nothing answers, so a machine with no Postgres is not broken by it. Set
 * READ_GUARDS_INTEGRATION_REQUIRED=1 to make "no reachable database" a hard failure, matching
 * SEARCH_INTEGRATION_REQUIRED / SCHEMA_DRIFT_REQUIRED / ENVIRONMENT_ACCESS_INTEGRATION_REQUIRED.
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.READ_GUARDS_INTEGRATION_REQUIRED === '1';

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
		`READ_GUARDS_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}.`
	);
}

const suffix = randomUUID().slice(0, 8);

const orgOwn = randomUUID();
const orgForeign = randomUUID();

// Two projects the caller can reach — decision 22's fix is only observable across two — and one in
// a different organization, which no assertion below may ever return.
const projectNarrowed = randomUUID();
const projectOpen = randomUUID();
const projectForeign = randomUUID();

// The narrowed member: an org `engineer` with NO `project_members` row anywhere (the ordinary case,
// §2.2), narrowed to `{staging}` in `projectNarrowed` only.
const narrowedUser = randomUUID();
// The unrestricted one: the same shape with no grant row at all. Before decision 35 this user saw
// nothing on `/search` and everything on `/`; both were the same fact about `project_members`.
const openUser = randomUUID();
// Reaches no organization at all: the empty authorised set, which must DENY.
const outsiderUser = randomUUID();

const issueNarrowedProd = randomUUID();
const issueNarrowedStaging = randomUUID();
const issueOpenProd = randomUUID();
const issueOpenStaging = randomUUID();
const issueForeignProd = randomUUID();

const SEEDED_ISSUES = [
	[issueNarrowedProd, projectNarrowed, 'production'],
	[issueNarrowedStaging, projectNarrowed, 'staging'],
	[issueOpenProd, projectOpen, 'production'],
	[issueOpenStaging, projectOpen, 'staging'],
	[issueForeignProd, projectForeign, 'production'],
] as const;

/** What the narrowed member may see: `projectNarrowed` staging only, `projectOpen` entirely. */
const VISIBLE_TO_NARROWED: string[] = [issueNarrowedStaging, issueOpenProd, issueOpenStaging];
/** What the unrestricted member may see: every issue of both reachable projects. */
const VISIBLE_TO_OPEN: string[] = [
	issueNarrowedProd,
	issueNarrowedStaging,
	issueOpenProd,
	issueOpenStaging,
];

const ids = (rows: { id: string }[]) => rows.map((r) => r.id).sort();
const issueIds = (rows: { issueId: string }[]) => rows.map((r) => r.issueId).sort();

describe.skipIf(!dbReachable)('the §2.2 read guards (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });

		for (const [id, label] of [
			[orgOwn, 'own'],
			[orgForeign, 'foreign'],
		] as const) {
			await sql`
				insert into organizations (id, name, slug)
				values (${id}, ${`rg-${label}-${suffix}`}, ${`rg-${label}-${suffix}`})
			`;
		}

		for (const [id, org, label] of [
			[projectNarrowed, orgOwn, 'narrowed'],
			[projectOpen, orgOwn, 'open'],
			[projectForeign, orgForeign, 'foreign'],
		] as const) {
			await sql`
				insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${id}, ${`rg-${label}-${suffix}`}, ${org},
				        ${`k_${label}_${suffix}`}, ${`h_${label}_${suffix}`})
			`;
		}

		for (const [id, label, role] of [
			[narrowedUser, 'narrowed', 'engineer'],
			[openUser, 'open', 'engineer'],
		] as const) {
			await sql`
				insert into "user" (id, name, email)
				values (${id}, ${label}, ${`rg-${label}-${suffix}@example.test`})
			`;
			await sql`
				insert into organization_members (organization_id, user_id, role)
				values (${orgOwn}, ${id}, ${role})
			`;
		}
		await sql`
			insert into "user" (id, name, email)
			values (${outsiderUser}, ${'outsider'}, ${`rg-outsider-${suffix}@example.test`})
		`;

		await sql`
			insert into project_environment_grants (user_id, project_id, environments, created_by)
			values (${narrowedUser}, ${projectNarrowed}, ${sql.array(['staging'])}, ${openUser})
		`;

		for (const [id, projectId, environment] of SEEDED_ISSUES) {
			await sql`
				insert into issues (id, project_id, environment, fingerprint, message, error_class)
				values (${id}, ${projectId}, ${environment}, ${`fp-${id}`},
				        ${`rg seed ${suffix}`}, ${'ReadGuardSeedError'})
			`;
			// One occurrence + one search-index row per issue: `searchErrors` inner-joins both, so an
			// issue with neither is invisible to it for reasons that have nothing to do with the guard.
			const occurrenceId = randomUUID();
			await sql`
				insert into error_occurrences (id, issue_id, environment, platform)
				values (${occurrenceId}, ${id}, ${environment}, ${'node'})
			`;
			await sql`insert into error_search_index (occurrence_id) values (${occurrenceId})`;
		}
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issues where project_id in (${projectNarrowed}, ${projectOpen}, ${projectForeign})`;
			await sql`delete from project_environment_grants where user_id in (${narrowedUser}, ${openUser})`;
			await sql`delete from organization_members where organization_id in (${orgOwn}, ${orgForeign})`;
			await sql`delete from projects where organization_id in (${orgOwn}, ${orgForeign})`;
			await sql`delete from "user" where id in (${narrowedUser}, ${openUser}, ${outsiderUser})`;
			await sql`delete from organizations where id in (${orgOwn}, ${orgForeign})`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	/** The filters the `/` loader builds, resolved the way the loader resolves them. */
	async function loaderFilters(userId: string, overrides: Record<string, unknown> = {}) {
		const { getAccessibleProjects } = await import('./issue-access');
		const { resolveEnvironmentScopes } = await import('./environment-access');
		return {
			status: 'all',
			projectId: 'all',
			projectIds: (await getAccessibleProjects(userId)).map((p) => p.id),
			environment: 'all',
			environmentScopes: await resolveEnvironmentScopes(userId),
			...overrides,
		};
	}

	// ---------------------------------------------------------------------------------------------
	// The POPULATION first — §4.4/§4.5's widening. Asserted before any guard, because a guard test
	// over an empty population passes vacuously.
	// ---------------------------------------------------------------------------------------------

	it('an org-role member with NO project_members row reaches both of the organization’s projects', async () => {
		const { getAccessibleProjects } = await import('./issue-access');

		const [{ count }] = await sql<{ count: string }[]>`
			select count(*)::text as count from project_members where user_id = ${openUser}
		`;
		expect(count).toBe('0');

		const reachable = (await getAccessibleProjects(openUser)).map((p) => p.id).sort();
		expect(reachable).toEqual([projectNarrowed, projectOpen].sort());
	});

	it('listIssues returns issues from EVERY authorised project, not the first — decision 22', async () => {
		const { issueQueries } = await import('./queries/issue-queries');

		const rows = await issueQueries.listIssues(await loaderFilters(openUser), 1, 100);

		expect(ids(rows)).toEqual([...VISIBLE_TO_OPEN].sort());
		// The pre-fix predicate was `eq(issues.projectId, filters.projectIds[0])`, which would return
		// one project's rows only. Both projects are represented here.
		expect(new Set(rows.map((r) => r.projectId))).toEqual(
			new Set([projectNarrowed, projectOpen])
		);
	});

	it('searchErrors returns results to that same member — the derivation that returned nothing to everybody', async () => {
		const { searchErrors } = await import('./search');

		const { results, total } = await searchErrors({ id: openUser } as any, {}, { limit: 100 });

		const seeded = results.filter((r) => VISIBLE_TO_OPEN.includes(r.issueId));
		expect(seeded.length).toBe(VISIBLE_TO_OPEN.length);
		expect(total).toBeGreaterThanOrEqual(VISIBLE_TO_OPEN.length);
	});

	// ---------------------------------------------------------------------------------------------
	// The GUARD, over that population.
	// ---------------------------------------------------------------------------------------------

	it('listIssues applies the environment guard without being asked, per project', async () => {
		const { issueQueries } = await import('./queries/issue-queries');

		const rows = await issueQueries.listIssues(await loaderFilters(narrowedUser), 1, 100);

		expect(ids(rows)).toEqual([...VISIBLE_TO_NARROWED].sort());
		// The narrowed project's production issue is the one this guard exists for.
		expect(ids(rows)).not.toContain(issueNarrowedProd);
		// …and the guard is PER PROJECT: the unrestricted project's production issue still shows.
		expect(ids(rows)).toContain(issueOpenProd);
	});

	it('countIssues agrees with listIssues rather than counting rows the list will not show', async () => {
		const { issueQueries } = await import('./queries/issue-queries');

		const filters = await loaderFilters(narrowedUser);
		const rows = await issueQueries.listIssues(filters, 1, 100);
		const total = await issueQueries.countIssues(filters);

		expect(total).toBe(rows.length);
		expect(total).toBe(VISIBLE_TO_NARROWED.length);
	});

	it('the environment FILTER applies when asked, and composes with the guard', async () => {
		const { issueQueries } = await import('./queries/issue-queries');

		const staging = await issueQueries.listIssues(
			await loaderFilters(narrowedUser, { environment: 'staging' }),
			1,
			100
		);
		expect(ids(staging)).toEqual([issueNarrowedStaging, issueOpenStaging].sort());

		// A filter the caller controls cannot be a guard: asking for `production` does not widen the
		// narrowed project, it only narrows the unrestricted one.
		const production = await issueQueries.listIssues(
			await loaderFilters(narrowedUser, { environment: 'production' }),
			1,
			100
		);
		expect(ids(production)).toEqual([issueOpenProd]);
	});

	it('an unrecognised environment name is an empty list, not an error', async () => {
		const { issueQueries } = await import('./queries/issue-queries');

		const rows = await issueQueries.listIssues(
			await loaderFilters(narrowedUser, { environment: 'qa' }),
			1,
			100
		);
		expect(rows).toEqual([]);
	});

	// ---------------------------------------------------------------------------------------------
	// The TENANCY predicate — decision 35's unconditional `inArray`.
	// ---------------------------------------------------------------------------------------------

	it('an empty authorised set DENIES rather than admitting every issue in the database', async () => {
		const { issueQueries } = await import('./queries/issue-queries');

		const filters = await loaderFilters(outsiderUser);
		expect(filters.projectIds).toEqual([]);

		// The pre-fix code added NO project predicate on an empty array, so this returned every issue
		// in the database, across every organization, to any authenticated user.
		expect(await issueQueries.listIssues(filters, 1, 100)).toEqual([]);
		expect(await issueQueries.countIssues(filters)).toBe(0);
	});

	it('selecting a project the caller cannot reach returns nothing — the two predicates AND', async () => {
		const { issueQueries } = await import('./queries/issue-queries');

		const rows = await issueQueries.listIssues(
			await loaderFilters(openUser, { projectId: projectForeign }),
			1,
			100
		);
		expect(rows).toEqual([]);
	});

	it('listProjects is scoped to the same set as the issue list', async () => {
		const { issueQueries } = await import('./queries/issue-queries');
		const { getAccessibleProjects } = await import('./issue-access');

		const authorised = (await getAccessibleProjects(openUser)).map((p) => p.id);
		const picker = await issueQueries.listProjects(authorised);

		expect(ids(picker)).toEqual([projectNarrowed, projectOpen].sort());
		expect(ids(picker)).not.toContain(projectForeign);
		// And the empty set is empty here too, not "every project in the deployment".
		expect(await issueQueries.listProjects([])).toEqual([]);
	});

	// ---------------------------------------------------------------------------------------------
	// §4.5 — BOTH of `searchErrors`'s queries.
	// ---------------------------------------------------------------------------------------------

	it('searchErrors carries the per-project scope in the RESULT query', async () => {
		const { searchErrors } = await import('./search');

		const { results } = await searchErrors({ id: narrowedUser } as any, {}, { limit: 100 });
		const seeded = results.filter((r) =>
			SEEDED_ISSUES.some(([id]) => id === r.issueId)
		);

		expect(issueIds(seeded as { issueId: string }[])).toEqual([...VISIBLE_TO_NARROWED].sort());
	});

	it('searchErrors carries the SAME scope in the count(DISTINCT issues.id) query', async () => {
		const { searchErrors } = await import('./search');

		// Scoped to one project so the totals are exact rather than "at least": a search that reports
		// how many production hits exist and shows none of them is the leak §4.5 names.
		const narrowed = await searchErrors(
			{ id: narrowedUser } as any,
			{ projectId: projectNarrowed },
			{ limit: 100 }
		);
		expect(narrowed.total).toBe(1);
		expect(issueIds(narrowed.results as { issueId: string }[])).toEqual([issueNarrowedStaging]);

		const open = await searchErrors(
			{ id: openUser } as any,
			{ projectId: projectNarrowed },
			{ limit: 100 }
		);
		expect(open.total).toBe(2);
	});

	// ---------------------------------------------------------------------------------------------
	// §2.2 — `requireIssueAccess`, and decision 20's `404`.
	// ---------------------------------------------------------------------------------------------

	it('requireIssueAccess 404s — never 403s — on an issue outside the caller’s grant', async () => {
		const { requireIssueAccess } = await import('./issue-access');

		let caught: any;
		try {
			await requireIssueAccess(narrowedUser, issueNarrowedProd, 'read');
		} catch (e) {
			caught = e;
		}

		expect(caught).toBeDefined();
		// `403` would confirm that a production issue with this id exists, which is the fact the
		// boundary was drawn to withhold. The PROJECT dimension keeps its 403s; this one is 404.
		expect(caught.status).toBe(404);
		expect(caught.body?.message ?? caught.message).toContain('Issue not found');
	});

	it('requireIssueAccess admits the same caller to an issue the grant covers, and admits an unrestricted caller to both', async () => {
		const { requireIssueAccess } = await import('./issue-access');

		await expect(
			requireIssueAccess(narrowedUser, issueNarrowedStaging, 'read')
		).resolves.toMatchObject({ projectId: projectNarrowed, organizationId: orgOwn });

		await expect(
			requireIssueAccess(openUser, issueNarrowedProd, 'read')
		).resolves.toMatchObject({ projectId: projectNarrowed });
	});
});
