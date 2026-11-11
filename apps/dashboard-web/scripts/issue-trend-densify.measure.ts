import { describe, it, expect, afterAll } from 'vitest';
import postgres from 'postgres';
import { issueQueries } from '$lib/server/queries/issue-queries';
import { detailTrendWindow, listTrendWindow, DAY_MS, HOUR_MS } from '$lib/server/trend-window';

/**
 * sentinel-issue-trend §13 M1 / M3 — the LOADER side of the labelled fallback (decision 15): the
 * real `listIssues` + `trendForIssues` (§4.1) and the real `trendForIssue` (§4.2) driven against a
 * disposable Postgres seeded by `issue-trend-seed.sql` IMMEDIATELY before, with the arrays they
 * return checked against `SELECT sum(count)` taken over the same rows in plain SQL, and the two
 * loaders' query sequences timed over `RUNS` (≥ 30) executions. `issue-trend-m1.sql` and
 * `issue-trend-m3.sql` are the SQL side (plans, and the same sums from psql); this file exists
 * because the densify — `trend24h` being exactly 24 long and summing to the rows, `daily[i]` being
 * the per-UTC-day sum with `start = window_start + i days` — is JavaScript, and a SQL script cannot
 * observe it. Evidence about shape on local data; never the criterion met.
 *
 * Run (see the seed's header for the container and the migration; the seed first, every time):
 *
 *   psql postgres://sentinel:changeme@localhost:15806/sentinel -f apps/dashboard-web/scripts/issue-trend-seed.sql
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15806/sentinel \
 *     pnpm vitest run --config scripts/vitest.measure.config.ts
 *
 * `RUNS=60` for more timing runs. Fails hard when nothing answers at DATABASE_URL — a driver that
 * skips is a measurement that did not happen.
 */
const connectionString = process.env.DATABASE_URL;
if (!connectionString) {
	throw new Error('DATABASE_URL is required: point it at the seeded DISPOSABLE database, never the shared dev one');
}
const RUNS = Number(process.env.RUNS ?? 40);

const PROJECT_A = '00000000-0000-4000-8000-0000000000b2';
const PROJECT_B = '00000000-0000-4000-8000-0000000000b3';
const FULLY_POPULATED = '00000000-0000-4000-8000-00000b200191'; // n = 401, plus the trunc(now) + 1h row
const STEADY_1 = '00000000-0000-4000-8000-00000b200001';

const sql = postgres(connectionString, { max: 1 });
afterAll(async () => {
	await sql.end({ timeout: 2 });
});

