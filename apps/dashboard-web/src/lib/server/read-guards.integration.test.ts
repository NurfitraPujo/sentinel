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
 *   - `lib/server/issue-access-dispatch.ts: requireIssueAccessAnyType` (§2.2, decision 25) — the
 *     guard point for reports, comments and attachments, asserted HERE and not at
 *     `report-access.ts: requireReportAccessForIssue`, because this is the function that dispatches
 *     on `issue_type` and a test at the report-specific helper proves nothing about the comment and
 *     attachment branches. **This boundary had no test file of its own at all**, which is why the
 *     predicate could be added to `requireIssueAccess` — reached on the `system_error` branch ONLY —
 *     and leave every `user_report` unguarded with nothing red.
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

// The `user_report` half of the dispatch. Everything above is a `system_error` (the column's
// default), and that asymmetry is the finding: §4.8a writes the literal `production` on every
// report with no client assertion, so a report is exactly the row a `{staging}` grant must hide.
const reportNarrowedProd = randomUUID();
const reportNarrowedStaging = randomUUID();

const SEEDED_ISSUES = [
	[issueNarrowedProd, projectNarrowed, 'production', 'system_error'],
	[issueNarrowedStaging, projectNarrowed, 'staging', 'system_error'],
	[issueOpenProd, projectOpen, 'production', 'system_error'],
	[issueOpenStaging, projectOpen, 'staging', 'system_error'],
	[issueForeignProd, projectForeign, 'production', 'system_error'],
	[reportNarrowedProd, projectNarrowed, 'production', 'user_report'],
	[reportNarrowedStaging, projectNarrowed, 'staging', 'user_report'],
] as const;

/**
 * What the narrowed member may see: `projectNarrowed` staging only, `projectOpen` entirely.
 *
 * The two `user_report` rows belong here beside the `system_error` ones because neither
 * `listIssues` nor `searchErrors` filters on `issue_type` — a report is an `issues` row like any
 * other to them, so the narrowed member sees the staging report and not the production one on
 * exactly the same predicate.
 */
const VISIBLE_TO_NARROWED: string[] = [
	issueNarrowedStaging,
	issueOpenProd,
	issueOpenStaging,
	reportNarrowedStaging,
];
/** What the unrestricted member may see: every issue of both reachable projects. */
const VISIBLE_TO_OPEN: string[] = [
	issueNarrowedProd,
	issueNarrowedStaging,
	issueOpenProd,
	issueOpenStaging,
	reportNarrowedProd,
	reportNarrowedStaging,
];

