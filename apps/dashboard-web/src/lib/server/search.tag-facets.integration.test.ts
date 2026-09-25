import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// sentinel-issue-tag-facets spec §6 and §10.2 (TASK-03, decision 5): /search by tag against a migrated
// Postgres — `device.model:SM-A115F` returns exactly the issues that have it, tags AND with each other and
// with the id OR-group, adding a tag never widens, overflowed values are found (branch (b)) through the
// key's index, the rows a tag search reads are bounded by the matching issues and never by an issue's
// size (5c), the per-issue form returns today's rows (5b), and the loader reads both `tag` spellings and
// the box's `key:value` tokens.
//
// THIS SUITE MUST NOT SKIP UNDER THE GATE: `task test-integration` runs it through
// scripts/test-dashboard-pg.sh with TAG_FACETS_INTEGRATION_REQUIRED=1 against a disposable, migrated
// Postgres, and CI's dashboard job sets the same flag. Every exactness assertion runs the shared check
// query scoped to this suite's issues ($1).

const connectionString = process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.TAG_FACETS_INTEGRATION_REQUIRED === '1';

let dbReachable = false;
let migrated = false;
const probeSql = postgres(connectionString, { connect_timeout: 3, max: 1, onnotice: () => {} });
try {
	await probeSql`select 1`;
	dbReachable = true;
	const [{ t }] = await probeSql`select to_regclass('issue_tag_values')::text as t`;
	migrated = t !== null;
} catch {
	dbReachable = false;
} finally {
	await probeSql.end({ timeout: 1 }).catch(() => {});
}
if (required && !(dbReachable && migrated)) {
	throw new Error(
		`TAG_FACETS_INTEGRATION_REQUIRED=1 but ${dbReachable ? 'issue_tag_values does not exist in' : 'no database answered at'} ` +
			`${connectionString}. This suite is the gate's never-skip proof of /search by tag ` +
			'(sentinel-issue-tag-facets contract decision 5); skipping it would leave that unproven.'
	);
}

// The search's db over a recording client: every statement searchErrors sends is kept (SQL and
// parameters), so the suite can EXPLAIN exactly what was run.
const recorded: { sql: string; params: unknown[] }[] = [];
vi.mock('$lib/server/db', async (importOriginal) => {
	const actual = await importOriginal<Record<string, unknown>>();
	const { drizzle } = await import('drizzle-orm/postgres-js');
	const pg = (await import('postgres')).default;
	const real = pg(process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel', {
		max: 2,
		onnotice: () => {},
		types: {
			jsonb: {
				to: 3802,
				from: [3802],
				serialize: (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v)),
				parse: (v: string) => JSON.parse(v),
			},
		},
	});
	const client = Object.assign((s: TemplateStringsArray, ...v: unknown[]) => real(s, ...(v as never[])), {
		options: real.options,
		unsafe: (q: string, p?: unknown[]) => {
			recorded.push({ sql: q, params: p ?? [] });
			return real.unsafe(q, p as never);
		},
		begin: real.begin.bind(real),
	});
	return { ...actual, db: drizzle(client as never) };
});
vi.mock('$env/dynamic/private', () => ({ env: { ...process.env } }));

const { searchErrors, tagCondition, rowsReadFromPlan } = await import('./search');
const { TAG_FACETS_CHECK_SQL } = await import('./tag-facets-check.sql');
const { tagSearchHref, formatSearchToken } = await import('$lib/tag-filter');
const { load } = await import('../../routes/search/+page.server');
const { db } = await import('./db');
const { issues, errorOccurrences, errorSearchIndex } = await import('$lib/db/schema');
const { and, eq, desc, sql: dsql, inArray } = await import('drizzle-orm');

const suffix = randomUUID().slice(0, 8);
const orgA = randomUUID();
const orgB = randomUUID();
const P1 = randomUUID();
const P2 = randomUUID();
const P3 = randomUUID(); // inaccessible
const P5c = randomUUID(); // the (5c) bound fixture's own project
const userU = `tfs-u-${suffix}`;
const userS = `tfs-s-${suffix}`; // scoped to P1 production
const allIssues: string[] = [];

