import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

/**
 * sentinel-issue-list-filters §5 item 8 — the six new server filters (release, type, source,
 * regressed, waiting, tags) against a REAL migrated Postgres: each narrows `listIssues`,
 * `countIssues` and the facet groups identically; release is found through the rollup AND through
 * the overflow branch; two tags AND; filters compose; and the guards still hold under every
 * combination. Seeds its own rows under a per-run suffix and deletes them.
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

describe.skipIf(!dbReachable)('issue list filters (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;
	const F: Record<string, string> = {};

	async function issue(
		key: string,
		o: {
			project?: string;
			environment?: string;
			type?: string;
			source?: string;
			regressed?: boolean;
			waiting?: string | null;
		} = {}
	) {
		const id = randomUUID();
		F[key] = id;
		await sql`
			insert into issues (id, project_id, environment, fingerprint, message, error_class, status,
			                    count, last_seen, first_seen, issue_type, source_channel,
			                    regression_status, waiting_on)
			values (${id}, ${o.project ?? projectA}, ${o.environment ?? 'production'}, ${`fp-${id}`},
			        ${`list filter ${suffix}`}, 'ListFilterError', 'unresolved', 1, now(), now(),
			        ${o.type ?? 'system_error'}, ${o.source ?? 'ingestion_sdk'},
			        ${o.regressed ? 'regressed' : 'none'}, ${o.waiting ?? null})`;
		return id;
	}

	async function occs(
		issueId: string,
		project: string,
		rows: { release?: string; tags?: Record<string, string>; environment?: string }[]
	) {
		const json = JSON.stringify(
			rows.map((r) => ({
				id: randomUUID(),
				release_version: r.release ?? '',
				environment: r.environment ?? 'production',
				tags: r.tags ? JSON.stringify(r.tags) : null,
			}))
		);
		await sql`
			insert into error_occurrences (id, issue_id, project_id, environment, platform, release_version,
			                               stacktrace, metadata, created_at, tags)
			select x.id::uuid, ${issueId}::uuid, ${project}::uuid, x.environment, 'javascript',
			       x.release_version, '[]'::jsonb, '{}'::jsonb, now(), x.tags::jsonb
			  from json_to_recordset(${json}::text::json)
			       as x(id text, release_version text, environment text, tags text)`;
	}

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		await sql`select pg_advisory_lock_shared(hashtext('sentinel.retention.integration'))`;
		for (const [id, label] of [
			[orgOwn, 'own'],
			[orgForeign, 'foreign'],
		] as const) {
			await sql`insert into organizations (id, name, slug)
				values (${id}, ${`lf-${label}-${suffix}`}, ${`lf-${label}-${suffix}`})`;
		}
		for (const [id, org, label] of [
			[projectA, orgOwn, 'a'],
			[projectForeign, orgForeign, 'foreign'],
		] as const) {
			await sql`insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${id}, ${`lf-${label}-${suffix}`}, ${org}, ${`k_${label}_${suffix}`}, ${`h_${label}_${suffix}`})`;
		}
		for (const [id, label] of [
			[openUser, 'open'],
			[scopedUser, 'scoped'],
		] as const) {
			await sql`insert into "user" (id, name, email)
				values (${id}, ${label}, ${`lf-${label}-${suffix}@example.test`})`;
			await sql`insert into organization_members (organization_id, user_id, role)
				values (${orgOwn}, ${id}, 'engineer')`;
		}
		await sql`insert into project_environment_grants (user_id, project_id, environments, created_by)
			values (${scopedUser}, ${projectA}, ${sql.array(['production'])}, ${openUser})`;

		// Column filters.
		await issue('plain');
		await issue('userReport', { type: 'user_report', source: 'user_report_api' });
		await issue('api', { source: 'api' });
		await issue('regressed', { regressed: true });
		await issue('waitReporter', { waiting: 'reporter' });
		await issue('waitTeam', { waiting: 'team' });

		// Release through the rollup, and tags.
		await issue('rel1');
		await occs(F.rel1, projectA, [{ release: '1.0.0', tags: { team: 'blue', plan: 'pro' } }]);
		await issue('rel1b', { type: 'user_report' });
		await occs(F.rel1b, projectA, [{ release: '1.0.0', tags: { team: 'blue' } }]);
		await issue('rel2');
		await occs(F.rel2, projectA, [{ release: '2.0.0', tags: { team: 'red', plan: 'pro' } }]);

		// Release through the OVERFLOW branch: 1,001 distinct releases on one issue — the 1,000
		// byte-smallest get named rollup rows, `zz-last` only the overflow ('') row.
		await issue('overflow');
		const n = (i: number) => i.toString().padStart(4, '0');
		await occs(
			F.overflow,
			projectA,
			Array.from({ length: 1001 }, (_, i) => ({ release: i < 1000 ? `ov-${n(i)}` : 'zz-last' }))
		);
		await issue('overflowWithout');
		await occs(
			F.overflowWithout,
			projectA,
			Array.from({ length: 1001 }, (_, i) => ({ release: `ow-${n(i)}` }))
		);

		// Guards: foreign project and a staging issue (outside the scoped user's grant) that match.
		await issue('foreign', {
			project: projectForeign,
			type: 'user_report',
			source: 'api',
			regressed: true,
			waiting: 'team',
		});
		await occs(F.foreign, projectForeign, [{ release: '1.0.0', tags: { team: 'blue' } }]);
		await issue('staging', {
			environment: 'staging',
			type: 'user_report',
			source: 'api',
			regressed: true,
			waiting: 'team',
		});
		await occs(F.staging, projectA, [
			{ release: '1.0.0', environment: 'staging', tags: { team: 'blue', plan: 'pro' } },
		]);
	}, 120_000);

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issues where project_id in (${projectA}, ${projectForeign})`;
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

	async function loaderFilters(userId: string, overrides: Record<string, unknown> = {}) {
		const { getAccessibleProjects } = await import('../issue-access');
		const { resolveEnvironmentScopes } = await import('../environment-access');
		return {
			status: 'all',
			projectId: projectA,
			projectIds: (await getAccessibleProjects(userId)).map((p) => p.id),
			environment: 'all',
			environmentScopes: await resolveEnvironmentScopes(userId),
			currentUserId: userId,
			...overrides,
		} as any;
	}

	/** list, count and facet totals for a filter set — they must agree by construction. */
	async function run(userId: string, overrides: Record<string, unknown>) {
		const { issueQueries } = await import('./issue-queries');
		const filters = await loaderFilters(userId, overrides);
		const list = await issueQueries.listIssues(filters, 1, 100);
		const count = await issueQueries.countIssues(filters);
		const facets = await issueQueries.getIssueFacets(filters, new Date(), userId);
		const mine = new Set(Object.values(F));
		const got = list.map((i: any) => i.id).filter((id: string) => mine.has(id));
		expect(count).toBe(list.length);
		expect(facets.status.all).toBe(list.length);
		expect(facets.age.all).toBe(list.length);
		expect(facets.project.all).toBe(list.length);
		return got.sort();
	}
	const ids = (...keys: string[]) => keys.map((k) => F[k]).sort();

	it('type narrows list, count and facets identically', async () => {
		expect(await run(openUser, { issueType: 'user_report' })).toEqual(
			ids('userReport', 'rel1b', 'staging')
		);
	});

	it('source narrows', async () => {
		expect(await run(openUser, { source: 'api' })).toEqual(ids('api', 'staging'));
	});

	it('regressed narrows', async () => {
		expect(await run(openUser, { regressed: true })).toEqual(ids('regressed', 'staging'));
	});

	it('waiting: any, reporter, team', async () => {
		expect(await run(openUser, { waiting: 'any' })).toEqual(
			ids('waitReporter', 'waitTeam', 'staging')
		);
		expect(await run(openUser, { waiting: 'reporter' })).toEqual(ids('waitReporter'));
		expect(await run(openUser, { waiting: 'team' })).toEqual(ids('waitTeam', 'staging'));
	});

	it('release via the rollup', async () => {
		expect(await run(openUser, { release: '1.0.0' })).toEqual(ids('rel1', 'rel1b', 'staging'));
		expect(await run(openUser, { release: '2.0.0' })).toEqual(ids('rel2'));
		expect(await run(openUser, { release: 'nope' })).toEqual([]);
	});

	it('release via the overflow branch (a release past the rollup cap)', async () => {
		expect(await run(openUser, { release: 'zz-last' })).toEqual(ids('overflow'));
		// a named release of the overflowed issue is found too, and the other overflowed issue is not
		expect(await run(openUser, { release: 'ov-0007' })).toEqual(ids('overflow'));
		expect(await run(openUser, { release: 'ow-0007' })).toEqual(ids('overflowWithout'));
	});

	it('two tags AND, and one tag alone is wider', async () => {
		const blue = { key: 'team', value: 'blue' };
		const pro = { key: 'plan', value: 'pro' };
		expect(await run(openUser, { tags: [blue] })).toEqual(ids('rel1', 'rel1b', 'staging'));
		expect(await run(openUser, { tags: [blue, pro] })).toEqual(ids('rel1', 'staging'));
		expect(await run(openUser, { tags: [blue, { key: 'plan', value: 'free' }] })).toEqual([]);
	});

	it('a tag=release:… behaves like the release filter (one predicate)', async () => {
		expect(await run(openUser, { tags: [{ key: 'release', value: '1.0.0' }] })).toEqual(
			await run(openUser, { release: '1.0.0' })
		);
	});

	it('tag + release + type compose', async () => {
		expect(
			await run(openUser, {
				tags: [{ key: 'team', value: 'blue' }],
				release: '1.0.0',
				issueType: 'user_report',
			})
		).toEqual(ids('rel1b', 'staging'));
	});

	it('the guards hold under every combination', async () => {
		const everything = {
			issueType: 'user_report',
			source: 'api',
			regressed: true,
			waiting: 'team',
			release: '1.0.0',
			tags: [
				{ key: 'team', value: 'blue' },
				{ key: 'plan', value: 'pro' },
			],
		};
		// open user: the staging issue matches all of it; the foreign project's never does
		expect(await run(openUser, everything)).toEqual(ids('staging'));
		// scoped user (production only): the staging issue is out of grant, the foreign one out of org
		expect(await run(scopedUser, everything)).toEqual([]);
		for (const o of [{ issueType: 'user_report' }, { source: 'api' }, { regressed: true }, { waiting: 'team' }, { release: '1.0.0' }, { tags: [{ key: 'team', value: 'blue' }] }]) {
			const got = await run(scopedUser, o);
			expect(got).not.toContain(F.staging);
			expect(got).not.toContain(F.foreign);
			// across projects too
			const all = await run(openUser, { ...o, projectId: 'all' });
			expect(all).not.toContain(F.foreign);
		}
	});
});
