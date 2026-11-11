import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

/**
 * fix-sentinel-issues-dashboard §4.1 — the facet reader, the new filters and the correctness fixes,
 * against a REAL migrated Postgres, because every claim is about a query's SQL: that a count is over
 * ALL matching rows and not a page, that each group drops only its OWN filter, that the project and
 * environment GUARDS are in every group's WHERE (two projects, a foreign project, a scoped user),
 * that `%` in a search is literal, and that `last_seen` pages have a total order.
 *
 * Seeds its own organizations/projects/users/issues under a per-run suffix and deletes them again.
 *
 * Point DATABASE_URL at a DISPOSABLE database — never the shared dev one (CLAUDE.md):
 *
 *   docker run -d --name sentinel-facets-pg -p 15811:5432 \
 *     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && GOWORK=off \
 *     DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15811/sentinel?sslmode=disable \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15811/sentinel \
 *     pnpm vitest run src/lib/server/queries/issue-queries.facets.integration.test.ts
 *
 * Skipped when nothing answers. SCHEMA_DRIFT_REQUIRED=1 / READ_GUARDS_INTEGRATION_REQUIRED=1 make
 * "no reachable database" a hard failure.
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
			'This is the only coverage that runs getIssueFacets against real Postgres.'
	);
}

const suffix = randomUUID().slice(0, 8);
const orgOwn = randomUUID();
const orgForeign = randomUUID();
const projectA = randomUUID();
const projectB = randomUUID();
const projectForeign = randomUUID();
const openUser = randomUUID();
const scopedUser = randomUUID(); // granted {production} in projectA only
const HOUR = 60 * 60 * 1000;
const NOW = new Date();

type Seed = {
	project: string;
	environment: string;
	status: string;
	count?: number;
	lastSeenHoursAgo?: number;
	firstSeenHoursAgo?: number;
	errorClass?: string;
	assignedTo?: string | null;
	assigneeType?: string | null;
};

// projectA: 30 issues (more than any page below), in two environments.
//   production: 12 unresolved, 6 resolved, 2 ignored   staging: 6 unresolved, 4 resolved
// projectB: 3 unresolved production.   projectForeign: 5 unresolved production (never visible).
const seeds: Seed[] = [];
const add = (n: number, s: Seed) => {
	for (let i = 0; i < n; i++) seeds.push({ ...s });
};
add(12, { project: projectA, environment: 'production', status: 'unresolved' });
add(6, { project: projectA, environment: 'production', status: 'resolved' });
add(2, { project: projectA, environment: 'production', status: 'ignored' });
add(6, { project: projectA, environment: 'staging', status: 'unresolved' });
add(4, { project: projectA, environment: 'staging', status: 'resolved' });
add(3, { project: projectB, environment: 'production', status: 'unresolved' });
add(5, { project: projectForeign, environment: 'production', status: 'unresolved' });

describe.skipIf(!dbReachable)('getIssueFacets and the shared filters (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;
	const issueIds: string[] = [];

	async function insert(s: Seed, overrides: Partial<Seed> = {}) {
		const x = { ...s, ...overrides };
		const id = randomUUID();
		issueIds.push(id);
		await sql`
			insert into issues (id, project_id, environment, fingerprint, message, error_class, status,
			                    count, last_seen, first_seen, assigned_to, assignee_type)
			values (${id}, ${x.project}, ${x.environment}, ${`fp-${id}`}, ${`facet seed ${suffix}`},
			        ${x.errorClass ?? 'FacetSeedError'}, ${x.status}, ${x.count ?? 1},
			        ${new Date(NOW.getTime() - (x.lastSeenHoursAgo ?? 1) * HOUR).toISOString()},
			        ${new Date(NOW.getTime() - (x.firstSeenHoursAgo ?? 100) * HOUR).toISOString()},
			        ${x.assignedTo ?? null}, ${x.assigneeType ?? null})
		`;
		return id;
	}

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		// The retention.*.integration family sweeps the WHOLE database -- retention.claims with a ONE-day
		// cutoff -- and every issue below is occurrence-less with first_seen 100 h back, which is exactly
		// the orphan rule: under --sequence.shuffle (B13) a sibling's sweep deleted part of this fixture
		// mid-file. The family holds this key EXCLUSIVELY for each member's whole file; holding it SHARED
		// here waits out a running sweep and keeps the next one out until afterAll, without serialising
		// this file against anything but sweeps.
		await sql`select pg_advisory_lock_shared(hashtext('sentinel.retention.integration'))`;
		for (const [id, label] of [
			[orgOwn, 'own'],
			[orgForeign, 'foreign'],
		] as const) {
			await sql`insert into organizations (id, name, slug)
				values (${id}, ${`facet-${label}-${suffix}`}, ${`facet-${label}-${suffix}`})`;
		}
		for (const [id, org, label] of [
			[projectA, orgOwn, 'a'],
			[projectB, orgOwn, 'b'],
			[projectForeign, orgForeign, 'foreign'],
		] as const) {
			await sql`insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${id}, ${`facet-${label}-${suffix}`}, ${org}, ${`k_${label}_${suffix}`}, ${`h_${label}_${suffix}`})`;
		}
		for (const [id, label] of [
			[openUser, 'open'],
			[scopedUser, 'scoped'],
		] as const) {
			await sql`insert into "user" (id, name, email)
				values (${id}, ${label}, ${`facet-${label}-${suffix}@example.test`})`;
			await sql`insert into organization_members (organization_id, user_id, role)
				values (${orgOwn}, ${id}, ${'engineer'})`;
		}
		await sql`insert into project_environment_grants (user_id, project_id, environments, created_by)
			values (${scopedUser}, ${projectA}, ${sql.array(['production'])}, ${openUser})`;

		for (const s of seeds) await insert(s);
		// The family's own hook ceiling: the shared lock can wait out a sibling's whole file.
	}, 120_000);

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issues where project_id in (${projectA}, ${projectB}, ${projectForeign})`;
			await sql`delete from project_environment_grants where user_id = ${scopedUser}`;
			await sql`delete from organization_members where organization_id = ${orgOwn}`;
			await sql`delete from projects where organization_id in (${orgOwn}, ${orgForeign})`;
			await sql`delete from "user" where id in (${openUser}, ${scopedUser})`;
			await sql`delete from organizations where id in (${orgOwn}, ${orgForeign})`;
		} finally {
			await sql`select pg_advisory_unlock_all()`.catch(() => {});
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	/** The filters the `/` loader builds for `userId`, resolved the way the loader resolves them. */
	async function loaderFilters(userId: string, overrides: Record<string, unknown> = {}) {
		const { getAccessibleProjects } = await import('../issue-access');
		const { resolveEnvironmentScopes } = await import('../environment-access');
		return {
			status: 'all',
			projectId: 'all',
			projectIds: (await getAccessibleProjects(userId)).map((p) => p.id),
			environment: 'all',
			environmentScopes: await resolveEnvironmentScopes(userId),
			currentUserId: userId,
			...overrides,
		} as any;
	}

	// -----------------------------------------------------------------------------------------
	// Acceptance rows 1 and 2: counts over ALL matching issues, not the page and not the selection.
	// -----------------------------------------------------------------------------------------

	it('row 1: status counts cover every matching issue, sum to All, and do not depend on the page', async () => {
		const { issueQueries } = await import('./issue-queries');
		const filters = await loaderFilters(openUser);

		const facets = await issueQueries.getIssueFacets(filters, NOW, openUser);

		// 33 reachable issues (30 in A, 3 in B); the 5 foreign ones never.
		expect(facets.status).toEqual({ all: 33, unresolved: 21, resolved: 10, ignored: 2 });
		expect(facets.status.unresolved + facets.status.resolved + facets.status.ignored).toBe(
			facets.status.all
		);
		// All equals what the list's footer would say, and more than any single page.
		expect(facets.status.all).toBe(await issueQueries.countIssues(filters));
		const page1 = await issueQueries.listIssues(filters, 1, 20);
		const page2 = await issueQueries.listIssues(filters, 2, 20);
		expect(page1).toHaveLength(20);
		expect(facets.status.all).toBeGreaterThan(page1.length);
		expect(page1.length + page2.length).toBe(33);
		// The reader takes no page or limit at all: calling it again after paging changes nothing.
		expect(await issueQueries.getIssueFacets(filters, NOW, openUser)).toEqual(facets);
	});

	it('row 2: with status=resolved selected, Unresolved and Ignored keep their database counts', async () => {
		const { issueQueries } = await import('./issue-queries');
		const facets = await issueQueries.getIssueFacets(
			await loaderFilters(openUser, { status: 'resolved' }),
			NOW,
			openUser
		);
		expect(facets.status).toEqual({ all: 33, unresolved: 21, resolved: 10, ignored: 2 });
		// …while the OTHER groups do narrow by the selected status.
		expect(facets.project.all).toBe(10);
		expect(facets.age.all).toBe(10);
		expect(facets.claim.total).toBe(10);
	});

	it('each group drops only its own filter: project, environment and age', async () => {
		const { issueQueries } = await import('./issue-queries');

		const inA = await issueQueries.getIssueFacets(
			await loaderFilters(openUser, { projectId: projectA, environment: 'staging' }),
			NOW,
			openUser
		);
		// Environment group: A's two environments, ignoring the selected environment.
		expect(inA.environment.byEnvironment).toEqual({ production: 20, staging: 10 });
		expect(inA.environment.all).toBe(30);
		// Project group ignores BOTH the project and the environment selection (a project change
		// drops the environment): every accessible project at its full count.
		expect(inA.project.byProject).toEqual({ [projectA]: 30, [projectB]: 3 });
		// Status narrows by project AND environment: staging in A is 6 unresolved + 4 resolved.
		expect(inA.status).toEqual({ all: 10, unresolved: 6, resolved: 4, ignored: 0 });
	});

	// -----------------------------------------------------------------------------------------
	// Row 5: the guards are in every group.
	// -----------------------------------------------------------------------------------------

	it('row 5: no group counts an issue outside projectIds, for any caller or selection', async () => {
		const { issueQueries } = await import('./issue-queries');

		// Even asking for the foreign project by id must not leak it into any group.
		for (const overrides of [{}, { projectId: projectForeign }, { projectId: projectA }]) {
			const facets = await issueQueries.getIssueFacets(
				await loaderFilters(openUser, overrides),
				NOW,
				openUser
			);
			expect(facets.project.byProject[projectForeign]).toBeUndefined();
			expect(Object.keys(facets.project.byProject).sort()).toEqual([projectA, projectB].sort());
		}
		const asked = await issueQueries.getIssueFacets(
			await loaderFilters(openUser, { projectId: projectForeign }),
			NOW,
			openUser
		);
		expect(asked.status.all).toBe(0);
		expect(asked.claim.total).toBe(0);
		expect(asked.age.all).toBe(0);
		expect(asked.environment.all).toBe(0);
	});

	it('row 5: a user with an empty authorised set gets zeros everywhere, not everything', async () => {
		const { issueQueries } = await import('./issue-queries');
		const facets = await issueQueries.getIssueFacets(
			{ status: 'all', projectId: 'all', projectIds: [], environment: 'all' } as any,
			NOW
		);
		expect(facets.status.all).toBe(0);
		expect(facets.project).toEqual({ all: 0, byProject: {} });
		expect(facets.claim.total).toBe(0);
	});

	it('row 5: a scoped user never has a staging issue counted — not in status, environment, project or claim', async () => {
		const { issueQueries } = await import('./issue-queries');

		// Population first: the scoped user DOES reach projectA's production and all of B.
		const filters = await loaderFilters(scopedUser);
		expect(await issueQueries.countIssues(filters)).toBe(20 + 3);

		// Selecting `staging` (the env filter) must not widen what the guard allows.
		for (const overrides of [{}, { projectId: projectA }, { projectId: projectA, environment: 'staging' }]) {
			const facets = await issueQueries.getIssueFacets(
				await loaderFilters(scopedUser, overrides),
				NOW,
				scopedUser
			);
			expect(facets.environment.byEnvironment.staging).toBeUndefined();
			expect(facets.project.byProject).toEqual({ [projectA]: 20, [projectB]: 3 });
			expect(facets.claim.total).toBe(
				'environment' in overrides ? 0 : 'projectId' in overrides ? 20 : 23
			);
		}
		const open = await issueQueries.getIssueFacets(filters, NOW, scopedUser);
		expect(open.status).toEqual({ all: 23, unresolved: 15, resolved: 6, ignored: 2 });
		expect(open.age.all).toBe(23);
	});

	// -----------------------------------------------------------------------------------------
	// Row 4: volume / seen / seen_from / seen_to narrow list, total and facets together.
	// -----------------------------------------------------------------------------------------

	it('row 4: volume and the last_seen window narrow the list, the count and every facet group together', async () => {
		const { issueQueries } = await import('./issue-queries');
		const { seenLowerBound } = await import('../issue-sort');

		const hot = await insert(
			{ project: projectB, environment: 'production', status: 'unresolved' },
			{ count: 500, lastSeenHoursAgo: 0.5, errorClass: 'HotSeed' }
		);
		const hotOld = await insert(
			{ project: projectB, environment: 'production', status: 'unresolved' },
			{ count: 500, lastSeenHoursAgo: 24 * 10, errorClass: 'HotOldSeed' }
		);
		const mid = await insert(
			{ project: projectB, environment: 'production', status: 'resolved' },
			{ count: 99, lastSeenHoursAgo: 0.5, errorClass: 'MidSeed' }
		);
		const hundred = await insert(
			{ project: projectB, environment: 'production', status: 'ignored' },
			{ count: 100, lastSeenHoursAgo: 0.5, errorClass: 'HundredSeed' }
		);

		const high = await loaderFilters(openUser, { volume: 'high' });
		const listed = (await issueQueries.listIssues(high, 1, 100)).map((r) => r.id);
		expect(listed.sort()).toEqual([hot, hotOld, hundred].sort()); // 100 is "100+", 99 is not
		expect(await issueQueries.countIssues(high)).toBe(3);
		const f = await issueQueries.getIssueFacets(high, NOW, openUser);
		expect(f.status).toEqual({ all: 3, unresolved: 2, resolved: 0, ignored: 1 });
		expect(f.project.all).toBe(3);

		const recent = await loaderFilters(openUser, {
			volume: 'high',
			seenAfter: seenLowerBound('24h', NOW),
		});
		expect((await issueQueries.listIssues(recent, 1, 100)).map((r) => r.id).sort()).toEqual(
			[hot, hundred].sort()
		);
		expect(await issueQueries.countIssues(recent)).toBe(2);
		expect((await issueQueries.getIssueFacets(recent, NOW, openUser)).status.all).toBe(2);

		// A closed custom range excludes the recent ones and keeps only the old one.
		const old = await loaderFilters(openUser, {
			volume: 'high',
			seenAfter: new Date(NOW.getTime() - 11 * 24 * HOUR),
			seenBefore: new Date(NOW.getTime() - 9 * 24 * HOUR),
		});
		expect((await issueQueries.listIssues(old, 1, 100)).map((r) => r.id)).toEqual([hotOld]);
		expect(await issueQueries.countIssues(old)).toBe(1);

		// Leave the shared counts above untouched for the other tests.
		await sql`delete from issues where id in (${hot}, ${hotOld}, ${mid}, ${hundred})`;
	});

	// -----------------------------------------------------------------------------------------
	// Row 7: LIKE wildcards in the search are literal.
	// -----------------------------------------------------------------------------------------

	it('row 7: a search for 100% matches the literal %, not 100 followed by anything', async () => {
		const { issueQueries } = await import('./issue-queries');
		const literal = await insert(
			{ project: projectB, environment: 'production', status: 'unresolved' },
			{ errorClass: `Rate100%Limit${suffix}` }
		);
		const lookalike = await insert(
			{ project: projectB, environment: 'production', status: 'unresolved' },
			{ errorClass: `Rate1000Limit${suffix}` }
		);
		const underscore = await insert(
			{ project: projectB, environment: 'production', status: 'unresolved' },
			{ errorClass: `a_b${suffix}` }
		);
		const noUnderscore = await insert(
			{ project: projectB, environment: 'production', status: 'unresolved' },
			{ errorClass: `axb${suffix}` }
		);

		const pct = await issueQueries.listIssues(await loaderFilters(openUser, { q: '100%' }), 1, 100);
		expect(pct.map((r) => r.id)).toEqual([literal]);
		expect(await issueQueries.countIssues(await loaderFilters(openUser, { q: '100%' }))).toBe(1);
		const us = await issueQueries.listIssues(
			await loaderFilters(openUser, { q: `a_b${suffix}` }),
			1,
			100
		);
		expect(us.map((r) => r.id)).toEqual([underscore]);
		const slash = await issueQueries.listIssues(await loaderFilters(openUser, { q: 'a\\b' }), 1, 100);
		expect(slash).toEqual([]);
		await sql`delete from issues where id in (${literal}, ${lookalike}, ${underscore}, ${noUnderscore})`;
	});

	// -----------------------------------------------------------------------------------------
	// Row 8 / D7: a claimant row's count equals the length of the list its link opens.
	// -----------------------------------------------------------------------------------------

	it('row 8: each claimant count equals the list its link opens; id and legacy email are separate rows', async () => {
		const { issueQueries } = await import('./issue-queries');
		const email = `facet-open-${suffix}@example.test`;
		await insert(
			{ project: projectB, environment: 'production', status: 'unresolved' },
			{ assignedTo: openUser, assigneeType: 'user' }
		);
		await insert(
			{ project: projectB, environment: 'production', status: 'resolved' },
			{ assignedTo: openUser, assigneeType: 'user' }
		);
		await insert(
			{ project: projectB, environment: 'production', status: 'unresolved' },
			{ assignedTo: email, assigneeType: 'user' }
		);
		await insert(
			{ project: projectB, environment: 'production', status: 'unresolved' },
			{ assignedTo: `agent-${suffix}`, assigneeType: 'agent' }
		);

		const filters = await loaderFilters(openUser);
		const claim = (await issueQueries.getIssueFacets(filters, NOW, openUser)).claim;

		expect(claim.byClaimant[openUser]).toBe(2);
		expect(claim.byClaimant[email]).toBe(1); // its own row — never added to the user's id
		expect(claim.claimedMe).toBe(2); // the server's `claimed-me` rule: id only
		expect(claim.claimedAgent).toBe(1);
		for (const [who, n] of Object.entries(claim.byClaimant)) {
			const rows = await issueQueries.listIssues({ ...filters, claim: who }, 1, 100);
			expect(rows).toHaveLength(n);
			expect(await issueQueries.countIssues({ ...filters, claim: who })).toBe(n);
		}
		expect(
			(await issueQueries.countIssues({ ...filters, claim: 'claimed-me' }))
		).toBe(claim.claimedMe);
		await sql`delete from issues where assigned_to in (${openUser}, ${email}, ${`agent-${suffix}`})
			and project_id = ${projectB}`;
	});

	// -----------------------------------------------------------------------------------------
	// D5: the default order has a unique final key; D8: no 100-project cap.
	// -----------------------------------------------------------------------------------------

	it('D5: last_seen ties are broken by id DESC, so offset pages never repeat or skip', async () => {
		const { issueQueries } = await import('./issue-queries');
		const tie = new Date(NOW.getTime() - 5 * 24 * HOUR).toISOString();
		const tied: string[] = [];
		for (let i = 0; i < 7; i++) {
			const id = await insert({ project: projectB, environment: 'production', status: 'unresolved' });
			await sql`update issues set last_seen = ${tie} where id = ${id}`;
			tied.push(id);
		}
		const filters = await loaderFilters(openUser, { projectId: projectB, q: undefined });
		const total = await issueQueries.countIssues(filters);
		const seen: string[] = [];
		for (let page = 1; page <= Math.ceil(total / 3); page++) {
			seen.push(...(await issueQueries.listIssues(filters, page, 3)).map((r) => r.id));
		}
		expect(new Set(seen).size).toBe(seen.length);
		expect(seen).toHaveLength(total);
		const tiedInOrder = seen.filter((id) => tied.includes(id));
		expect(tiedInOrder).toEqual([...tied].sort().reverse());
		await sql`delete from issues where id = any(${sql.array(tied)}::uuid[])`;
	});

	it('D8: listProjects returns the whole authorised set, past 100', async () => {
		const { issueQueries } = await import('./issue-queries');
		const extra: string[] = [];
		for (let i = 0; i < 105; i++) {
			const id = randomUUID();
			extra.push(id);
			await sql`insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${id}, ${`many-${i}-${suffix}`}, ${orgOwn}, ${`mk_${i}_${suffix}`}, ${`mh_${i}_${suffix}`})`;
		}
		try {
			expect(await issueQueries.listProjects([projectA, projectB, ...extra])).toHaveLength(107);
		} finally {
			await sql`delete from projects where id = any(${sql.array(extra)}::uuid[])`;
		}
	});
});