type Sql = ReturnType<typeof postgres>;
let sql: Sql;

async function issue(projectId: string, environment: string): Promise<string> {
	const id = randomUUID();
	await sql`
		insert into issues (id, project_id, environment, fingerprint, message, error_class, status, first_seen, last_seen, count)
		values (${id}, ${projectId}, ${environment}, ${'fp-tfs-' + id}, ${'tag search ' + suffix}, 'TagSearchError',
		        'unresolved', now(), now() - (${allIssues.length} || ' seconds')::interval, 0)`;
	allIssues.push(id);
	return id;
}

interface Occ {
	issueId: string;
	projectId: string;
	environment: string;
	platform?: string;
	release?: string;
	tags?: Record<string, string>;
	deviceModel?: string;
	userId?: string; // event_user.id (the rollup's user.id)
	searchUserId?: string; // error_search_index.user_id
	searchTraceId?: string;
	indexed?: boolean;
	createdAt?: string;
}

async function occs(rows: Occ[]): Promise<void> {
	for (let off = 0; off < rows.length; off += 1000) {
		const chunk = rows.slice(off, off + 1000).map((r, i) => ({
			id: randomUUID(),
			issue_id: r.issueId,
			project_id: r.projectId,
			environment: r.environment,
			platform: r.platform ?? 'javascript',
			release_version: r.release ?? '',
			created_at: r.createdAt ?? new Date(Date.now() - (off + i) * 10).toISOString(),
			tags: r.tags ? JSON.stringify(r.tags) : null,
			contexts: r.deviceModel ? JSON.stringify({ device: { model: r.deviceModel } }) : null,
			event_user: r.userId ? JSON.stringify({ id: r.userId }) : null,
			indexed: r.indexed ?? true,
			s_user: r.searchUserId ?? null,
			s_trace: r.searchTraceId ?? null,
		}));
		const json = JSON.stringify(chunk);
		await sql`
			insert into error_occurrences (id, issue_id, project_id, environment, platform, release_version,
			                               stacktrace, metadata, created_at, tags, contexts, event_user)
			select x.id::uuid, x.issue_id::uuid, x.project_id::uuid, x.environment, x.platform, x.release_version,
			       '[]'::jsonb, '{}'::jsonb, x.created_at::timestamptz, x.tags::jsonb, x.contexts::jsonb, x.event_user::jsonb
			  from json_to_recordset(${json}::text::json)
			       as x(id text, issue_id text, project_id text, environment text, platform text, release_version text,
			            created_at text, tags text, contexts text, event_user text)`;
		await sql`
			insert into error_search_index (occurrence_id, user_id, trace_id)
			select x.id::uuid, x.s_user, x.s_trace
			  from json_to_recordset(${json}::text::json) as x(id text, indexed boolean, s_user text, s_trace text)
			 where x.indexed`;
	}
}

const ids = (r: { results: { issueId: string }[] }) => r.results.map((x) => x.issueId).sort();
const sorted = (a: string[]) => [...a].sort();
const U = () => ({ id: userU, email: `${userU}@example.test` }) as never;
const S = () => ({ id: userS, email: `${userS}@example.test` }) as never;

const F: Record<string, string> = {}; // named fixture issues
let small: string[] = []; // the (5c) fixture's 48 small staging issues

