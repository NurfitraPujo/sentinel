import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

/**
 * sentinel-reporting-and-dashboard D19 — the `service` filter and facet against a REAL migrated Postgres. Raw
 * `tags.service` already lands in `issue_tag_values` (no derived key needed), so the filter finds an issue with at least one
 * occurrence from the service, the facet counts issues per service under every OTHER filter, and the guards hold: a
 * foreign project and an environment outside a scoped grant never appear, not even as a facet count. Seeds its own rows
 * under a per-run suffix and deletes them.
 *
 * Point DATABASE_URL at a DISPOSABLE database (see issue-queries.facets.integration.test.ts).
 * SCHEMA_DRIFT_REQUIRED=1 / READ_GUARDS_INTEGRATION_REQUIRED=1 make "no database" a failure.
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
	throw new Error(`a database is required but none answered at ${connectionString}`);
}

const suffix = randomUUID().slice(0, 8);
const orgOwn = randomUUID();
const orgForeign = randomUUID();
const projectA = randomUUID();
const projectForeign = randomUUID();
const openUser = randomUUID();
const scopedUser = randomUUID(); // granted {production} in projectA only

describe.skipIf(!dbReachable)('service filter and facet (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;
	let backfillWasDone = false;
	const F: Record<string, string> = {};

	async function issueWith(key: string, o: { project?: string; environment?: string; status?: string; services: string[] }) {
		const id = randomUUID();
		F[key] = id;
		await sql`
			insert into issues (id, project_id, environment, fingerprint, message, error_class, status, count, last_seen, first_seen)
			values (${id}, ${o.project ?? projectA}, ${o.environment ?? 'production'}, ${`fp-${id}`}, ${`svc ${suffix}`}, 'SvcError',
			        ${o.status ?? 'unresolved'}, 1, now(), now())`;
		for (const service of o.services) {
			await sql`
				insert into error_occurrences (id, issue_id, project_id, environment, platform, stacktrace, metadata, created_at, tags)
				values (${randomUUID()}, ${id}, ${o.project ?? projectA}, ${o.environment ?? 'production'}, 'go', '[]'::jsonb, '{}'::jsonb, now(),
				        ${JSON.stringify({ service })}::text::jsonb)`;
		}
	}

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		await sql`select pg_advisory_lock_shared(hashtext('sentinel.retention.integration'))`;
		// The rollup's triggers count an occurrence only once the one-time backfill is done (issue_tag_counted); a freshly
		// migrated database has not run it. This suite needs the steady state production is in, so it sets the flag and
		// restores it afterwards (it is a disposable database; see the header).
		[{ done: backfillWasDone }] = await sql`select done from issue_tag_values_backfill where singleton`;
		await sql`update issue_tag_values_backfill set done = true where singleton`;
		for (const [id, label] of [[orgOwn, 'own'], [orgForeign, 'foreign']] as const) {
			await sql`insert into organizations (id, name, slug) values (${id}, ${`sv-${label}-${suffix}`}, ${`sv-${label}-${suffix}`})`;
		}
		for (const [id, org, label] of [[projectA, orgOwn, 'a'], [projectForeign, orgForeign, 'foreign']] as const) {
			await sql`insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${id}, ${`sv-${label}-${suffix}`}, ${org}, ${`k_${label}_${suffix}`}, ${`h_${label}_${suffix}`})`;
		}
		for (const [id, label] of [[openUser, 'open'], [scopedUser, 'scoped']] as const) {
			await sql`insert into "user" (id, name, email) values (${id}, ${label}, ${`sv-${label}-${suffix}@example.test`})`;
			await sql`insert into organization_members (organization_id, user_id, role) values (${orgOwn}, ${id}, 'engineer')`;
		}
		await sql`insert into project_environment_grants (user_id, project_id, environments, created_by)
			values (${scopedUser}, ${projectA}, ${sql.array(['production'])}, ${openUser})`;

		await issueWith('api', { services: ['api'] });
		await issueWith('jobs', { services: ['jobs worker'] });
		await issueWith('both', { services: ['api', 'jobs worker'] });
		await issueWith('resolvedApi', { services: ['api'], status: 'resolved' });
		await issueWith('none', { services: [] });
		await issueWith('stagingApi', { services: ['api'], environment: 'staging' });
		await issueWith('foreignApi', { project: projectForeign, services: ['api', 'foreign-only'] });
	}, 120_000);

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issues where project_id in (${projectA}, ${projectForeign})`;
			await sql`update issue_tag_values_backfill set done = ${backfillWasDone} where singleton`;
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

	async function filters(userId: string, overrides: Record<string, unknown> = {}) {
		const { getAccessibleProjects } = await import('../issue-access');
		const { resolveEnvironmentScopes } = await import('../environment-access');
		return {
			status: 'all', projectId: projectA, projectIds: (await getAccessibleProjects(userId)).map((p) => p.id), environment: 'all',
			environmentScopes: await resolveEnvironmentScopes(userId), currentUserId: userId, ...overrides,
		} as any;
	}
	const mine = () => new Set(Object.values(F));
	async function list(userId: string, overrides: Record<string, unknown> = {}) {
		const { issueQueries } = await import('./issue-queries');
		const f = await filters(userId, overrides);
		const rows = await issueQueries.listIssues(f, 1, 100);
		const count = await issueQueries.countIssues(f);
		expect(count).toBe(rows.length); // the total agrees with the list by construction
		return rows.map((r: any) => r.id).filter((id: string) => mine().has(id)).sort();
	}
	const ids = (...keys: string[]) => keys.map((k) => F[k]).sort();

	it('service=api returns issues with at least one occurrence from api, across statuses, inside the guards', async () => {
		expect(await list(openUser, { service: 'api' })).toEqual(ids('api', 'both', 'resolvedApi', 'stagingApi'));
	});

	it('a service with a space matches exactly, and no service value matches nothing', async () => {
		expect(await list(openUser, { service: 'jobs worker' })).toEqual(ids('jobs', 'both'));
		expect(await list(openUser, { service: 'does-not-exist' })).toEqual([]);
		expect(await list(openUser, { service: 'all' })).toEqual(ids('api', 'jobs', 'both', 'resolvedApi', 'none', 'stagingApi'));
	});

	it('composes with other filters', async () => {
		expect(await list(openUser, { service: 'api', status: 'resolved' })).toEqual(ids('resolvedApi'));
		expect(await list(openUser, { service: 'api', environment: 'staging' })).toEqual(ids('stagingApi'));
	});

	it('guards: a foreign project and an out-of-grant environment never appear, even with the matching service', async () => {
		expect(await list(openUser, { service: 'foreign-only' })).toEqual([]);
		// the scoped user is granted {production} only: the staging issue is invisible to the service filter too
		expect(await list(scopedUser, { service: 'api' })).toEqual(ids('api', 'both', 'resolvedApi'));
		// asking for the foreign project by id still reads nothing of it
		expect(await list(openUser, { service: 'api', projectId: projectForeign })).toEqual([]);
	});

	it('facet: issues per service under every other filter, dropping its own, and never counting a foreign project', async () => {
		const { issueQueries } = await import('./issue-queries');
		const f = await filters(openUser);
		const all = await issueQueries.getIssueFacets(f, new Date(), openUser);
		expect(all.service.byService['api']).toBe(4);
		expect(all.service.byService['jobs worker']).toBe(2);
		expect(all.service.byService['foreign-only']).toBeUndefined();
		// selecting a service does not narrow the service facet itself (it drops its own filter) ...
		const selected = await issueQueries.getIssueFacets(await filters(openUser, { service: 'api' }), new Date(), openUser);
		expect(selected.service.byService['jobs worker']).toBe(2);
		// ... but every other facet group narrows with it
		expect(selected.status.all).toBe(4);
		// other filters do narrow the service facet
		const resolvedOnly = await issueQueries.getIssueFacets(await filters(openUser, { status: 'resolved' }), new Date(), openUser);
		expect(resolvedOnly.service.byService).toEqual({ api: 1 });
		// the scoped user's facet carries no staging issue
		const scoped = await issueQueries.getIssueFacets(await filters(scopedUser), new Date(), scopedUser);
		expect(scoped.service.byService['api']).toBe(3);
	});

	it('servicesForIssues names each listed issue\'s services, bounded, for exactly the ids it is given', async () => {
		const { issueQueries } = await import('./issue-queries');
		const m = await issueQueries.servicesForIssues([F.api, F.both, F.none, F.foreignApi]);
		expect(m.get(F.api)).toEqual(['api']);
		expect([...(m.get(F.both) ?? [])].sort()).toEqual(['api', 'jobs worker']);
		expect(m.get(F.none)).toEqual([]);
		expect((await issueQueries.servicesForIssues([])).size).toBe(0);
		expect((await issueQueries.servicesForIssues([F.both], 1)).get(F.both)).toHaveLength(1);
	});
});