// ONE instant, the way a loader takes one per request.
const now = new Date();
const list = listTrendWindow(now);
const detail = detailTrendWindow(now);

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
/** §4.2's `TrendPoint.start` spelling (RFC 3339 UTC, `Z`, whole seconds) — the loader's `slotStartIso`. */
function iso(d: Date): string {
	return d.toISOString().slice(0, 19) + 'Z';
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

// The measuring user's filters for `?project=<A>&status=all&page=1`, as `+page.server.ts` builds
// them: the selected project ANDed with the tenancy predicate over both projects; no environment
// filter and no environment grants (the seed user has none).
const filtersA = {
	status: 'all',
	projectId: PROJECT_A,
	projectIds: [PROJECT_A, PROJECT_B],
	environment: 'all',
	environmentScopes: new Map<string, string[]>()
};

describe('M1 — the §4.1 loader: listIssues + trendForIssues on the seed (LABELLED FALLBACK)', () => {
	it('every returned issue has a trend24h of length 24 whose sum equals SELECT sum(count) over the bounded window', async () => {
		const page1 = await issueQueries.listIssues({ ...filtersA, sort: 'last_seen' }, 1, 20);
		expect(page1).toHaveLength(20);
		const ids = page1.map((i) => i.id);
		const trends = await issueQueries.trendForIssues(ids, list.windowStart, list.windowEnd);

		const sums = await sql<{ issue_id: string; rows: string; bounded: string; lower_only: string }[]>`
			select p.id as issue_id,
			       count(b.bucket_start) filter (where b.bucket_start <= ${list.windowEnd}) as rows,
			       coalesce(sum(b.count) filter (where b.bucket_start <= ${list.windowEnd}), 0) as bounded,
			       coalesce(sum(b.count), 0) as lower_only
			from unnest(${ids}::uuid[]) as p(id)
			left join issue_occurrence_buckets b on b.issue_id = p.id and b.bucket_start >= ${list.windowStart}
			group by p.id`;
		const byId = new Map(sums.map((s) => [s.issue_id, s]));

		const rows: string[][] = [['id', 'trend24h.length', 'sum(trend24h)', 'SQL sum(count) bounded', 'SQL sum lower-bound only', 'rows in window', 'match']];
		let matches = 0;
		for (const id of ids) {
			const series = trends.get(id);
			expect(series, `trendForIssues returned no entry for ${id}`).toBeDefined();
			const sum = series!.reduce((a, b) => a + b, 0);
			const s = byId.get(id)!;
			const ok = series!.length === 24 && sum === Number(s.bounded);
			if (ok) matches++;
			rows.push([id, String(series!.length), String(sum), s.bounded, s.lower_only, s.rows, ok ? 'yes' : 'NO']);
		}
		console.log(
			`\n== M1 PRIMARY (loader side) — ?project=<A>&status=all&page=1, window [${list.windowStart.toISOString()} … ${list.windowEnd.toISOString()}]\n` +
				table(rows) +
				`\n${matches} of ${ids.length} ids: trend24h.length = 24 and sum(trend24h) = SQL sum(count) over the bounded window\n`
		);
		expect(matches).toBe(ids.length);
		expect(trends.size).toBe(ids.length);
	});

	it('beside: the fully-populated issue (n = 401) through trendForIssues — the trunc(now) + 1h row is in no slot', async () => {
		const trends = await issueQueries.trendForIssues([FULLY_POPULATED, STEADY_1], list.windowStart, list.windowEnd);
		const [{ bounded, lower_only, future_rows }] = await sql<{ bounded: string; lower_only: string; future_rows: string }[]>`
			select coalesce(sum(count) filter (where bucket_start <= ${list.windowEnd}), 0) as bounded,
			       coalesce(sum(count), 0) as lower_only,
			       count(*) filter (where bucket_start > ${list.windowEnd}) as future_rows
			from issue_occurrence_buckets
			where issue_id = ${FULLY_POPULATED} and bucket_start >= ${list.windowStart}`;
		const series = trends.get(FULLY_POPULATED)!;
		const sum = series.reduce((a, b) => a + b, 0);
		console.log(
			`\n== M1 beside — n = 401 trend24h: length ${series.length}, sum ${sum}; SQL bounded ${bounded}, lower-bound-only ${lower_only} (${future_rows} row(s) after window_end, count 999)\n` +
				`trend24h = [${series.join(', ')}]\n`
		);
		expect(series).toHaveLength(24);
		expect(sum).toBe(Number(bounded));
		expect(Number(lower_only) - Number(bounded)).toBe(999);
		expect(Number(future_rows)).toBe(1);
	});

	it(`times listIssues + trendForIssues together over ${RUNS} runs beside listIssues alone`, async () => {
		const both: number[] = [];
		const alone: number[] = [];
		// warm
		{
			const r = await issueQueries.listIssues({ ...filtersA, sort: 'last_seen' }, 1, 20);
			await issueQueries.trendForIssues(r.map((i) => i.id), list.windowStart, list.windowEnd);
		}
		for (let i = 0; i < RUNS; i++) {
			let t0 = process.hrtime.bigint();
			const r = await issueQueries.listIssues({ ...filtersA, sort: 'last_seen' }, 1, 20);
			await issueQueries.trendForIssues(r.map((x) => x.id), list.windowStart, list.windowEnd);
			both.push(ms(t0));

			t0 = process.hrtime.bigint();
			await issueQueries.listIssues({ ...filtersA, sort: 'last_seen' }, 1, 20);
			alone.push(ms(t0));
		}
		const rows: string[][] = [
			['variant (loader side, driver round trips included)', 'runs', 'p50 ms', 'p95 ms', 'max ms'],
			['listIssues + trendForIssues', String(RUNS), fmt(p(both, 0.5)), fmt(p(both, 0.95)), fmt(Math.max(...both))],
			['listIssues alone (no trend query)', String(RUNS), fmt(p(alone, 0.5)), fmt(p(alone, 0.95)), fmt(Math.max(...alone))]
		];
		console.log(
			`\n== M1 SECONDARY (loader side) — issues_list_with_trend_p95_ms\n${table(rows)}\n` +
				`p95 ratio (list + trend) / (list alone) = ${(p(both, 0.95) / p(alone, 0.95)).toFixed(2)} — §13 bound 1.5x, reported, not gated\n`
		);
		expect(both).toHaveLength(RUNS);
	});
});

describe('M3 — the §4.2 loader: trendForIssue on the fully-populated issue (LABELLED FALLBACK)', () => {
	it('daily[i].count = per-UTC-day SELECT sum(count), daily[i].start = window_start + i days, and the +1h row is in neither series', async () => {
		const trend = await issueQueries.trendForIssue(FULLY_POPULATED, detail.windowStart, detail.windowEnd);
		expect(trend.hourly).toHaveLength(24);
		expect(trend.daily).toHaveLength(30);

		const days = await sql<{ i: number; day_start: Date; rows: string; bounded: string; lower_only: string }[]>`
			with days as (
				select i, (${detail.windowStart}::timestamptz + (i * interval '1 day')) as day_start from generate_series(0, 29) as i
			)
			select d.i, d.day_start,
			       (select count(*) from issue_occurrence_buckets b where b.issue_id = ${FULLY_POPULATED}
			          and b.bucket_start >= ${detail.windowStart} and b.bucket_start <= ${detail.windowEnd}
			          and date_trunc('day', b.bucket_start at time zone 'utc') at time zone 'utc' = d.day_start) as rows,
			       coalesce((select sum(b.count) from issue_occurrence_buckets b where b.issue_id = ${FULLY_POPULATED}
			          and b.bucket_start >= ${detail.windowStart} and b.bucket_start <= ${detail.windowEnd}
			          and date_trunc('day', b.bucket_start at time zone 'utc') at time zone 'utc' = d.day_start), 0) as bounded,
			       coalesce((select sum(b.count) from issue_occurrence_buckets b where b.issue_id = ${FULLY_POPULATED}
			          and b.bucket_start >= ${detail.windowStart}
			          and date_trunc('day', b.bucket_start at time zone 'utc') at time zone 'utc' = d.day_start), 0) as lower_only
			from days d order by d.i`;

		const rows: string[][] = [['i', 'daily[i].start', 'expected start (window_start + i d)', 'daily[i].count', 'SQL per-UTC-day sum (bounded)', 'SQL lower-bound only', 'rows that day', 'match']];
		let matches = 0;
		for (let i = 0; i < 30; i++) {
			const expectedStart = iso(new Date(detail.windowStart.getTime() + i * DAY_MS));
			const d = days[i];
			const ok = trend.daily[i].start === expectedStart && trend.daily[i].count === Number(d.bounded);
			if (ok) matches++;
			rows.push([String(i), trend.daily[i].start, expectedStart, String(trend.daily[i].count), d.bounded, d.lower_only, d.rows, ok ? 'yes' : 'NO']);
		}

		const hours = await sql<{ bucket_start: Date; count: number }[]>`
			select bucket_start, count from issue_occurrence_buckets
			where issue_id = ${FULLY_POPULATED} and bucket_start >= ${new Date(detail.windowEnd.getTime() - 23 * HOUR_MS)}
			order by bucket_start`;
		const hourRows: string[][] = [['i', 'hourly[i].start', 'hourly[i].count', 'SQL row count at that hour', 'match']];
		let hourMatches = 0;
		for (let i = 0; i < 24; i++) {
			const start = new Date(detail.windowEnd.getTime() - (23 - i) * HOUR_MS);
			const row = hours.find((h) => h.bucket_start.getTime() === start.getTime());
			const ok = trend.hourly[i].start === iso(start) && trend.hourly[i].count === (row?.count ?? 0);
			if (ok) hourMatches++;
			hourRows.push([String(i), trend.hourly[i].start, String(trend.hourly[i].count), String(row?.count ?? '—'), ok ? 'yes' : 'NO']);
		}
		const futureRow = hours.find((h) => h.bucket_start.getTime() > detail.windowEnd.getTime());

		const sumDaily = trend.daily.reduce((a, b) => a + b.count, 0);
		const sumHourly = trend.hourly.reduce((a, b) => a + b.count, 0);
		const [{ bounded, lower_only }] = await sql<{ bounded: string; lower_only: string }[]>`
			select coalesce(sum(count) filter (where bucket_start <= ${detail.windowEnd}), 0) as bounded,
			       coalesce(sum(count), 0) as lower_only
			from issue_occurrence_buckets where issue_id = ${FULLY_POPULATED} and bucket_start >= ${detail.windowStart}`;

		console.log(
			`\n== M3 PRIMARY (loader side) — /issues/${FULLY_POPULATED}, window [${detail.windowStart.toISOString()} … ${detail.windowEnd.toISOString()}]\n` +
				table(rows) +
				`\n${matches} of 30 days: daily[i].start = window_start + i days and daily[i].count = SQL per-UTC-day sum (bounded)\n\n` +
				table(hourRows) +
				`\n${hourMatches} of 24 hours match\n` +
				`sum(daily) = ${sumDaily}, sum(hourly) = ${sumHourly}; SQL bounded sum = ${bounded}, lower-bound-only = ${lower_only} (difference ${Number(lower_only) - Number(bounded)} = the trunc(now) + 1h row's 999)\n` +
				`the +1h row: ${futureRow ? `${futureRow.bucket_start.toISOString()} count ${futureRow.count}` : 'NOT FOUND'}; in hourly[]? ${trend.hourly.some((h) => futureRow && h.start === iso(futureRow.bucket_start))}; last daily start ${trend.daily[29].start}\n`
		);
		expect(matches).toBe(30);
		expect(hourMatches).toBe(24);
		expect(sumDaily).toBe(Number(bounded));
		expect(Number(lower_only) - Number(bounded)).toBe(999);
		expect(futureRow).toBeDefined();
		expect(trend.hourly.some((h) => h.start === iso(futureRow!.bucket_start))).toBe(false);
		expect(trend.daily.some((d) => d.start > iso(detail.windowEnd))).toBe(false);
	});

	it(`times trendForIssue (query + densify) over ${RUNS} runs`, async () => {
		const full: number[] = [];
		const steady: number[] = [];
		await issueQueries.trendForIssue(FULLY_POPULATED, detail.windowStart, detail.windowEnd);
		for (let i = 0; i < RUNS; i++) {
			let t0 = process.hrtime.bigint();
			await issueQueries.trendForIssue(FULLY_POPULATED, detail.windowStart, detail.windowEnd);
			full.push(ms(t0));
			t0 = process.hrtime.bigint();
			await issueQueries.trendForIssue(STEADY_1, detail.windowStart, detail.windowEnd);
			steady.push(ms(t0));
		}
		const rows: string[][] = [
			['variant (loader side: query + densify into 24 + 30)', 'runs', 'p50 ms', 'p95 ms', 'max ms'],
			['fully populated (n = 401, <= 720 rows) — §13 M3', String(RUNS), fmt(p(full, 0.5)), fmt(p(full, 0.95)), fmt(Math.max(...full))],
			['steady (n = 1) (beside)', String(RUNS), fmt(p(steady, 0.5)), fmt(p(steady, 0.95)), fmt(Math.max(...steady))]
		];
		console.log(`\n== M3 SECONDARY (loader side) — issue_trend_detail_p95_ms\n${table(rows)}\n§13 bound 20 ms, reported, not gated\n`);
		expect(full).toHaveLength(RUNS);
	});
});