describe.skipIf(!(dbReachable && migrated))('search by tag (sentinel-issue-tag-facets, real Postgres)', () => {
	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 4, onnotice: () => {} });
		for (const [org, label] of [[orgA, 'a'], [orgB, 'b']] as const) {
			await sql`insert into organizations (id, name, slug) values (${org}, ${`tfs-${label}-${suffix}`}, ${`tfs-${label}-${suffix}`})`;
		}
		for (const [p, org, label] of [[P1, orgA, 'p1'], [P2, orgA, 'p2'], [P3, orgB, 'p3'], [P5c, orgA, 'p5c']] as const) {
			await sql`insert into projects (id, name, organization_id, api_key, api_key_hash)
			          values (${p}, ${`tfs-${label}-${suffix}`}, ${org}, ${`k_${label}_${suffix}`}, ${`h_${label}_${suffix}`})`;
		}
		for (const u of [userU, userS]) {
			await sql`insert into "user" (id, name, email) values (${u}, ${u}, ${`${u}@example.test`})`;
			await sql`insert into organization_members (organization_id, user_id, role) values (${orgA}, ${u}, 'engineer')`;
		}
		await sql`insert into project_environment_grants (user_id, project_id, environments, created_by)
		          values (${userS}, ${P1}, ${sql.array(['production'])}, ${userU})`;

		// (1)-(4), (6)
		F.i1 = await issue(P1, 'production');
		F.i2 = await issue(P2, 'production');
		F.i3 = await issue(P1, 'staging');
		F.i4 = await issue(P3, 'production');
		F.i5 = await issue(P1, 'production');
		F.unindexed = await issue(P1, 'production');
		F.ab = await issue(P2, 'production');
		F.k = await issue(P2, 'production');
		await occs([
			{ issueId: F.i1, projectId: P1, environment: 'production', platform: 'go', release: 'r1', deviceModel: 'SM-A115F', tags: { url: '/checkout' }, searchUserId: 'u-x' },
			{ issueId: F.i1, projectId: P1, environment: 'production', platform: 'go', release: 'r1', deviceModel: 'Pixel 7' },
			{ issueId: F.i2, projectId: P2, environment: 'production', release: 'r2', deviceModel: 'SM-A115F', tags: { url: '/checkout' }, searchTraceId: 'tr-x' },
			{ issueId: F.i3, projectId: P1, environment: 'staging', release: 'r1', deviceModel: 'SM-A115F' },
			{ issueId: F.i4, projectId: P3, environment: 'production', release: 'r1', deviceModel: 'SM-A115F' },
			{ issueId: F.i5, projectId: P1, environment: 'production', release: 'r1', deviceModel: 'Pixel 7', searchUserId: 'u-x' },
			{ issueId: F.unindexed, projectId: P1, environment: 'production', release: 'r1', deviceModel: 'SM-A115F', indexed: false },
			{ issueId: F.ab, projectId: P2, environment: 'production', tags: { 'a:b': 'c:d e/%' } },
			{ issueId: F.k, projectId: P2, environment: 'production', tags: { k: "it's (x)!~" } },
		]);

		// (5) overflow: 1,001 distinct values in ONE statement each — the 1,000 byte-smallest get named rows.
		F.ovDevice = await issue(P1, 'production');
		F.ovDeviceWithout = await issue(P1, 'production');
		F.ovUser = await issue(P1, 'production');
		F.ovUserWithout = await issue(P1, 'production');
		F.ovUrl = await issue(P1, 'production');
		const n = (i: number) => i.toString().padStart(4, '0');
		await occs(Array.from({ length: 1001 }, (_, i) => ({ issueId: F.ovDevice, projectId: P1, environment: 'production', deviceModel: i < 1000 ? `0${n(i)}` : 'SM-A115F' })));
		await occs(Array.from({ length: 1001 }, (_, i) => ({ issueId: F.ovDeviceWithout, projectId: P1, environment: 'production', deviceModel: `1${n(i)}` })));
		await occs(Array.from({ length: 1001 }, (_, i) => ({ issueId: F.ovUser, projectId: P1, environment: 'production', userId: i < 1000 ? `a${n(i)}` : 'zz-target' })));
		await occs(Array.from({ length: 1001 }, (_, i) => ({ issueId: F.ovUserWithout, projectId: P1, environment: 'production', userId: `b${n(i)}` })));
		await occs(Array.from({ length: 1001 }, (_, i) => ({ issueId: F.ovUrl, projectId: P1, environment: 'production', tags: { url: `/a/${n(i)}` } })));

		// (5c) the bound, in its own project: one production/go issue of 20,000 occurrences (30 of them with
		// the search-index user u-bound), 48 staging/javascript issues of 5, and the (5b) equal-timestamp
		// issue (two indexed occurrences at one created_at). Every one carries release r-bound, none overflowed.
		F.big = await issue(P5c, 'production');
		await sql`
			with o as (
			  insert into error_occurrences (id, issue_id, project_id, environment, platform, release_version, stacktrace, metadata, created_at)
			  select gen_random_uuid(), ${F.big}::uuid, ${P5c}::uuid, 'production', 'go', 'r-bound', '[]', '{}', now() - g * interval '1 second'
			    from generate_series(1, 20000) g
			  returning id, created_at)
			insert into error_search_index (occurrence_id, user_id)
			select id, case when row_number() over (order by created_at) <= 30 then 'u-bound' end from o`;
		small = [];
		for (let i = 0; i < 48; i++) {
			const id = await issue(P5c, 'staging');
			small.push(id);
			await occs(Array.from({ length: 5 }, (_, k) => ({ issueId: id, projectId: P5c, environment: 'staging', release: 'r-bound', createdAt: new Date(Date.now() - (i * 10 + k) * 1000).toISOString() })));
		}
		F.equalTs = await issue(P5c, 'staging');
		const at = new Date(Date.now() - 5_000).toISOString();
		await occs([
			{ issueId: F.equalTs, projectId: P5c, environment: 'staging', release: 'r-bound', createdAt: at },
			{ issueId: F.equalTs, projectId: P5c, environment: 'staging', release: 'r-bound', createdAt: at },
		]);
		// Every table the plans read is analyzed, as autovacuum keeps them in production. Left unanalyzed, `issues`
		// is estimated at one row per project and the planner may drive form J from it, re-reading the id
		// filter's M index rows once per issue (50 x 30 here): a plan production's statistics do not produce.
		await sql`analyze issues`;
		await sql`analyze projects`;
		await sql`analyze error_occurrences`;
		await sql`analyze error_search_index`;
		await sql`analyze issue_tag_values`;
	}, 300_000);

	afterAll(async () => {
		if (!sql) return;
		await sql`delete from issues where id = any(${allIssues}::uuid[])`;
		await sql`delete from project_environment_grants where user_id in (${userU}, ${userS})`;
		await sql`delete from organization_members where user_id in (${userU}, ${userS})`;
		await sql`delete from "user" where id in (${userU}, ${userS})`;
		await sql`delete from projects where id in (${P1}, ${P2}, ${P3}, ${P5c})`;
		await sql`delete from organizations where id in (${orgA}, ${orgB})`;
		await sql.end({ timeout: 5 });
	});

	it('the fixture rollup is exact (scoped check query)', async () => {
		const [row] = await sql.unsafe(TAG_FACETS_CHECK_SQL, [allIssues]);
		expect(Object.values(row).map(Number)).toEqual([0, 0, 0, 0, 0]);
		// SM-A115F sorts past the 1,000 byte-smallest models, so it is the overflow row's one occurrence.
		const ov = await sql`select count from issue_tag_values where issue_id = ${F.ovDevice} and key = 'device.model' and value = ''`;
		expect(ov.map((r) => Number(r.count))).toEqual([1]);
	});

	const SM = [{ key: 'device.model', value: 'SM-A115F' }];

	it('(1) device.model:SM-A115F returns exactly the issues having it, never the inaccessible project or an out-of-scope environment', async () => {
		expect(ids(await searchErrors(U(), { tags: SM }, { limit: 500 }))).toEqual(sorted([F.i1, F.i2, F.i3, F.ovDevice]));
		expect(ids(await searchErrors(S(), { tags: SM }, { limit: 500 }))).toEqual(sorted([F.i1, F.i2, F.ovDevice]));
	});

	it('(2) two tags -> the intersection', async () => {
		expect(ids(await searchErrors(U(), { tags: [...SM, { key: 'release', value: 'r1' }] }, { limit: 500 }))).toEqual(sorted([F.i1, F.i3]));
	});

	it('(3) tag + user_id + trace_id -> (user_id OR trace_id) AND tag', async () => {
		const r = await searchErrors(U(), { tags: SM, userId: 'u-x', traceId: 'tr-x' }, { limit: 500 });
		expect(ids(r)).toEqual(sorted([F.i1, F.i2]));
		expect(r.total).toBe(2);
	});

	it('(4) monotonicity: adding a tag never widens the result (10 filter sets)', async () => {
		const sets: Record<string, string | undefined>[] = [
			{},
			{ projectId: P1 },
			{ environment: 'production' },
			{ environment: 'staging' },
			{ platform: 'go' },
			{ userId: 'u-x' },
			{ traceId: 'tr-x' },
			{ userId: 'u-x', traceId: 'tr-x' },
			{ search: 'tag' },
			{ projectId: P2, environment: 'production' },
		];
		for (const f of sets) {
			const without = new Set(ids(await searchErrors(U(), f, { limit: 1000 })));
			const withTag = ids(await searchErrors(U(), { ...f, tags: SM }, { limit: 1000 }));
			for (const id of withTag) expect(without.has(id), `${JSON.stringify(f)}: ${id}`).toBe(true);
			expect(withTag).not.toContain(F.unindexed);
		}
	});

	it('(5) overflowed values are found through branch (b); an overflowed issue without the value is not', async () => {
		expect(ids(await searchErrors(U(), { tags: SM, projectId: P1 }, { limit: 500 }))).toContain(F.ovDevice);
		expect(ids(await searchErrors(U(), { tags: SM, projectId: P1 }, { limit: 500 }))).not.toContain(F.ovDeviceWithout);
		expect(ids(await searchErrors(U(), { tags: [{ key: 'user.id', value: 'zz-target' }] }, { limit: 500 }))).toEqual([F.ovUser]);
		// custom key url: '/checkout' has named rows on i1/i2 only; the overflowed issue does not carry it
		expect(ids(await searchErrors(U(), { tags: [{ key: 'url', value: '/checkout' }] }, { limit: 500 }))).toEqual(sorted([F.i1, F.i2]));
		expect(ids(await searchErrors(U(), { tags: [{ key: 'url', value: '/a/1000' }] }, { limit: 500 }))).toEqual([F.ovUrl]);
	});

	// ---- (5c) the bound, and (5b) equivalence ----
	let indexTables: Map<string, string>;
	async function explainRead(stmt: { sql: string; params: unknown[] }) {
		if (!indexTables) {
			const rows = await sql`select indexrelid::regclass::text as i, indrelid::regclass::text as t from pg_index
			                        where indrelid in ('error_occurrences'::regclass, 'error_search_index'::regclass)`;
			indexTables = new Map(rows.map((r) => [r.i as string, r.t as string]));
		}
		const [row] = await sql.unsafe(`EXPLAIN (ANALYZE, FORMAT JSON) ${stmt.sql}`, stmt.params as never);
		const plan = (row['QUERY PLAN'] as unknown[])[0] as { Plan: Record<string, unknown> };
		let limitChildRows = 0;
		const findLimit = (node: Record<string, unknown>) => {
			if (node['Node Type'] === 'Limit' && limitChildRows === 0) {
				limitChildRows = Number(((node.Plans as Record<string, unknown>[])[0] ?? {})['Actual Rows'] ?? 0);
			}
			for (const c of (node.Plans as Record<string, unknown>[] | undefined) ?? []) findLimit(c);
		};
		findLimit(plan.Plan);
		return { read: rowsReadFromPlan(plan, indexTables), r: limitChildRows };
	}
	async function runRecorded(filters: Record<string, unknown>) {
		recorded.length = 0;
		const out = await searchErrors(U(), { projectId: P5c, tags: [{ key: 'release', value: 'r-bound' }], ...filters }, { limit: 50 });
		expect(recorded.length).toBeGreaterThanOrEqual(2);
		const [result, count] = recorded.slice(-2);
		return { out, result, count };
	}

	it('(5c) the rows a tag search reads are bounded by the matching issues, never by an issue of 20,000', async () => {
		// per issue: n (occurrences), g (occurrences sharing the newest created_at), u = 0 for every one here
		const nOf = (id: string) => (id === F.big ? 20_000 : id === F.equalTs ? 2 : 5);
		const gOf = (id: string) => (id === F.equalTs ? 2 : 1);
		const allowance = (id: string, perIssue: number) => (nOf(id) <= 20 ? Math.max(perIssue, 2 * nOf(id)) : perIssue);
		const staging = [...small, F.equalTs];
		const cases: { name: string; filters: Record<string, unknown>; form: 'L' | 'J'; I: string[]; platform?: boolean; M?: number }[] = [
			{ name: 'tag alone', filters: {}, form: 'L', I: [F.big, ...staging] },
			{ name: 'tag + environment=staging', filters: { environment: 'staging' }, form: 'L', I: staging },
			{ name: 'tag + environment=production', filters: { environment: 'production' }, form: 'L', I: [F.big] },
			{ name: 'tag + platform=javascript', filters: { platform: 'javascript' }, form: 'L', I: [F.big, ...staging], platform: true },
			{ name: 'tag + an absent user_id', filters: { userId: 'nobody' }, form: 'J', I: [], M: 0 },
			{ name: 'tag + user_id=u-bound', filters: { userId: 'u-bound' }, form: 'J', I: [F.big], M: 30 },
		];
		for (const c of cases) {
			const { result, count } = await runRecorded(c.filters);
			const res = await explainRead(result);
			const cnt = await explainRead(count);
			let resultBound: number;
			let countBound: number;
			if (c.form === 'L') {
				resultBound = c.I.reduce((s, id) => s + allowance(id, c.platform ? 2 * (0 + 1) : 2 * (0 + gOf(id) + 1)), 0);
				countBound = c.I.reduce((s, id) => s + allowance(id, 2 * (0 + 1)), 0);
			} else {
				resultBound = 6 * c.M! + 2 * res.r;
				countBound = 3 * c.M!;
			}
			expect(res.read, `${c.name}: result read ${res.read} > bound ${resultBound}`).toBeLessThanOrEqual(resultBound);
			expect(cnt.read, `${c.name}: count read ${cnt.read} > bound ${countBound}`).toBeLessThanOrEqual(countBound);
			expect(res.read, `${c.name}: result`).toBeLessThan(10_000);
			expect(cnt.read, `${c.name}: count`).toBeLessThan(10_000);
		}
	}, 120_000);

	// Today's shape for the same conditions: DISTINCT ON over the inner join, and count(DISTINCT).
	async function reference(user: string, filters: { environment?: string; platform?: string; projectId: string; tags: { key: string; value: string }[]; userId?: string }) {
		const conds = [
			eq(issues.projectId, filters.projectId),
			...(filters.environment ? [eq(errorOccurrences.environment, filters.environment)] : []),
			...(filters.platform ? [eq(errorOccurrences.platform, filters.platform)] : []),
			...(filters.userId ? [eq(errorSearchIndex.userId, filters.userId)] : []),
			...filters.tags.map(tagCondition),
		];
		void user;
		const rows = await db
			.selectDistinctOn([issues.id], { issueId: issues.id, occurrenceId: errorOccurrences.id })
			.from(issues)
			.innerJoin(errorOccurrences, eq(errorOccurrences.issueId, issues.id))
			.innerJoin(errorSearchIndex, eq(errorSearchIndex.occurrenceId, errorOccurrences.id))
			.where(and(...conds))
			.orderBy(issues.id, desc(errorOccurrences.createdAt), desc(errorOccurrences.id));
		const [{ n }] = await db
			.select({ n: dsql<number>`cast(count(DISTINCT ${issues.id}) as integer)` })
			.from(issues)
			.innerJoin(errorOccurrences, eq(errorOccurrences.issueId, issues.id))
			.innerJoin(errorSearchIndex, eq(errorSearchIndex.occurrenceId, errorOccurrences.id))
			.where(and(...conds));
		return { pairs: rows.map((r) => `${r.issueId}/${r.occurrenceId}`).sort(), total: Number(n) };
	}

	it('(5b) equivalence: the per-issue form returns today\'s (issue, occurrence) rows and total, for every tag case', async () => {
		const tagSets = [[{ key: 'release', value: 'r-bound' }], [{ key: 'release', value: 'r1' }], SM, [{ key: 'url', value: '/a/1000' }], [{ key: 'user.id', value: 'zz-target' }]];
		const extras: { environment?: string; platform?: string }[] = [{}, { environment: 'staging' }, { environment: 'production' }, { platform: 'javascript' }, { platform: 'go' }];
		for (const projectId of [P1, P5c]) {
			for (const tags of tagSets) {
				for (const extra of extras) {
					const got = await searchErrors(U(), { projectId, tags, ...extra }, { limit: 1000 });
					const ref = await reference(userU, { projectId, tags, ...extra });
					const pairs = got.results.map((r) => `${r.issueId}/${r.matchingOccurrence.id}`).sort();
					expect(pairs, JSON.stringify({ projectId, tags, extra })).toEqual(ref.pairs);
					expect(got.total).toBe(ref.total);
				}
			}
		}
		// the equal-timestamp issue picks the occurrence id tie-break, as today
		const got = await searchErrors(U(), { projectId: P5c, tags: [{ key: 'release', value: 'r-bound' }], environment: 'staging' }, { limit: 1000 });
		expect(got.results.map((r) => r.issueId)).toContain(F.equalTs);
	}, 180_000);

	// ---- the loader: both tag spellings, the box's tokens, malformed parameters ----
	const locals = () => ({ auth: async () => ({ user: { id: userU, email: `${userU}@example.test` } }) });
	const loadAt = (qs: string) => load({ url: new URL(`http://x${qs}`), locals: locals() } as never) as Promise<{
		results: { issueId: string }[];
		filters: { tags: { key: string; value: string }[] };
		ignoredTagTerms: string[];
		pagination: { total: number };
	}>;

	it("(6) key 'a:b' = 'c:d e/%' and k = \"it's (x)!~\" round-trip through their tag links", async () => {
		const ab = await loadAt(tagSearchHref('a:b', 'c:d e/%'));
		expect(ab.results.map((r) => r.issueId)).toEqual([F.ab]);
		expect(ab.filters.tags).toMatchObject([{ key: 'a:b', value: 'c:d e/%' }]);
		const k = await loadAt(tagSearchHref('k', "it's (x)!~"));
		expect(k.results.map((r) => r.issueId)).toEqual([F.k]);
		const usp = await loadAt('/search?tag=k%3Ait%27s%2520%28x%29%21%7E');
		expect(usp.results.map((r) => r.issueId)).toEqual([F.k]);
	});

	it('(7) malformed parameters (P1-P6) are ignored, never an error: the results equal the no-tag query', async () => {
		const base = await loadAt(`/search?project=${P2}`);
		for (const bad of ['abc', 'a:b:c', ':v', 'k:', '%E0%A4%A:v', 'a%2520b:v', `k:${encodeURIComponent(encodeURIComponent('é'.repeat(513)))}`]) {
			const r = await loadAt(`/search?project=${P2}&tag=${encodeURIComponent(bad)}`);
			expect(r.results.map((x) => x.issueId).sort(), bad).toEqual(base.results.map((x) => x.issueId).sort());
			expect(r.pagination.total).toBe(base.pagination.total);
			expect(r.ignoredTagTerms.length).toBe(1);
		}
	});

	it('(8) the box token "a:b":"c:d e/%" equals (6)', async () => {
		const r = await loadAt(`/search?search=${encodeURIComponent(formatSearchToken('a:b', 'c:d e/%'))}`);
		expect(r.results.map((x) => x.issueId)).toEqual([F.ab]);
		expect(r.filters.tags).toMatchObject([{ key: 'a:b', value: 'c:d e/%' }]);
	});

	it('no tag: the five id filters return the same rows with an unrelated rollup present (§6.6)', async () => {
		for (const f of [{ userId: 'u-x' }, { traceId: 'tr-x' }, { userId: 'u-bound', projectId: P5c }]) {
			const a = ids(await searchErrors(U(), f, { limit: 1000 }));
			const ref = await db
				.selectDistinct({ id: issues.id })
				.from(issues)
				.innerJoin(errorOccurrences, eq(errorOccurrences.issueId, issues.id))
				.innerJoin(errorSearchIndex, eq(errorSearchIndex.occurrenceId, errorOccurrences.id))
				.where(
					and(
						inArray(issues.projectId, [P1, P2, P5c]),
						f.userId ? eq(errorSearchIndex.userId, f.userId) : eq(errorSearchIndex.traceId, (f as { traceId: string }).traceId)
					)
				);
			expect(a).toEqual(ref.map((r) => r.id).sort());
		}
	});
});
