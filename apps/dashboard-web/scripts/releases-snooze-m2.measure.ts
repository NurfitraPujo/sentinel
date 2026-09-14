import { describe, it, expect, afterAll } from 'vitest';
import postgres from 'postgres';
import { db } from '$lib/server/db';
import { listProjectReleases, RELEASES_PAGE_LIMIT } from '$lib/db/queries/releases';
import { requireProjectAccess } from '$lib/server/issue-access';
import { resolveEnvironmentScope } from '$lib/server/environment-access';

/**
 * sentinel-releases-snooze §13 M2 — `releases_page_p95_ms`, the LABELLED FALLBACK (decisions 23,
 * 35): the real `listProjectReleases` (§4.4's loader query — the list, the `COUNT(*)` beside it and
 * the ONE grouped `issuesIntroduced` count) driven against the SEEDED COMPOSE DATABASE with
 * `releases-snooze-seed.sql` loaded IMMEDIATELY before, as the measuring user (the loader's two
 * guards — `requireProjectAccess` and a null `resolveEnvironmentScope` — are run for that user
 * first, because they are what makes them the only caller who reaches the query, decision 20),
 * with every `issuesIntroduced` checked against an independent `SELECT count(*) FROM issues WHERE
 * project_id = A AND first_release_version = <version>` taken over ALL environments in plain SQL,
 * `truncated = true` at 300 with 200 rows newest first, the three statements' plans pasted exactly
 * as Drizzle emits them (captured from the executor, never retyped), and the three queries timed
 * TOGETHER over `RUNS` (≥ 30) executions. Evidence about plan shape on local data; never the
 * criterion met — `sentinel-staging-db` re-takes it via the held task.
 *
 * Run (the seed first, every time — its timestamps are now()-relative):
 *
 *   psql postgres://sentinel:changeme@localhost:5432/sentinel -f apps/dashboard-web/scripts/releases-snooze-seed.sql
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:5432/sentinel \
 *     pnpm vitest run --config scripts/vitest.measure.config.ts scripts/releases-snooze-m2.measure.ts
 *
 * `RUNS=60` for more timing runs. Fails hard when nothing answers at DATABASE_URL, when the seed's
 * organization is absent, or when any count disagrees — a driver that skips is a measurement that
 * did not happen.
 */
const connectionString = process.env.DATABASE_URL;
if (!connectionString) {
	throw new Error('DATABASE_URL is required: point it at the SEEDED compose database (decision 35)');
}
const RUNS = Number(process.env.RUNS ?? 40);

const ORG = '00000000-0000-4000-8000-0000000000c1';
const PROJECT_A = '00000000-0000-4000-8000-0000000000c2';
const MEASURING_USER = 'releases-snooze-seed-engineer';

const sql = postgres(connectionString, { max: 1 });
afterAll(async () => {
	await sql.end({ timeout: 2 });
});

function p(values: number[], q: number): number {
	const sorted = [...values].sort((a, b) => a - b);
	const idx = (sorted.length - 1) * q;
	const lo = Math.floor(idx);
	const hi = Math.ceil(idx);
	return sorted[lo] + (sorted[hi] - sorted[lo]) * (idx - lo);
}
function ms(t0: bigint): number {
	return Number(process.hrtime.bigint() - t0) / 1e6;
}
function fmt(n: number): string {
	return n.toFixed(3);
}
function table(rows: string[][]): string {
	const widths = rows[0].map((_, c) => Math.max(...rows.map((r) => r[c].length)));
	return rows
		.map((r, i) => {
			const line = r.map((cell, c) => cell.padEnd(widths[c])).join(' | ');
			return i === 0 ? `${line}\n${widths.map((w) => '-'.repeat(w)).join('-+-')}` : line;
		})
		.join('\n');
}

/**
 * The executor `listProjectReleases` accepts, wrapped so every statement it awaits is recorded —
 * `toSQL()` on the builder the moment `.then` is read, i.e. the text and parameters Drizzle sends
 * to Postgres for THAT call. The plans below are taken from these captures, so they cannot drift
 * from releases.ts.
 */
type Captured = { sql: string; params: unknown[] };
function capturingExecutor(sink: Captured[]): Pick<typeof db, 'select'> {
	const wrap = <T extends object>(target: T): T =>
		new Proxy(target, {
			get(t, prop, receiver) {
				const value = Reflect.get(t, prop, receiver);
				if (prop === 'then' && typeof value === 'function') {
					const q = (t as { toSQL: () => Captured }).toSQL();
					sink.push({ sql: q.sql, params: q.params });
					return value.bind(t);
				}
				if (typeof value === 'function') {
					return (...args: unknown[]) => {
						const result = value.apply(t, args);
						return result !== null && typeof result === 'object' && !Array.isArray(result) ? wrap(result) : result;
					};
				}
				return value;
			}
		});
	return { select: ((...args: Parameters<typeof db.select>) => wrap(db.select(...args))) as typeof db.select };
}

