-- sentinel-issue-trend §13 M3 — the LABELLED FALLBACK take of the detail's trend query (decision 15).
-- Evidence about PLAN SHAPE on a disposable local Postgres seeded by `issue-trend-seed.sql`, loaded
-- IMMEDIATELY before (its rows are now()-relative); never the criterion met. M3 is "local compose
-- only" in §13 — final as in "never taken on a rig", not as in "met": a report, not a gate.
--
-- What it measures — the §4.2 loader's ONE query for `/issues/{id}` on the seed's FULLY-POPULATED
-- issue (n = 401, `00000000-0000-4000-8000-00000b200191`: every hourly bucket of the 30-day
-- calendar window, ≤ 720 rows, PLUS decision 18's row at trunc(now) + 1h with count 999). The SQL
-- is NOT hand-written: it was captured from the server's `log_statement = 'all'` while
-- `issue-queries.trend.integration.test.ts` drove the real `trendForIssue` (2026-09-13), then the
-- seed's id substituted for the test's:
--
--   select "bucket_start", "count"
--   from "issue_occurrence_buckets"
--   where ("issue_occurrence_buckets"."issue_id" = $1
--      and "issue_occurrence_buckets"."bucket_start" >= $2
--      and "issue_occurrence_buckets"."bucket_start" <= $3)
--   order by "issue_occurrence_buckets"."bucket_start" asc
--
-- `$2` is `window_start` = 00:00:00Z of (UTC today − 29 days), `$3` is `window_end` = trunc(now)
-- (§3, `detailTrendWindow`); both ends bounded (decision 18).
--
-- The densify (24 + 30 points, `daily[i].count` summed IN THE LOADER per UTC day, decision 11) is
-- JavaScript, so the loader's arrays are driven by `issue-trend-densify.measure.ts` against this
-- same database; this file is the SQL side the status pastes beside them — the per-UTC-day sums
-- `trend.daily[i].count` must equal, and the per-hour rows `trend.hourly[i].count` must equal.
--
-- Three sections, in the order §13's M3 row lists its criteria:
--   1. PRIMARY   — the 30 UTC days of the window, i = 0..29: `window_start + i days`, the day's
--                  `sum(count)` over the rows the query returns (`date_trunc('day', bucket_start AT
--                  TIME ZONE 'UTC')` = that day), rows per day (24 for a full day; today's so far),
--                  and the same sum over the UNBOUNDED tail beside it, so the +1h row's 999 shows up
--                  exactly once — in the unbounded column of the LAST day and nowhere else. Then the
--                  24 hours the hourly series covers, and the verdict lines: the +1h row exists, is
--                  > window_end, is in neither series' rows and in neither sum.
--   2. SECONDARY — `EXPLAIN (ANALYZE, BUFFERS)` of the query and one line naming whether it is an
--                  index range scan on `issue_occurrence_buckets_pkey` with NO Sort node (the PK's
--                  second key is `bucket_start`, so the `ORDER BY` is the index order).
--   3. TERTIARY  — p50/p95 over `:runs` executions (default 40, ≥ 30) through `EXECUTE … USING`
--                  (§13's bound is 20 ms; reported, not gated here), beside the same query for the
--                  seed's "steady" issue (n = 1, 720 rows but only 708 in the window) and for an
--                  issue with NO bucket rows (the all-zero case).
--
-- Run against the seeded disposable database (see the seed's header for the container and the
-- migration):
--
--   psql postgres://sentinel:changeme@localhost:15806/sentinel -v ON_ERROR_STOP=1 \
--     -f apps/dashboard-web/scripts/issue-trend-m3.sql
--
-- Overrides: `-v runs=60` for more timing runs. Nothing here writes; the temp table is session-local.

\set QUIET 1
\if :{?runs}
\else
\set runs 40
\endif
\set full_id '00000000-0000-4000-8000-00000b200191'
\set steady_id '00000000-0000-4000-8000-00000b200001'
\set quiet_id '00000000-0000-4000-8000-00000b2003e7'
\set QUIET 0

-- The clock, §3's spelling (`detailTrendWindow`): window_end = trunc(now); window_start = 00:00Z of
-- (UTC today − 29 days); the hourly series is the 24 slots ending at window_end.
select date_trunc('hour', now() at time zone 'utc') at time zone 'utc' as window_end,
       (date_trunc('day', now() at time zone 'utc') - interval '29 days') at time zone 'utc' as window_start,
       (date_trunc('hour', now() at time zone 'utc') - interval '23 hours') at time zone 'utc' as hourly_start
\gset
\echo ''
\echo '== M3 — detail window [' :'window_start' ' … ' :'window_end' '] (both ends inclusive); hourly from ' :'hourly_start' '; ' :runs ' timing runs'

\echo ''
\echo '== M3 §1 PRIMARY — the fully-populated issue (n = 401): per-UTC-day sums the loader''s daily[i] must equal'

-- The query exactly as the loader issues it, kept for the section.
create temp table m3_rows as
select "bucket_start", "count"
from "issue_occurrence_buckets"
where ("issue_occurrence_buckets"."issue_id" = :'full_id'
   and "issue_occurrence_buckets"."bucket_start" >= :'window_start'::timestamptz
   and "issue_occurrence_buckets"."bucket_start" <= :'window_end'::timestamptz)
order by "issue_occurrence_buckets"."bucket_start" asc;

\echo '-- daily[i]: i, start = window_start + i days, rows that day, sum over the query''s rows (bounded), sum over the unbounded tail beside it'
with days as (
	select i, (:'window_start'::timestamptz + (i * interval '1 day')) as day_start
	from generate_series(0, 29) as i
),
unbounded as (
	select bucket_start, count from issue_occurrence_buckets
	where issue_id = :'full_id' and bucket_start >= :'window_start'::timestamptz
)
select d.i,
       d.day_start as "daily[i].start",
       (select count(*) from m3_rows r where date_trunc('day', r.bucket_start at time zone 'utc') at time zone 'utc' = d.day_start) as rows_that_day,
       coalesce((select sum(r.count) from m3_rows r where date_trunc('day', r.bucket_start at time zone 'utc') at time zone 'utc' = d.day_start), 0) as "sum(count) bounded = daily[i].count",
       coalesce((select sum(u.count) from unbounded u where date_trunc('day', u.bucket_start at time zone 'utc') at time zone 'utc' = d.day_start), 0) as sum_lower_bound_only
from days d
order by d.i;

\echo '-- hourly[i]: i, start = window_end − (23 − i) h, the row''s count (0 where no row) — the last 24 of the same rows'
with hours as (
	select i, (:'hourly_start'::timestamptz + (i * interval '1 hour')) as hour_start
	from generate_series(0, 23) as i
)
select h.i, h.hour_start as "hourly[i].start",
       coalesce((select sum(r.count) from m3_rows r where r.bucket_start = h.hour_start), 0) as "count = hourly[i].count"
from hours h order by h.i;

\echo '-- verdict lines'
select 'rows the query returned (<= 720; 29 full days x 24 + today''s hours so far + 1)' as check,
       (select count(*) from m3_rows)::text as value
union all
select 'expected rows for this clock: 29 * 24 + (hours into today UTC) + 1',
       (29 * 24 + extract(hour from :'window_end'::timestamptz at time zone 'utc')::int + 1)::text
union all
select 'the trunc(now) + 1h row exists on this issue (1 = the seed built it)',
       (select count(*) from issue_occurrence_buckets where issue_id = :'full_id' and bucket_start = :'window_end'::timestamptz + interval '1 hour' and count = 999)::text
union all
select 'rows the query returned with bucket_start > window_end (0 = it is in neither series)',
       (select count(*) from m3_rows where bucket_start > :'window_end'::timestamptz)::text
union all
select 'sum over the query''s rows (what daily[] sums to)',
       (select sum(count) from m3_rows)::text
union all
select 'sum over the lower-bound-only tail (differs by exactly 999 = the +1h row is in neither sum)',
       (select sum(count) from issue_occurrence_buckets where issue_id = :'full_id' and bucket_start >= :'window_start'::timestamptz)::text
union all
select 'the day boundary: rows at exactly 00:00Z of a window day (30 = every day''s first hour is counted on that day)',
       (select count(*) from m3_rows where bucket_start = date_trunc('day', bucket_start at time zone 'utc') at time zone 'utc')::text
union all
select 'rows at 23:00Z (29 = every full day''s last hour; today''s depends on the clock)',
       (select count(*) from m3_rows where extract(hour from bucket_start at time zone 'utc') = 23)::text
union all
select 'distinct UTC days among the rows (30 = every daily[i] has data)',
       (select count(distinct date_trunc('day', bucket_start at time zone 'utc')) from m3_rows)::text;

\echo ''
\echo '== M3 §2 SECONDARY — EXPLAIN (ANALYZE, BUFFERS) of the detail query on the fully-populated issue'
explain (analyze, buffers)
select "bucket_start", "count"
from "issue_occurrence_buckets"
where ("issue_occurrence_buckets"."issue_id" = :'full_id'
   and "issue_occurrence_buckets"."bucket_start" >= :'window_start'::timestamptz
   and "issue_occurrence_buckets"."bucket_start" <= :'window_end'::timestamptz)
order by "issue_occurrence_buckets"."bucket_start" asc;

\echo '-- the one line: index range scan on the primary key, and no Sort node?'
select set_config('m3.full_id', :'full_id', false) as _ \gset
select set_config('m3.window_start', :'window_start', false) as _ \gset
select set_config('m3.window_end', :'window_end', false) as _ \gset
do $$
declare
	line text;
	plan text := '';
begin
	for line in execute format($q$
		explain (format text)
		select "bucket_start", "count"
		from "issue_occurrence_buckets"
		where ("issue_occurrence_buckets"."issue_id" = %L
		   and "issue_occurrence_buckets"."bucket_start" >= %L::timestamptz
		   and "issue_occurrence_buckets"."bucket_start" <= %L::timestamptz)
		order by "issue_occurrence_buckets"."bucket_start" asc
	$q$, current_setting('m3.full_id'), current_setting('m3.window_start'), current_setting('m3.window_end')) loop
		plan := plan || line || E'\n';
	end loop;
	if plan like '%Seq Scan on issue_occurrence_buckets%' then
		raise notice 'VERDICT: the detail query is a SEQ SCAN on issue_occurrence_buckets — paste the plan';
	elsif (plan like '%Index Scan using issue_occurrence_buckets_pkey%' or plan like '%Index Only Scan using issue_occurrence_buckets_pkey%') and plan not like '%Sort%' then
		raise notice 'VERDICT: the detail query is an index range scan on issue_occurrence_buckets_pkey with no Sort node — the ORDER BY is the index order';
	elsif plan like '%issue_occurrence_buckets_pkey%' then
		raise notice 'VERDICT: issue_occurrence_buckets_pkey serves the detail query but the plan carries a Sort or a bitmap — paste the plan';
	else
		raise notice 'VERDICT: the detail query uses neither issue_occurrence_buckets_pkey nor a seq scan — paste the plan';
	end if;
end $$;

\echo ''
\echo '== M3 §3 TERTIARY — p50/p95 over :runs runs of the detail query (fully populated), beside steady (n = 1) and an issue with no rows'

create temp table if not exists m3_timing (variant text, run int, ms double precision);
truncate m3_timing;
select set_config('m3.runs', :'runs', false) as runs;
select set_config('m3.steady_id', :'steady_id', false) as _ \gset
select set_config('m3.quiet_id', :'quiet_id', false) as _ \gset

do $$
declare
	runs int := current_setting('m3.runs')::int;
	detail_sql constant text := $q$
		select "bucket_start", "count"
		from "issue_occurrence_buckets"
		where ("issue_occurrence_buckets"."issue_id" = $1
		   and "issue_occurrence_buckets"."bucket_start" >= $2
		   and "issue_occurrence_buckets"."bucket_start" <= $3)
		order by "issue_occurrence_buckets"."bucket_start" asc
	$q$;
	full_id constant uuid := current_setting('m3.full_id')::uuid;
	steady_id constant uuid := current_setting('m3.steady_id')::uuid;
	quiet_id constant uuid := current_setting('m3.quiet_id')::uuid;
	ws constant timestamptz := current_setting('m3.window_start')::timestamptz;
	we constant timestamptz := current_setting('m3.window_end')::timestamptz;
	t0 timestamptz;
	i int;
begin
	execute detail_sql using full_id, ws, we;
	execute detail_sql using steady_id, ws, we;
	execute detail_sql using quiet_id, ws, we;
	for i in 1..runs loop
		t0 := clock_timestamp();
		execute detail_sql using full_id, ws, we;
		insert into m3_timing values ('fully populated (n = 401, <= 720 rows) — §13 M3', i, extract(epoch from clock_timestamp() - t0) * 1000);

		t0 := clock_timestamp();
		execute detail_sql using steady_id, ws, we;
		insert into m3_timing values ('steady (n = 1) (beside)', i, extract(epoch from clock_timestamp() - t0) * 1000);

		t0 := clock_timestamp();
		execute detail_sql using quiet_id, ws, we;
		insert into m3_timing values ('no bucket rows (n = 999) (beside)', i, extract(epoch from clock_timestamp() - t0) * 1000);
	end loop;
end $$;

with per_variant as (
	select variant,
		count(*) as runs,
		round(percentile_cont(0.5) within group (order by ms)::numeric, 3) as p50_ms,
		round(percentile_cont(0.95) within group (order by ms)::numeric, 3) as p95_ms,
		round(max(ms)::numeric, 3) as max_ms
	from m3_timing group by variant
)
select * from per_variant order by variant;

select round(percentile_cont(0.95) within group (order by ms)::numeric, 3) as "p95 ms, fully populated; §13 bound 20 ms, not gated"
from m3_timing where variant like 'fully populated%';

drop table m3_timing;
drop table m3_rows;
