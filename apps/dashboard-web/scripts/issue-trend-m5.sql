-- sentinel-issue-trend §13 M5 — the LABELLED FALLBACK take of the two new sorts (decision 15).
-- Evidence about PLAN SHAPE on a disposable local Postgres seeded by `issue-trend-seed.sql`, loaded
-- IMMEDIATELY before (its rows are now()-relative); never the criterion met. The criterion's venue
-- is `sentinel-staging-db`, via the held task, once that rig exists. The with/without plans here
-- are the named evidence for the follow-up item that may strike `idx_issues_project_count`
-- (decision 21); nothing at PLANNING waits on them.
--
-- What it measures — the §4.1 loader's `listIssues` for FOUR requests, guards applied, exactly as
-- the application emits them: `sort=count` and `sort=first_seen`, each for `project=<A>` and for the
-- landing form `project=all` (the measuring user is a member of BOTH seeded projects, so the guard
-- is `project_id IN (A, B)`). The SQL is NOT hand-written: captured from the server's
-- `log_statement = 'all'` while `issue-queries.trend.integration.test.ts` drove the real
-- `listIssues` (2026-09-13), then the seed's ids substituted for the test's:
--
--   select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status",
--          "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count",
--          "regression_status", "last_regressed_at"
--   from "issues"
--   where ("issues"."project_id" = $1 and "issues"."project_id" in ($2, $3))      -- project=<A>
--   where "issues"."project_id" in ($1, $2)                                          -- project=all
--   order by "issues"."count" desc, "issues"."last_seen" desc, "issues"."id" desc    -- sort=count
--   order by "issues"."first_seen" DESC NULLS LAST, "issues"."id" desc               -- sort=first_seen
--   limit $n offset $m
--
-- `status=all` adds no predicate and the seed user has no `project_environment_grants` row so
-- `environmentScopeCondition` adds none.
--
-- Three sections, in the order §13's M5 row lists its criteria:
--   1. PRIMARY   — for each of the four requests, pages 1–3 (20 rows each) and a verdict: the 60
--                  ids are in the §4.1 order, they are exactly rows 1–60 of the whole order (no
--                  repeated id, no skipped id), and — under `sort=first_seen` — the seed's
--                  null-`first_seen` issue (n = 999) is the LAST row of the whole order, for both
--                  project forms. Page 1 of each is printed; pages 2–3 are summarised.
--   2. SECONDARY — `EXPLAIN (ANALYZE, BUFFERS)` of page 1 for all four requests WITH §4.6's two
--                  `issues` indexes, then the two indexes are DROPPED, the four plans taken again
--                  WITHOUT them, and the indexes RECREATED with the migration's own DDL — all in
--                  this one session, and a catalog check at the end proves both are back. One line
--                  per request names the index (or the Sort) serving it. The `project=all` forms are
--                  EXPECTED not to walk a `(project_id, …)` btree in order (§4.6, the BUG-01
--                  structural gap) — reported, not gated.
--   3. TERTIARY  — p50/p95 over `:runs` executions (default 40, ≥ 30) through `EXECUTE … USING` of
--                  all four requests beside `sort=last_seen` for both project forms, taken TWICE:
--                  with the indexes and without them (inside the drop/recreate window of §2), and
--                  the ratios `sort=count / sort=last_seen` and `sort=first_seen / sort=last_seen`
--                  for the `project=<A>` forms with the indexes in place (§13's bound is 2.0×;
--                  reported, not gated here).
--
-- Run against the seeded disposable database ONLY — §2 drops and recreates two indexes on
-- `issues` (see the seed's header for the container and the migration):
--
--   psql postgres://sentinel:changeme@localhost:15806/sentinel -v ON_ERROR_STOP=1 \
--     -f apps/dashboard-web/scripts/issue-trend-m5.sql
--
-- Overrides: `-v runs=60` for more timing runs. Apart from the index drop/recreate nothing here
-- writes; the temp objects are session-local.

\set QUIET 1
\if :{?runs}
\else
\set runs 40
\endif
\set a '00000000-0000-4000-8000-0000000000b2'
\set b '00000000-0000-4000-8000-0000000000b3'
\set null_first_seen_id '00000000-0000-4000-8000-00000b2003e7'
\set QUIET 0

-- The four requests (and the two sort=last_seen besides) as (form, sort, where, order) rows, so
-- every section below reads them from ONE place. The text is the captured statement's, the seed's
-- ids in place of the parameters.
create temp table m5_requests (seq int, form text, sort text, where_sql text, order_sql text);
insert into m5_requests values
(1, 'project=<A>', 'count',
 $w$("issues"."project_id" = '00000000-0000-4000-8000-0000000000b2' and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000b2', '00000000-0000-4000-8000-0000000000b3'))$w$,
 $o$"issues"."count" desc, "issues"."last_seen" desc, "issues"."id" desc$o$),
(2, 'project=<A>', 'first_seen',
 $w$("issues"."project_id" = '00000000-0000-4000-8000-0000000000b2' and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000b2', '00000000-0000-4000-8000-0000000000b3'))$w$,
 $o$"issues"."first_seen" DESC NULLS LAST, "issues"."id" desc$o$),
(3, 'project=all', 'count',
 $w$"issues"."project_id" in ('00000000-0000-4000-8000-0000000000b2', '00000000-0000-4000-8000-0000000000b3')$w$,
 $o$"issues"."count" desc, "issues"."last_seen" desc, "issues"."id" desc$o$),
(4, 'project=all', 'first_seen',
 $w$"issues"."project_id" in ('00000000-0000-4000-8000-0000000000b2', '00000000-0000-4000-8000-0000000000b3')$w$,
 $o$"issues"."first_seen" DESC NULLS LAST, "issues"."id" desc$o$),
(5, 'project=<A>', 'last_seen (beside)',
 $w$("issues"."project_id" = '00000000-0000-4000-8000-0000000000b2' and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000b2', '00000000-0000-4000-8000-0000000000b3'))$w$,
 $o$"issues"."last_seen" desc$o$),
(6, 'project=all', 'last_seen (beside)',
 $w$"issues"."project_id" in ('00000000-0000-4000-8000-0000000000b2', '00000000-0000-4000-8000-0000000000b3')$w$,
 $o$"issues"."last_seen" desc$o$);

create temp table m5_timing (state text, form text, sort text, run int, ms double precision);
create temp table m5_plans (state text, seq int, form text, sort text, plan text);
create temp table m5_paged (seq int, id uuid, "count" bigint, first_seen timestamptz, last_seen timestamptz);
create temp table m5_wholeorder (seq bigint, id uuid, "count" bigint, first_seen timestamptz, last_seen timestamptz);

-- Pages 1–3 of one request, exactly as the loader issues them (limit 20 offset (page − 1) * 20),
-- tagged with page and position; and the whole order with row_number(), for the verdict.
create or replace function pg_temp.m5_pages(p_where text, p_order text)
returns table (page int, pos int, id uuid, "count" bigint, first_seen timestamptz, last_seen timestamptz)
language plpgsql as $$
declare
	p int;
begin
	for p in 1..3 loop
		return query execute format($q$
			select %s as page, (row_number() over ())::int as pos, r.id, r.count, r.first_seen, r.last_seen
			from (
				select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status", "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count", "regression_status", "last_regressed_at"
				from "issues"
				where %s
				order by %s
				limit 20 offset %s
			) r
		$q$, p, p_where, p_order, (p - 1) * 20);
	end loop;
end $$;

create or replace function pg_temp.m5_whole(p_where text, p_order text)
returns table (seq bigint, id uuid, "count" bigint, first_seen timestamptz, last_seen timestamptz)
language plpgsql as $$
begin
	return query execute format($q$
		select row_number() over (order by %s) as seq, "id", "count", "first_seen", "last_seen"
		from "issues" where %s
	$q$, p_order, p_where);
end $$;

-- The verdict over one request: the concatenated pages against rows 1–60 of the whole order.
create or replace function pg_temp.m5_verdict(p_form text, p_sort text, p_where text, p_order text, p_null_id uuid)
returns table ("check" text, value text)
language plpgsql as $$
declare
	total bigint;
	null_seq bigint;
begin
	truncate m5_paged; truncate m5_wholeorder;
	insert into m5_paged select (page - 1) * 20 + pos, id, count, first_seen, last_seen from pg_temp.m5_pages(p_where, p_order);
	insert into m5_wholeorder select * from pg_temp.m5_whole(p_where, p_order);
	select count(*) into total from m5_wholeorder;
	select w.seq into null_seq from m5_wholeorder w where w.id = p_null_id;

	return query
	select 'rows across the three pages', (select count(*) from m5_paged)::text
	union all
	select 'distinct ids across the three pages (60 = no repeated id)', (select count(distinct id) from m5_paged)::text
	union all
	select 'ids outside rows 1-60 of the whole order (0 = nothing foreign)', (select count(*) from m5_paged p where not exists (select 1 from m5_wholeorder w where w.id = p.id and w.seq between 1 and 60))::text
	union all
	select 'rows 1-60 of the whole order missing from the pages (0 = no skipped id)', (select count(*) from m5_wholeorder w where w.seq between 1 and 60 and not exists (select 1 from m5_paged p where p.id = w.id))::text
	union all
	select 'rows at the wrong position versus the whole order (0 = the pages ARE rows 1-60 in order)', (select count(*) from m5_paged p join m5_wholeorder w on w.id = p.id where w.seq <> p.seq)::text
	union all
	select 'adjacent pairs violating the ORDER BY inside the 60', (
		select count(*) from (
			select id, count, first_seen, last_seen,
				lag(count) over (order by seq) as pc, lag(first_seen) over (order by seq) as pf,
				lag(last_seen) over (order by seq) as pl, lag(id) over (order by seq) as pi
			from m5_paged) s
		where pi is not null and (
			case when p_sort like 'count%' then
				count > pc or (count = pc and last_seen > pl) or (count = pc and last_seen = pl and id > pi)
			when p_sort like 'first_seen%' then
				(first_seen is not null and pf is null)
				or (first_seen is not null and pf is not null and first_seen > pf)
				or (first_seen is not distinct from pf and id > pi)
			else last_seen > pl end))::text
	union all
	select 'tie groups on the leading key(s) inside the 60 (the unique final key is load-bearing where > 0)', (
		case when p_sort like 'count%' then (select count(*) from (select 1 from m5_paged group by count, last_seen having count(*) > 1) t)
		     when p_sort like 'first_seen%' then (select count(*) from (select 1 from m5_paged group by first_seen having count(*) > 1) t)
		     else 0 end)::text
	union all
	select 'rows in the whole order for this form', total::text
	union all
	select 'position of the null-first_seen issue (n = 999) in the whole order (LAST = the row count above)',
	       case when p_sort like 'first_seen%' then coalesce(null_seq::text, 'absent') || case when null_seq = total then ' = LAST' else ' — NOT last' end
	            else 'n/a (only sort=first_seen orders on first_seen)' end;
end $$;

\echo ''
\echo '== M5 — four requests, pages 1–3, plans with and without idx_issues_project_count / idx_issues_project_first_seen, ' :runs ' timing runs'

\echo ''
\echo '== M5 §1 PRIMARY — the §4.1 order across pages 1–3, no repeated or skipped id, null first_seen last'

\echo ''
\echo '-- [1] project=<A> sort=count — page 1'
select * from pg_temp.m5_pages((select where_sql from m5_requests where seq = 1), (select order_sql from m5_requests where seq = 1)) where page = 1;
\echo '-- [1] pages 2-3, first and last row'
with p as (select * from pg_temp.m5_pages((select where_sql from m5_requests where seq = 1), (select order_sql from m5_requests where seq = 1)))
select page, min(pos) as first_pos, max(pos) as last_pos, (array_agg(id order by pos))[1] as first_id, (array_agg(id order by pos))[count(*)] as last_id, (array_agg(count order by pos))[1] as first_count, (array_agg(count order by pos))[count(*)] as last_count
from p where page > 1 group by page order by page;
\echo '-- [1] verdict'
select * from pg_temp.m5_verdict('project=<A>', 'count', (select where_sql from m5_requests where seq = 1), (select order_sql from m5_requests where seq = 1), :'null_first_seen_id');

\echo ''
\echo '-- [2] project=<A> sort=first_seen — page 1'
select * from pg_temp.m5_pages((select where_sql from m5_requests where seq = 2), (select order_sql from m5_requests where seq = 2)) where page = 1;
\echo '-- [2] pages 2-3, first and last row'
with p as (select * from pg_temp.m5_pages((select where_sql from m5_requests where seq = 2), (select order_sql from m5_requests where seq = 2)))
select page, min(pos) as first_pos, max(pos) as last_pos, (array_agg(id order by pos))[1] as first_id, (array_agg(id order by pos))[count(*)] as last_id, (array_agg(first_seen order by pos))[1] as first_first_seen, (array_agg(first_seen order by pos))[count(*)] as last_first_seen
from p where page > 1 group by page order by page;
\echo '-- [2] verdict'
select * from pg_temp.m5_verdict('project=<A>', 'first_seen', (select where_sql from m5_requests where seq = 2), (select order_sql from m5_requests where seq = 2), :'null_first_seen_id');

\echo ''
\echo '-- [3] project=all sort=count — page 1'
select * from pg_temp.m5_pages((select where_sql from m5_requests where seq = 3), (select order_sql from m5_requests where seq = 3)) where page = 1;
\echo '-- [3] pages 2-3, first and last row'
with p as (select * from pg_temp.m5_pages((select where_sql from m5_requests where seq = 3), (select order_sql from m5_requests where seq = 3)))
select page, min(pos) as first_pos, max(pos) as last_pos, (array_agg(id order by pos))[1] as first_id, (array_agg(id order by pos))[count(*)] as last_id, (array_agg(count order by pos))[1] as first_count, (array_agg(count order by pos))[count(*)] as last_count
from p where page > 1 group by page order by page;
\echo '-- [3] verdict'
select * from pg_temp.m5_verdict('project=all', 'count', (select where_sql from m5_requests where seq = 3), (select order_sql from m5_requests where seq = 3), :'null_first_seen_id');

\echo ''
\echo '-- [4] project=all sort=first_seen — page 1'
select * from pg_temp.m5_pages((select where_sql from m5_requests where seq = 4), (select order_sql from m5_requests where seq = 4)) where page = 1;
\echo '-- [4] pages 2-3, first and last row'
with p as (select * from pg_temp.m5_pages((select where_sql from m5_requests where seq = 4), (select order_sql from m5_requests where seq = 4)))
select page, min(pos) as first_pos, max(pos) as last_pos, (array_agg(id order by pos))[1] as first_id, (array_agg(id order by pos))[count(*)] as last_id, (array_agg(first_seen order by pos))[1] as first_first_seen, (array_agg(first_seen order by pos))[count(*)] as last_first_seen
from p where page > 1 group by page order by page;
\echo '-- [4] verdict'
select * from pg_temp.m5_verdict('project=all', 'first_seen', (select where_sql from m5_requests where seq = 4), (select order_sql from m5_requests where seq = 4), :'null_first_seen_id');

-- The seed's ties on (count, last_seen) are by construction (n and n + 60 share both) but sit in
-- the 1–20 band, below the 200 distinct-valued issues of the upper bands, so pages 1–3 of
-- sort=count may contain none. The third key is load-bearing on the first pages that DO contain a
-- tie group; find them for the project=<A> form and run the same verdict there, beside.
\echo ''
\echo '-- beside [1]: the first three pages of project=<A> sort=count that contain a (count, last_seen) tie group'
with whole as (select * from pg_temp.m5_whole((select where_sql from m5_requests where seq = 1), (select order_sql from m5_requests where seq = 1)))
select ceil(min(w.seq) / 20.0)::int as tie_page
from whole w
where exists (select 1 from whole x where x.count = w.count and x.last_seen = w.last_seen and x.id <> w.id)
\gset
\echo '   first tie group is on page' :tie_page
with paged as (
	select (p.page - 1) * 20 + (row_number() over (partition by p.page order by r.pos))::int as seq, r.id, r.count, r.last_seen
	from (values (:tie_page), (:tie_page + 1), (:tie_page + 2)) as p(page)
	cross join lateral (
		select (row_number() over ())::int as pos, "id", "count", "last_seen"
		from (
			select "id", "count", "last_seen" from "issues"
			where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000b2' and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000b2', '00000000-0000-4000-8000-0000000000b3'))
			order by "issues"."count" desc, "issues"."last_seen" desc, "issues"."id" desc
			limit 20 offset (p.page - 1) * 20
		) q
	) r
),
whole as (select * from pg_temp.m5_whole((select where_sql from m5_requests where seq = 1), (select order_sql from m5_requests where seq = 1)))
select 'rows across the three pages' as check, (select count(*) from paged)::text as value
union all
select 'distinct ids (60 = no repeated id)', (select count(distinct id) from paged)::text
union all
select 'rows at the wrong position versus the whole order (0 = in order, no skip)', (select count(*) from paged p join whole w on w.id = p.id where w.seq <> p.seq)::text
union all
select 'tie groups on (count, last_seen) inside these 60 (> 0 = id DESC exercised)', (select count(*) from (select 1 from paged group by count, last_seen having count(*) > 1) t)::text;

\echo ''
\echo '== M5 §2 SECONDARY — EXPLAIN (ANALYZE, BUFFERS), page 1 of all four requests, WITH the two §4.6 indexes'
\echo '-- indexes present before:'
select indexname from pg_indexes where tablename = 'issues' and indexname in ('idx_issues_project_count', 'idx_issues_project_first_seen') order by 1;

-- One function takes all six plans (four requests + two last_seen besides) for a state and stores
-- the text; the section prints them afterwards and the one-line verdicts read them.
create or replace function pg_temp.m5_take_plans(p_state text) returns void language plpgsql as $$
declare
	r record;
	line text;
	plan text;
begin
	for r in select * from m5_requests order by seq loop
		plan := '';
		for line in execute format($q$
			explain (analyze, buffers)
			select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status", "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count", "regression_status", "last_regressed_at"
			from "issues" where %s order by %s limit 20 offset 0
		$q$, r.where_sql, r.order_sql) loop
			plan := plan || line || E'\n';
		end loop;
		insert into m5_plans values (p_state, r.seq, r.form, r.sort, plan);
	end loop;
end $$;

create or replace function pg_temp.m5_time(p_state text, p_runs int) returns void language plpgsql as $$
declare
	r record;
	stmt text;
	t0 timestamptz;
	i int;
begin
	for r in select * from m5_requests order by seq loop
		stmt := format($q$
			select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status", "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count", "regression_status", "last_regressed_at"
			from "issues" where %s order by %s limit $1 offset $2
		$q$, r.where_sql, r.order_sql);
		execute stmt using 20, 0; -- warm
		for i in 1..p_runs loop
			t0 := clock_timestamp();
			execute stmt using 20, 0;
			insert into m5_timing values (p_state, r.form, r.sort, i, extract(epoch from clock_timestamp() - t0) * 1000);
		end loop;
	end loop;
end $$;

select pg_temp.m5_take_plans('with indexes') as _ \gset
select set_config('m5.runs', :'runs', false) as _ \gset
select pg_temp.m5_time('with indexes', current_setting('m5.runs')::int) as _ \gset

\echo '-- [1] project=<A> sort=count — with indexes'
select plan from m5_plans where state = 'with indexes' and seq = 1;
\echo '-- [2] project=<A> sort=first_seen — with indexes'
select plan from m5_plans where state = 'with indexes' and seq = 2;
\echo '-- [3] project=all sort=count — with indexes'
select plan from m5_plans where state = 'with indexes' and seq = 3;
\echo '-- [4] project=all sort=first_seen — with indexes'
select plan from m5_plans where state = 'with indexes' and seq = 4;

\echo ''
\echo '-- DROP the two §4.6 indexes (this session, this disposable database only)'
set lock_timeout = '5s';
drop index if exists idx_issues_project_first_seen;
drop index if exists idx_issues_project_count;
\echo '-- indexes present during the WITHOUT take (expected: none):'
select indexname from pg_indexes where tablename = 'issues' and indexname in ('idx_issues_project_count', 'idx_issues_project_first_seen') order by 1;

select pg_temp.m5_take_plans('without indexes') as _ \gset
select pg_temp.m5_time('without indexes', current_setting('m5.runs')::int) as _ \gset

\echo '-- [1] project=<A> sort=count — WITHOUT indexes'
select plan from m5_plans where state = 'without indexes' and seq = 1;
\echo '-- [2] project=<A> sort=first_seen — WITHOUT indexes'
select plan from m5_plans where state = 'without indexes' and seq = 2;
\echo '-- [3] project=all sort=count — WITHOUT indexes'
select plan from m5_plans where state = 'without indexes' and seq = 3;
\echo '-- [4] project=all sort=first_seen — WITHOUT indexes'
select plan from m5_plans where state = 'without indexes' and seq = 4;

\echo ''
\echo '-- RECREATE the two indexes with the migration''s own DDL (1724700000_add_issue_occurrence_buckets.sql)'
CREATE INDEX IF NOT EXISTS idx_issues_project_count
    ON issues (project_id, count DESC, last_seen DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_issues_project_first_seen
    ON issues (project_id, first_seen DESC NULLS LAST, id DESC);
reset lock_timeout;
\echo '-- indexes present after (expected: both):'
select indexname, indexdef from pg_indexes where tablename = 'issues' and indexname in ('idx_issues_project_count', 'idx_issues_project_first_seen') order by 1;

\echo ''
\echo '-- the one line per request: what serves it, with and without'
select seq, form, sort, state,
       case
         when plan like '%Index Scan using idx_issues_project_count%' or plan like '%Index Scan Backward using idx_issues_project_count%' then 'idx_issues_project_count (index order, no Sort)'
         when plan like '%Index Scan using idx_issues_project_first_seen%' or plan like '%Index Scan Backward using idx_issues_project_first_seen%' then 'idx_issues_project_first_seen (index order, no Sort)'
         when plan like '%Sort%' and plan like '%idx_issues_project_id%' then 'top-N Sort over idx_issues_project_id'
         when plan like '%Sort%' and plan like '%Seq Scan on issues%' then 'top-N Sort over a Seq Scan'
         when plan like '%Sort%' then 'top-N Sort over: ' || coalesce(substring(plan from 'using ([a-z_]+) on issues'), 'another index')
         when plan like '%idx_issues_last_seen%' then 'idx_issues_last_seen (index order, no Sort)'
         else 'see plan'
       end as serves_it,
       substring(plan from 'Execution Time: ([0-9.]+ ms)') as execution_time
from m5_plans order by seq, state desc;

\echo ''
\echo '== M5 §3 TERTIARY — p50/p95 over :runs runs, all six statements, with and without the indexes'
select state, form, sort,
	count(*) as runs,
	round(percentile_cont(0.5) within group (order by ms)::numeric, 3) as p50_ms,
	round(percentile_cont(0.95) within group (order by ms)::numeric, 3) as p95_ms,
	round(max(ms)::numeric, 3) as max_ms
from m5_timing group by state, form, sort order by form, sort, state desc;

\echo '-- §13''s ratios: project=<A> forms with the indexes, against sort=last_seen (bound 2.0x, reported not gated)'
with p as (
	select sort, percentile_cont(0.95) within group (order by ms) as p95
	from m5_timing where state = 'with indexes' and form = 'project=<A>' group by sort
)
select c.sort || ' / last_seen' as ratio_of, round((c.p95 / nullif(l.p95, 0))::numeric, 2) as p95_ratio
from p c, p l where l.sort like 'last_seen%' and c.sort not like 'last_seen%' order by 1;

\echo '-- beside: the same ratios WITHOUT the indexes, and the project=all forms both ways'
with p as (
	select state, form, sort, percentile_cont(0.95) within group (order by ms) as p95
	from m5_timing group by state, form, sort
)
select c.state, c.form, c.sort || ' / last_seen' as ratio_of, round((c.p95 / nullif(l.p95, 0))::numeric, 2) as p95_ratio
from p c join p l on l.state = c.state and l.form = c.form and l.sort like 'last_seen%'
where c.sort not like 'last_seen%' and not (c.state = 'with indexes' and c.form = 'project=<A>')
order by c.form, c.state desc, c.sort;

drop table m5_timing;
drop table m5_plans;
drop table m5_requests;
drop table m5_paged;
drop table m5_wholeorder;