describe('M2 — the §4.4 loader query on the seeded compose database as the measuring user (LABELLED FALLBACK)', () => {
	it('precondition: the seed is loaded and the measuring user passes both loader guards with a null scope', async () => {
		const [org] = await sql<{ slug: string }[]>`select slug from organizations where id = ${ORG}`;
		expect(org?.slug, 'load releases-snooze-seed.sql first').toBe('releases-snooze-seed');
		const access = await requireProjectAccess(MEASURING_USER, PROJECT_A, 'read', { organizationId: ORG });
		expect(access.projectId).toBe(PROJECT_A);
		const scope = await resolveEnvironmentScope(MEASURING_USER, PROJECT_A);
		expect(scope, 'decision 20: only a null-scope caller reaches the query').toBeNull();
		console.log(`\n== M2 precondition — ${MEASURING_USER}: requireProjectAccess(read) ok, resolveEnvironmentScope = null\n`);
	});

	it('PRIMARY: every issuesIntroduced equals the independent all-environments count(*) per version; truncated = true at 300 with 200 rows newest first', async () => {
		const captured: Captured[] = [];
		const page = await listProjectReleases(PROJECT_A, capturingExecutor(captured));

		const [{ total }] = await sql<{ total: string }[]>`select count(*) as total from releases where project_id = ${PROJECT_A}`;
		expect(Number(total)).toBe(300);
		expect(page.truncated).toBe(true);
		expect(page.releases).toHaveLength(RELEASES_PAGE_LIMIT);
		for (let i = 1; i < page.releases.length; i++) {
			expect(page.releases[i - 1].firstSeen.getTime(), `row ${i - 1} → ${i} must be first_seen DESC`).toBeGreaterThanOrEqual(page.releases[i].firstSeen.getTime());
		}

		// The independent count: plain SQL, no scope clause, every environment of project A.
		const counts = await sql<{ version: string; all_envs: string; production: string; staging: string; canary: string }[]>`
			select first_release_version as version,
			       count(*) as all_envs,
			       count(*) filter (where environment = 'production') as production,
			       count(*) filter (where environment = 'staging') as staging,
			       count(*) filter (where environment = 'canary') as canary
			  from issues
			 where project_id = ${PROJECT_A} and first_release_version is not null
			 group by first_release_version`;
		const byVersion = new Map(counts.map((c) => [c.version, c]));

		let matches = 0;
		let nonZero = 0;
		const sample: string[][] = [['version', 'firstSeen', 'issuesIntroduced (loader)', 'count(*) all envs (SQL)', 'production', 'staging', 'canary', 'match']];
		for (const r of page.releases) {
			const c = byVersion.get(r.version);
			const expected = c ? Number(c.all_envs) : 0;
			const ok = r.issuesIntroduced === expected;
			if (ok) matches++;
			if (expected > 0) nonZero++;
			if (expected >= 50 || sample.length < 6 || !ok) {
				sample.push([r.version, r.firstSeen.toISOString(), String(r.issuesIntroduced), String(expected), c?.production ?? '0', c?.staging ?? '0', c?.canary ?? '0', ok ? 'yes' : 'NO']);
			}
		}
		console.log(
			`\n== M2 PRIMARY — listProjectReleases(A): truncated = ${page.truncated} (COUNT(*) = ${total}), ${page.releases.length} rows, newest first\n` +
				table(sample) +
				`\n${matches} of ${page.releases.length} rows: issuesIntroduced = SELECT count(*) … across all environments (${nonZero} non-zero; the three 50+ releases and the first rows are shown, plus any mismatch)\n`
		);
		expect(matches).toBe(page.releases.length);
		expect(captured, 'the loader query is exactly three statements: the list, the COUNT(*), the grouped count').toHaveLength(3);
	});

	it('SECONDARY: the plans, exactly as Drizzle emits the three statements — the list on idx_releases_project_first_seen, the grouped count on idx_issues_project_first_release', async () => {
		const captured: Captured[] = [];
		await listProjectReleases(PROJECT_A, capturingExecutor(captured));
		expect(captured).toHaveLength(3);
		const labels = ['(1) the list — 200 newest by first_seen', '(2) the COUNT(*) beside it (truncated)', '(3) the grouped issuesIntroduced count over the 200 versions'];
		let out = '\n== M2 SECONDARY — EXPLAIN (ANALYZE, BUFFERS) of the three statements listProjectReleases sends (captured from the executor)\n';
		const plans: string[] = [];
		for (let i = 0; i < captured.length; i++) {
			const { sql: text, params } = captured[i];
			const rows = await sql.unsafe(`EXPLAIN (ANALYZE, BUFFERS) ${text}`, params as never[]);
			const plan = rows.map((r) => r['QUERY PLAN']).join('\n');
			plans.push(plan);
			const shownParams = params.length > 6 ? `[${params.slice(0, 3).map(String).join(', ')}, … ${params.length} params]` : JSON.stringify(params);
			out += `\n-- ${labels[i]}\n${text}\n-- params: ${shownParams}\n${plan}\n`;
		}
		// The seed's `releases` table is 340 rows on a handful of pages, and at that size the planner
		// prefers a sequential scan + quicksort over the index for (1) — the correct choice for a
		// table this small, not evidence about the index. The same statement with sequential scans
		// disabled for ONE transaction shows the plan the index serves and a larger table would pick:
		// an index range scan on idx_releases_project_first_seen, no sort. Both are pasted; the
		// finding is stated in the output rather than asserted away.
		// The same is true of (3) on `issues` at 2,301 rows: the partial index is an Index Only Scan
		// whenever the visibility map is fresh (a VACUUMed table) and a Seq Scan otherwise — the
		// planner's call at this size, not the index's. Both statements are taken again under the
		// same SET LOCAL and pasted beside the default plan.
		const forced = async (i: number) =>
			sql.begin(async (tx) => {
				await tx`set local enable_seqscan = off`;
				const rows = await tx.unsafe(`EXPLAIN (ANALYZE, BUFFERS) ${captured[i].sql}`, captured[i].params as never[]);
				return rows.map((r) => r['QUERY PLAN']).join('\n');
			});
		const pagesOf = async (relname: string) => {
			const [{ pages }] = await sql<{ pages: string }[]>`select relpages as pages from pg_class where relname = ${relname}`;
			return pages;
		};
		const forcedList = await forced(0);
		const forcedCount = await forced(2);
		const describe = (plan: string, index: string) => (plan.includes(index) ? `already uses ${index}` : `chose a Seq Scan over ${index}`);
		out += `\n-- (1) again with SET LOCAL enable_seqscan = off (releases is ${await pagesOf('releases')} page(s); the default plan ${describe(plans[0], 'idx_releases_project_first_seen')} at this size)\n${forcedList}\n`;
		out += `\n-- (3) again with SET LOCAL enable_seqscan = off (issues is ${await pagesOf('issues')} page(s); the default plan ${describe(plans[2], 'idx_issues_project_first_release')} at this size)\n${forcedCount}\n`;
		console.log(out);
		expect(forcedList, 'the list is served by an index range scan on idx_releases_project_first_seen once the planner cannot choose a sequential scan').toContain('idx_releases_project_first_seen');
		expect(forcedList, 'the index order makes the Sort disappear').not.toContain('Sort Key');
		expect(forcedCount, 'the grouped count is served by idx_issues_project_first_release once the planner cannot choose a sequential scan').toContain('idx_issues_project_first_release');
	});

	it(`TERTIARY: releases_page_p95_ms — the three queries timed together over ${RUNS} runs`, async () => {
		await listProjectReleases(PROJECT_A); // warm
		const samples: number[] = [];
		for (let i = 0; i < RUNS; i++) {
			const t0 = process.hrtime.bigint();
			const page = await listProjectReleases(PROJECT_A);
			samples.push(ms(t0));
			expect(page.releases).toHaveLength(RELEASES_PAGE_LIMIT);
		}
		const rows: string[][] = [
			['variant (loader side, driver round trips included)', 'runs', 'p50 ms', 'p95 ms', 'p99 ms', 'max ms'],
			['listProjectReleases(A): list + COUNT(*) + grouped count', String(RUNS), fmt(p(samples, 0.5)), fmt(p(samples, 0.95)), fmt(p(samples, 0.99)), fmt(Math.max(...samples))]
		];
		console.log(`\n== M2 TERTIARY — releases_page_p95_ms\n${table(rows)}\np95 = ${fmt(p(samples, 0.95))} ms — §13 bound 50 ms, reported, not gated\n`);
		expect(samples).toHaveLength(RUNS);
	});
});
