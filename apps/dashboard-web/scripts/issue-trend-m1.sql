-- sentinel-issue-trend §13 M1 — the LABELLED FALLBACK take of the list's two queries (decision 15).
-- This is evidence about PLAN SHAPE on a disposable local Postgres seeded by `issue-trend-seed.sql`,
-- loaded IMMEDIATELY before (its rows are now()-relative); it is never the criterion met. The
-- criterion's venue is `sentinel-staging-db`, via the held task, once that rig exists.
--
-- What it measures — the §4.1 loader's TWO queries for `?project=<A>&status=all&page=1` (default
-- `sort=last_seen`), guards applied, exactly as the application emits them. Neither statement is
-- hand-written: both were captured from the server's own `log_statement = 'all'` while
-- `issue-queries.trend.integration.test.ts` drove the real `listIssues` and `trendForIssues`
-- (2026-09-13), then the seed's ids were substituted for the test's:
--
--   select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status",
--          "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count",
--          "regression_status", "last_regressed_at"
--   from "issues"
--   where ("issues"."project_id" = $1 and "issues"."project_id" in ($2, $3))
--   order by "issues"."last_seen" desc
--   limit $4 offset $5
--
--   select "issue_id", "bucket_start", "count"
--   from "issue_occurrence_buckets"
--   where ("issue_occurrence_buckets"."issue_id" in ($1, …, $20)
--      and "issue_occurrence_buckets"."bucket_start" >= $21
--      and "issue_occurrence_buckets"."bucket_start" <= $22)
--
-- `$1` is the `project=<A>` filter, `in ($2, $3)` the tenancy predicate over the measuring user's
-- two projects (`getAccessibleProjects`), `status=all` adds no predicate, and the seed user has no
-- `project_environment_grants` row so `environmentScopeCondition` adds none. The bucket query's
-- ids are EXACTLY the 20 ids the first query returned (the loader zips them), and its window is
-- §3's `[trunc(now) − 23h … trunc(now)]`, bounded at BOTH ends (decision 18).
--
-- The densify itself (`trend24h` being 24 long and summing to these rows) is JavaScript, not SQL,
-- so it is driven by `issue-trend-densify.measure.ts` against this same database; this file is
-- the SQL side the status pastes beside it: the per-id sums the loader's arrays must equal.
--
-- Three sections, in the order §13's M1 row lists its criteria:
--   1. PRIMARY   — page 1's 20 ids, and for each: rows in the bounded window, `sum(count)` over
--                  them (what `trend24h` must sum to), and `sum(count)` over the UNBOUNDED tail
--                  (`bucket_start >= window_start` alone) beside it — the two differ exactly where
--                  a future-dated row exists (the seed's trunc(now) + 1h row on issue n = 401).
--   2. SECONDARY — `EXPLAIN (ANALYZE, BUFFERS)` of the bucket query for those 20 ids, and one line
--                  naming whether it is an index range scan on `issue_occurrence_buckets_pkey`
--                  (§13: "not a seq scan"). The `listIssues` plan is pasted beside it.
--   3. TERTIARY  — p50/p95 over `:runs` executions (default 40, ≥ 30) of `listIssues` + the bucket
--                  query timed TOGETHER, beside `listIssues` alone, and the ratio (§13's bound is
--                  1.5×; reported, not gated here). Each execution goes through `EXECUTE … USING`
--                  so it is planned with parameters the way the driver sends them; the id list is
--                  passed as `= any($1::uuid[])`, which is the form PostgreSQL rewrites
--                  `in ($1, …, $20)` to (ScalarArrayOp) — the §2 plan, taken with the literal
--                  in-list, shows the same node.
--
-- Run against the seeded disposable database (see the seed's header for the container and the
-- migration):
--
--   psql postgres://sentinel:changeme@localhost:15806/sentinel -v ON_ERROR_STOP=1 \
--     -f apps/dashboard-web/scripts/issue-trend-m1.sql
--
-- Overrides: `-v runs=60` for more timing runs. Nothing here writes; the temp table is session-local.

\set QUIET 1
\if :{?runs}
\else
\set runs 40
\endif
\set QUIET 0

-- The clock, §3's spelling: trunc(now) is the current UTC hour's start; the list window is the 24
-- hourly slots ending there.
select date_trunc('hour', now() at time zone 'utc') at time zone 'utc' as window_end,
       (date_trunc('hour', now() at time zone 'utc') - interval '23 hours') at time zone 'utc' as window_start
\gset
\echo ''
\echo '== M1 — list window [' :'window_start' ' … ' :'window_end' '] (both ends inclusive), ' :runs ' timing runs'

\echo ''
\echo '== M1 §1 PRIMARY — page 1 of ?project=<A>&status=all (sort=last_seen), and the bounded sum per id'

-- Page 1 exactly as the loader issues it. The default order has no tiebreak (§4.1: `last_seen DESC`
-- and nothing else, deliberately), so WHICH 20 of the seed's 50 "new" issues come back is up to
-- the executor; the check below is made on whatever came back, as the loader's is.
create temp table m1_page1 as
select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status", "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count", "regression_status", "last_regressed_at"
from "issues"
where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000b2'
   and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000b2', '00000000-0000-4000-8000-0000000000b3'))
order by "issues"."last_seen" desc
limit 20 offset 0;

-- The 20 ids as a literal in-list, for the EXPLAIN below (the spelling Drizzle's inArray emits).
select string_agg(quote_literal(id), ', ' order by last_seen desc, id) as page_ids from m1_page1
\gset

\echo '-- the 20 ids, and per id: rows and sum(count) in the BOUNDED window (what trend24h must sum to),'
\echo '-- beside the sum with only the lower bound (what an unbounded reader would show)'
select p.id,
       p.last_seen,
       count(b.bucket_start) filter (where b.bucket_start <= :'window_end'::timestamptz)                  as rows_in_window,
       coalesce(sum(b.count)  filter (where b.bucket_start <= :'window_end'::timestamptz), 0)             as sum_bounded,
       coalesce(sum(b.count), 0)                                                                          as sum_lower_bound_only,
       count(b.bucket_start) filter (where b.bucket_start > :'window_end'::timestamptz)                   as rows_after_window_end
from m1_page1 p
left join issue_occurrence_buckets b
       on b.issue_id = p.id and b.bucket_start >= :'window_start'::timestamptz
group by p.id, p.last_seen
order by p.last_seen desc, p.id;

\echo '-- verdict: the 20 ids'' bounded sums versus the bucket query AS EMITTED (in-list + both bounds), summed per id'
with emitted as (
	select "issue_id", "bucket_start", "count"
	from "issue_occurrence_buckets"
	where ("issue_occurrence_buckets"."issue_id" in (:page_ids)
	   and "issue_occurrence_buckets"."bucket_start" >= :'window_start'::timestamptz
	   and "issue_occurrence_buckets"."bucket_start" <= :'window_end'::timestamptz)
),
per_id as (
	select p.id,
	       coalesce((select sum(e.count) from emitted e where e.issue_id = p.id), 0) as sum_emitted,
	       coalesce((select sum(b.count) from issue_occurrence_buckets b
	                  where b.issue_id = p.id
	                    and b.bucket_start >= :'window_start'::timestamptz
	                    and b.bucket_start <= :'window_end'::timestamptz), 0) as sum_bounded,
	       (select count(distinct e.bucket_start) from emitted e where e.issue_id = p.id) as distinct_slots
	from m1_page1 p
)
select 'ids on page 1'                                                        as check, count(*)::text as value from per_id
union all
select 'ids whose emitted-query sum = bounded SELECT sum(count) (20 = all)',   count(*) filter (where sum_emitted = sum_bounded)::text from per_id
union all
select 'ids with more than 24 distinct bucket_start values in the window (0 = densify cannot overflow)', count(*) filter (where distinct_slots > 24)::text from per_id
union all
select 'rows the emitted query returned for these 20 ids',                     (select count(*) from emitted)::text
union all
select 'rows the emitted query returned with bucket_start > window_end (0 = upper bound holds)', (select count(*) from emitted where bucket_start > :'window_end'::timestamptz)::text;

\echo ''
\echo '== M1 §2 SECONDARY — EXPLAIN (ANALYZE, BUFFERS): the bucket query for page 1''s 20 ids, then listIssues page 1'

\echo '-- trendForIssues(ids, window_start, window_end), ids = page 1'
explain (analyze, buffers)
select "issue_id", "bucket_start", "count"
from "issue_occurrence_buckets"
where ("issue_occurrence_buckets"."issue_id" in (:page_ids)
   and "issue_occurrence_buckets"."bucket_start" >= :'window_start'::timestamptz
   and "issue_occurrence_buckets"."bucket_start" <= :'window_end'::timestamptz);

\echo '-- listIssues page 1 (project=<A>, status=all, sort=last_seen)'
explain (analyze, buffers)
select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status", "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count", "regression_status", "last_regressed_at"
from "issues"
where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000b2'
   and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000b2', '00000000-0000-4000-8000-0000000000b3'))
order by "issues"."last_seen" desc
limit 20 offset 0;

\echo '-- the one line §13 asks for: is the bucket query an index range scan on the primary key?'
-- psql variables are not expanded inside a dollar-quoted DO body; hand them over as GUCs (quietly)
select set_config('m1.page_ids', :'page_ids', false) as _ \gset
select set_config('m1.window_start', :'window_start', false) as _ \gset
select set_config('m1.window_end', :'window_end', false) as _ \gset
do $$
declare
	line text;
	plan text := '';
begin
	for line in execute format($q$
		explain (format text)
		select "issue_id", "bucket_start", "count"
		from "issue_occurrence_buckets"
		where ("issue_occurrence_buckets"."issue_id" in (%s)
		   and "issue_occurrence_buckets"."bucket_start" >= %L::timestamptz
		   and "issue_occurrence_buckets"."bucket_start" <= %L::timestamptz)
	$q$, current_setting('m1.page_ids'), current_setting('m1.window_start'), current_setting('m1.window_end')) loop
		plan := plan || line || E'\n';
	end loop;
	if plan like '%Seq Scan on issue_occurrence_buckets%' then
		raise notice 'VERDICT: the bucket query is a SEQ SCAN on issue_occurrence_buckets — §13 M1 tertiary''s named failure; paste the plan';
	elsif plan like '%Index Scan using issue_occurrence_buckets_pkey%' or plan like '%Index Only Scan using issue_occurrence_buckets_pkey%' then
		raise notice 'VERDICT: the bucket query is an index range scan on issue_occurrence_buckets_pkey (issue_id = ANY(...), bucket_start between the bounds) — the shape §13 names';
	elsif plan like '%Bitmap Index Scan on issue_occurrence_buckets_pkey%' then
		raise notice 'VERDICT: the bucket query is a BITMAP scan driven by issue_occurrence_buckets_pkey — the primary key serves it, as a bitmap rather than a plain range scan; paste the plan';
	else
		raise notice 'VERDICT: the bucket query uses neither issue_occurrence_buckets_pkey nor a seq scan — paste the plan';
	end if;
end $$;

\echo ''
\echo '== M1 §3 TERTIARY — p50/p95 over :runs runs: listIssues + trendForIssues together, beside listIssues alone (page 1)'

create temp table if not exists m1_timing (variant text, run int, ms double precision);
truncate m1_timing;
select set_config('m1.runs', :'runs', false) as runs;

do $$
declare
	runs int := current_setting('m1.runs')::int;
	list_sql constant text := $q$
		select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status", "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count", "regression_status", "last_regressed_at"
		from "issues"
		where ("issues"."project_id" = $1 and "issues"."project_id" in ($2, $3))
		order by "issues"."last_seen" desc
		limit $4 offset $5
	$q$;
	trend_sql constant text := $q$
		select "issue_id", "bucket_start", "count"
		from "issue_occurrence_buckets"
		where ("issue_occurrence_buckets"."issue_id" = any($1::uuid[])
		   and "issue_occurrence_buckets"."bucket_start" >= $2
		   and "issue_occurrence_buckets"."bucket_start" <= $3)
	$q$;
	a constant uuid := '00000000-0000-4000-8000-0000000000b2';
	b constant uuid := '00000000-0000-4000-8000-0000000000b3';
	ws constant timestamptz := current_setting('m1.window_start')::timestamptz;
	we constant timestamptz := current_setting('m1.window_end')::timestamptz;
	ids uuid[];
	t0 timestamptz;
	i int;
begin
	-- warm both once so the first run's relcache miss is not in either sample
	execute 'select array_agg(id) from (' || list_sql || ') p' into ids using a, a, b, 20, 0;
	execute trend_sql using ids, ws, we;
	for i in 1..runs loop
		-- the loader's sequence: the page read, then the bucket read over the ids it returned
		t0 := clock_timestamp();
		execute 'select array_agg(id) from (' || list_sql || ') p' into ids using a, a, b, 20, 0;
		execute trend_sql using ids, ws, we;
		insert into m1_timing values ('listIssues + trendForIssues', i, extract(epoch from clock_timestamp() - t0) * 1000);

		t0 := clock_timestamp();
		execute list_sql using a, a, b, 20, 0;
		insert into m1_timing values ('listIssues alone (no trend query)', i, extract(epoch from clock_timestamp() - t0) * 1000);

		-- beside: the bucket query by itself, so the two-query figure can be decomposed
		t0 := clock_timestamp();
		execute trend_sql using ids, ws, we;
		insert into m1_timing values ('trendForIssues alone (beside)', i, extract(epoch from clock_timestamp() - t0) * 1000);
	end loop;
end $$;

with per_variant as (
	select variant,
		count(*) as runs,
		round(percentile_cont(0.5) within group (order by ms)::numeric, 3) as p50_ms,
		round(percentile_cont(0.95) within group (order by ms)::numeric, 3) as p95_ms,
		round(max(ms)::numeric, 3) as max_ms
	from m1_timing group by variant
)
select * from per_variant order by variant;

select round((a.p95 / nullif(b.p95, 0))::numeric, 2) as "p95 ratio (list+trend)/(list alone); §13 bound 1.5x, not gated"
from (select percentile_cont(0.95) within group (order by ms) as p95 from m1_timing where variant = 'listIssues + trendForIssues') a,
     (select percentile_cont(0.95) within group (order by ms) as p95 from m1_timing where variant like 'listIssues alone%') b;

drop table m1_timing;
drop table m1_page1;