// sentinel-project-environments/BUG-14 — the FAN-OUT fixture, deliberately in an organization of
// its own so the exact-set assertions above keep asserting exactly what they asserted before.
// `searchErrors` joins `issues → error_occurrences → error_search_index`, which is one-to-many: an
// issue with several indexed occurrences produced one ROW PER OCCURRENCE while `total` counted
// `DISTINCT issues.id`. Two issues in one project, one of them fanned out, is the smallest fixture
// that can tell "one row per issue" from "one row per occurrence" AND pin the paging order.
const orgFanout = randomUUID();
const projectFanout = randomUUID();
const fanoutUser = randomUUID();
const issueFanout = randomUUID();
const issueSingle = randomUUID();
const FANOUT_OCCURRENCE_COUNT = 5;
// The occurrence DISTINCT ON must keep: the newest of `issueFanout`'s five.
let newestFanoutOccurrence = '';

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

		for (const [id, projectId, environment, issueType] of SEEDED_ISSUES) {
			await sql`
				insert into issues (id, project_id, environment, fingerprint, message, error_class, issue_type)
				values (${id}, ${projectId}, ${environment}, ${`fp-${id}`},
				        ${`rg seed ${suffix}`}, ${'ReadGuardSeedError'}, ${issueType})
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

		// --- BUG-14's fan-out fixture -------------------------------------------------------------
		await sql`
			insert into organizations (id, name, slug)
			values (${orgFanout}, ${`rg-fanout-${suffix}`}, ${`rg-fanout-${suffix}`})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectFanout}, ${`rg-fanout-${suffix}`}, ${orgFanout},
			        ${`k_fanout_${suffix}`}, ${`h_fanout_${suffix}`})
		`;
		await sql`
			insert into "user" (id, name, email)
			values (${fanoutUser}, ${'fanout'}, ${`rg-fanout-${suffix}@example.test`})
		`;
		await sql`
			insert into organization_members (organization_id, user_id, role)
			values (${orgFanout}, ${fanoutUser}, ${'engineer'})
		`;

		// Every timestamp below is RELATIVE to now, deliberately: `retention.ts` deletes occurrences
		// older than `retentionDays` (30 by default) across the whole database, and the retention
		// suite runs against this same database, so a fixture dated in absolute terms is deleted out
		// from under this file whenever the two happen to run together.
		//
		// `issueSingle` is the NEWER of the two by `last_seen`, so the outer ordering the fix applies
		// puts it first — an assertion that fails if the ordering is dropped again and the plan's
		// emission order happens to differ.
		const fixtureNow = Date.now();
		for (const [id, lastSeen] of [
			[issueFanout, new Date(fixtureNow - 2 * 60 * 60 * 1000).toISOString()],
			[issueSingle, new Date(fixtureNow - 1 * 60 * 60 * 1000).toISOString()],
		] as const) {
			await sql`
				insert into issues (id, project_id, environment, fingerprint, message, error_class, last_seen)
				values (${id}, ${projectFanout}, ${'production'}, ${`fp-${id}`},
				        ${`rg fanout seed ${suffix}`}, ${'ReadGuardFanoutError'}, ${lastSeen})
			`;
		}

		for (let i = 0; i < FANOUT_OCCURRENCE_COUNT; i++) {
			const occurrenceId = randomUUID();
			// Ascending, so the LAST one written is the newest — the one DISTINCT ON must keep.
			const createdAt = new Date(
				fixtureNow - (FANOUT_OCCURRENCE_COUNT - i) * 60 * 1000
			).toISOString();
			await sql`
				insert into error_occurrences (id, issue_id, environment, platform, created_at)
				values (${occurrenceId}, ${issueFanout}, ${'production'}, ${'node'}, ${createdAt})
			`;
			await sql`insert into error_search_index (occurrence_id) values (${occurrenceId})`;
			newestFanoutOccurrence = occurrenceId;
		}

		const singleOccurrence = randomUUID();
		await sql`
			insert into error_occurrences (id, issue_id, environment, platform)
			values (${singleOccurrence}, ${issueSingle}, ${'production'}, ${'node'})
		`;
		await sql`insert into error_search_index (occurrence_id) values (${singleOccurrence})`;
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issues where project_id in (${projectNarrowed}, ${projectOpen}, ${projectForeign}, ${projectFanout})`;
			await sql`delete from project_environment_grants where user_id in (${narrowedUser}, ${openUser})`;
			await sql`delete from organization_members where organization_id in (${orgOwn}, ${orgForeign}, ${orgFanout})`;
			await sql`delete from projects where organization_id in (${orgOwn}, ${orgForeign}, ${orgFanout})`;
			await sql`delete from "user" where id in (${narrowedUser}, ${openUser}, ${outsiderUser}, ${fanoutUser})`;
			await sql`delete from organizations where id in (${orgOwn}, ${orgForeign}, ${orgFanout})`;
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
		expect(ids(staging)).toEqual(
			[issueNarrowedStaging, issueOpenStaging, reportNarrowedStaging].sort()
		);

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
		expect(narrowed.total).toBe(2);
		expect(issueIds(narrowed.results as { issueId: string }[])).toEqual(
			[issueNarrowedStaging, reportNarrowedStaging].sort()
		);

		const open = await searchErrors(
			{ id: openUser } as any,
			{ projectId: projectNarrowed },
			{ limit: 100 }
		);
		expect(open.total).toBe(4);
	});

	// ---------------------------------------------------------------------------------------------
	// BUG-14 — one row per ISSUE. The result query fanned out over `error_occurrences` while the
	// count query counted `DISTINCT issues.id`, so `total` described a set the page could not
	// produce and one busy issue could fill a whole page by itself. Nothing else in this file could
	// see it: every other fixture issue carries exactly ONE occurrence, at which cardinality the
	// broken query and the fixed one return the same rows.
	// ---------------------------------------------------------------------------------------------

	it('an issue with several indexed occurrences is ONE result row, and total agrees with it', async () => {
		const { searchErrors } = await import('./search');

		const { results, total } = await searchErrors(
			{ id: fanoutUser } as any,
			{ projectId: projectFanout },
			{ limit: 100 }
		);

		// Pre-fix this was FANOUT_OCCURRENCE_COUNT + 1 rows against a total of 2.
		expect(results.filter((r) => r.issueId === issueFanout)).toHaveLength(1);
		expect(issueIds(results as { issueId: string }[])).toEqual(
			[issueFanout, issueSingle].sort()
		);
		expect(total).toBe(2);
		expect(results).toHaveLength(total);
	});

	it('a single-issue search is one row and total === 1, not one row per occurrence', async () => {
		const { searchErrors } = await import('./search');

		// The finding's own example, isolated: ONE issue carrying several indexed occurrences.
		const { results, total } = await searchErrors(
			{ id: fanoutUser } as any,
			{ projectId: projectFanout, platform: 'node', search: 'fanout' },
			{ limit: 100 }
		);
		const fanout = results.filter((r) => r.issueId === issueFanout);

		expect(fanout).toHaveLength(1);
		expect(total).toBe(results.length);
	});

	it('the surviving row carries the NEWEST matching occurrence, deterministically', async () => {
		const { searchErrors } = await import('./search');

		const first = await searchErrors(
			{ id: fanoutUser } as any,
			{ projectId: projectFanout },
			{ limit: 100 }
		);
		const second = await searchErrors(
			{ id: fanoutUser } as any,
			{ projectId: projectFanout },
			{ limit: 100 }
		);

		const pick = (r: typeof first) =>
			r.results.find((x) => x.issueId === issueFanout)?.matchingOccurrence.id;

		// WHICH of the five survives is a choice, and it has to be the same choice every time or the
		// row's `matchingOccurrence` flickers between requests.
		expect(pick(first)).toBe(newestFanoutOccurrence);
		expect(pick(second)).toBe(newestFanoutOccurrence);
	});

	it('LIMIT/OFFSET pages deterministically: every issue once, in a stable order', async () => {
		const { searchErrors } = await import('./search');

		const page = (offset: number) =>
			searchErrors({ id: fanoutUser } as any, { projectId: projectFanout }, { limit: 1, offset });

		const [first, second, firstAgain] = await Promise.all([page(0), page(1), page(0)]);

		// Without an ORDER BY, LIMIT/OFFSET may repeat a row on one page and skip another entirely.
		expect(issueIds(first.results as { issueId: string }[])).toEqual([issueSingle]);
		expect(issueIds(second.results as { issueId: string }[])).toEqual([issueFanout]);
		// `issueSingle` has the newer `last_seen`, so it leads — and it leads on every request.
		expect(issueIds(firstAgain.results as { issueId: string }[])).toEqual([issueSingle]);
		expect(first.total).toBe(2);
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
	// ---------------------------------------------------------------------------------------------
	// §2.2, decision 25 — `requireIssueAccessAnyType`, the guard point the table names for reports,
	// comments and attachments.
	//
	// This is the boundary §11 lists as "existing" and which had NO test file at all; that absence
	// is part of the finding these tests close. The assertions come in pairs on purpose: the
	// `system_error` branch alone was already guarded (it dispatches to `requireIssueAccess`, which
	// carries its own §2.2 predicate), so a test that only exercised it would have passed against
	// the broken code. The `user_report` branch is the one that was open, and it is the majority of
	// report rows because §4.8a writes the literal `production` with no client assertion.
	// ---------------------------------------------------------------------------------------------

	it('requireIssueAccessAnyType 404s on an out-of-scope user_report — the branch requireIssueAccess never reaches', async () => {
		const { requireIssueAccessAnyType } = await import('./issue-access-dispatch');

		let caught: any;
		try {
			await requireIssueAccessAnyType(narrowedUser, reportNarrowedProd, 'read');
		} catch (e) {
			caught = e;
		}

		expect(caught).toBeDefined();
		// Decision 20: `404`, never `403`. `requireReportAccessForIssue` — the helper this branch
		// dispatches to, and the placement §2.2 rejects by name — resolves the issue row and then
		// applies an ORG-level check with no environment predicate at all, so before this guard the
		// call below RESOLVED.
		expect(caught.status).toBe(404);
		expect(caught.body?.message ?? caught.message).toContain('not found');
	});

	it('requireIssueAccessAnyType 404s on an out-of-scope system_error too — both branches, one predicate', async () => {
		const { requireIssueAccessAnyType } = await import('./issue-access-dispatch');

		let caught: any;
		try {
			await requireIssueAccessAnyType(narrowedUser, issueNarrowedProd, 'read');
		} catch (e) {
			caught = e;
		}

		expect(caught).toBeDefined();
		expect(caught.status).toBe(404);
	});

	it('the guard runs BEFORE the dispatch, so a write action is refused on the same 404', async () => {
		const { requireIssueAccessAnyType } = await import('./issue-access-dispatch');

		// `narrowedUser` is an org `engineer`, i.e. on ISSUE_WRITE_ROLES — the project dimension
		// grants this write. Only the environment dimension refuses it, and it must refuse it as a
		// 404 rather than the 403 the project dimension would use.
		await expect(
			requireIssueAccessAnyType(narrowedUser, reportNarrowedProd, 'write')
		).rejects.toMatchObject({ status: 404 });
	});

	it('requireIssueAccessAnyType admits what the grant covers, on both branches', async () => {
		const { requireIssueAccessAnyType } = await import('./issue-access-dispatch');

		await expect(
			requireIssueAccessAnyType(narrowedUser, reportNarrowedStaging, 'read')
		).resolves.toMatchObject({ issueType: 'user_report', organizationId: orgOwn });

		await expect(
			requireIssueAccessAnyType(narrowedUser, issueNarrowedStaging, 'read')
		).resolves.toMatchObject({ issueType: 'system_error', organizationId: orgOwn });
	});

	it('an unrestricted member is admitted to the production report the narrowed one is refused', async () => {
		const { requireIssueAccessAnyType } = await import('./issue-access-dispatch');

		// The other half of "absence means all": a caller with no grant row sees every environment,
		// so the 404 above is the grant firing and not the report being unreachable for some other
		// reason. Without this the first assertion could pass vacuously.
		await expect(
			requireIssueAccessAnyType(openUser, reportNarrowedProd, 'read')
		).resolves.toMatchObject({ issueType: 'user_report', organizationId: orgOwn });
	});
});
