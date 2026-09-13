-- sentinel-affected-users §13 M1 — the LABELLED FALLBACK take of the sort's plan and timings
-- (decision 20). This is evidence about PLAN SHAPE on a disposable local Postgres seeded by
-- `affected-users-seed.sql`; it is never the criterion met. The criterion's venue is
-- `sentinel-staging-db`, taken by `tasks/TASK-01.md` after the `v*-staging` deploy has applied §4.6.
--
-- What it measures — the §4.1 loader's `listIssues` query for
-- `?project=<A>&status=all&sort=affected_users&page=1..3`, guards applied, exactly as the
-- application emits it. The SQL below is NOT hand-written: it was captured from the server's own
-- `log_statement = 'all'` output while `issue-queries.sort.integration.test.ts` drove the real
-- `listIssues` (2026-09-13), then the seed's ids were substituted for the test's:
--
--   select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status",
--          "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count"
--   from "issues"
--   where ("issues"."project_id" = $1 and "issues"."project_id" in ($2, $3))
--   order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
--   limit $4 offset $5
--
-- `$1` is the `project=<A>` filter, `in ($2, $3)` is the tenancy predicate over the measuring
-- user's two projects (`getAccessibleProjects`), `status=all` adds no predicate, and the seed user
-- has no `project_environment_grants` row so `environmentScopeCondition` adds none either.
--
-- Three sections, in the order §13's M1 row lists its criteria:
--   1. PRIMARY   — pages 1–3 (20 rows each) with `sort=affected_users`, then a verdict: the 60 ids
--                  are in `affected_user_count DESC, last_seen DESC, id DESC`, and they are exactly
--                  the first 60 of the whole order — no repeated id, no skipped id.
--   2. SECONDARY — `EXPLAIN (ANALYZE, BUFFERS)` of page 1 and page 3, and one line naming which
--                  index serves each form: the request as specified (environment=all) should be
--                  `idx_issues_project_affected_users` (1724600000, BUG-01 — no `environment` key,
--                  so the ORDER BY is the index order with only `project_id` pinned); the
--                  environment-filtered variant should be `idx_issues_project_env_affected_users`
--                  (1724500000, §4.6).
--   3. TERTIARY  — p50/p95 over `:runs` executions (default 40, ≥ 30) of page 1 under
--                  `sort=affected_users` beside the same request under `sort=last_seen`, and the
--                  ratio. Each execution goes through `EXECUTE … USING` so it is planned with
--                  parameters the way the driver sends them, not with literals.
--
-- Run against the seeded disposable database (see the seed's header for the container and the
-- migration):
--
--   psql postgres://sentinel:changeme@localhost:15537/sentinel -v ON_ERROR_STOP=1 \
--     -f apps/dashboard-web/scripts/affected-users-m1.sql
--
-- Overrides: `-v runs=60` for more timing runs. Nothing here writes; the temp table is session-local.

\set QUIET 1
\if :{?runs}
\else
\set runs 40
\endif
\set QUIET 0

\echo ''
\echo '== M1 §1 PRIMARY — pages 1–3 of sort=affected_users (project A, status=all)'

\echo '-- page 1'
select "id", "affected_user_count", "last_seen"
from "issues"
where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000a2'
   and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3'))
order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
limit 20;

\echo '-- page 2'
select "id", "affected_user_count", "last_seen"
from "issues"
where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000a2'
   and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3'))
order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
limit 20 offset 20;

\echo '-- page 3'
select "id", "affected_user_count", "last_seen"
from "issues"
where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000a2'
   and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3'))
order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
limit 20 offset 40;

\echo '-- verdict: pages 1-3, concatenated, versus rows 1-60 of the whole order'
\set first_page 1
with page as (
	-- the three page reads, each exactly as the loader issues it, tagged with page and position
	select p.page, row_number() over (partition by p.page) as pos, r.id, r.affected_user_count, r.last_seen
	from (values (:first_page, (:first_page - 1) * 20), (:first_page + 1, :first_page * 20), (:first_page + 2, (:first_page + 1) * 20)) as p(page, off)
	cross join lateral (
		select "id", "affected_user_count", "last_seen"
		from "issues"
		where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000a2'
		   and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3'))
		order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
		limit 20 offset p.off
	) r
),
paged as (
	select id, affected_user_count, last_seen, (page - 1) * 20 + pos as seq from page
),
whole as (
	select "id", row_number() over (
		order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
	) as seq
	from "issues"
	where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000a2'
	   and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3'))
),
ordered as (
	select
		count(*) filter (where prev_count is not null and (
			affected_user_count > prev_count
			or (affected_user_count = prev_count and last_seen > prev_seen)
			or (affected_user_count = prev_count and last_seen = prev_seen and id > prev_id)
		)) as out_of_order
	from (
		select id, affected_user_count, last_seen,
			lag(affected_user_count) over (order by seq) as prev_count,
			lag(last_seen) over (order by seq) as prev_seen,
			lag(id) over (order by seq) as prev_id
		from paged
	) s
)
select 'rows across the three pages'                                    as check, (select count(*) from paged)::text as value
union all
select 'distinct ids across the three pages (60 = no repeated id)',     (select count(distinct id) from paged)::text
union all
select 'ids outside the whole order''s window for these pages',         (select count(*) from paged p where not exists (select 1 from whole w where w.id = p.id and w.seq between (:first_page - 1) * 20 + 1 and (:first_page + 2) * 20))::text
union all
select 'window ids missing from the pages (0 = no skipped id)',         (select count(*) from whole w where w.seq between (:first_page - 1) * 20 + 1 and (:first_page + 2) * 20 and not exists (select 1 from paged p where p.id = w.id))::text
union all
select 'rows at the wrong position versus the whole order',             (select count(*) from paged p join whole w on w.id = p.id where w.seq <> p.seq)::text
union all
select 'adjacent pairs violating count DESC, last_seen DESC, id DESC',  (select out_of_order from ordered)::text
union all
select 'tie groups on (count, last_seen) inside the 60 (id DESC load-bearing)', (select count(*) from (select 1 from paged group by affected_user_count, last_seen having count(*) > 1) t)::text;


-- The seed's ties on (affected_user_count, last_seen) — the reason id DESC exists (decision 22) —
-- all sit in the 0–3 band, below the 200 distinct-valued issues of the 10–100 and 500–5,000 bands,
-- so pages 1–3 contain none of them (the last verdict line above says how many). The third key
-- is load-bearing on the first pages that DO contain a tie group; find them and run the same
-- verdict there, beside the criterion's pages, so the tiebreak is exercised by data and not only
-- by the query text.
\echo '-- beside: the first three pages that contain a (count, last_seen) tie group'
with whole as (
	select id, affected_user_count, last_seen, row_number() over (
		order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
	) as seq
	from "issues"
	where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000a2'
	   and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3'))
)
select ceil(min(w.seq) / 20.0)::int as first_page
from whole w
where exists (select 1 from whole x where x.affected_user_count = w.affected_user_count and x.last_seen = w.last_seen and x.id <> w.id)
\gset
\echo '   first tie group is on page' :first_page
with page as (
	-- the three page reads, each exactly as the loader issues it, tagged with page and position
	select p.page, row_number() over (partition by p.page) as pos, r.id, r.affected_user_count, r.last_seen
	from (values (:first_page, (:first_page - 1) * 20), (:first_page + 1, :first_page * 20), (:first_page + 2, (:first_page + 1) * 20)) as p(page, off)
	cross join lateral (
		select "id", "affected_user_count", "last_seen"
		from "issues"
		where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000a2'
		   and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3'))
		order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
		limit 20 offset p.off
	) r
),
paged as (
	select id, affected_user_count, last_seen, (page - 1) * 20 + pos as seq from page
),
whole as (
	select "id", row_number() over (
		order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
	) as seq
	from "issues"
	where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000a2'
	   and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3'))
),
ordered as (
	select
		count(*) filter (where prev_count is not null and (
			affected_user_count > prev_count
			or (affected_user_count = prev_count and last_seen > prev_seen)
			or (affected_user_count = prev_count and last_seen = prev_seen and id > prev_id)
		)) as out_of_order
	from (
		select id, affected_user_count, last_seen,
			lag(affected_user_count) over (order by seq) as prev_count,
			lag(last_seen) over (order by seq) as prev_seen,
			lag(id) over (order by seq) as prev_id
		from paged
	) s
)
select 'rows across the three pages'                                    as check, (select count(*) from paged)::text as value
union all
select 'distinct ids across the three pages (60 = no repeated id)',     (select count(distinct id) from paged)::text
union all
select 'ids outside the whole order''s window for these pages',         (select count(*) from paged p where not exists (select 1 from whole w where w.id = p.id and w.seq between (:first_page - 1) * 20 + 1 and (:first_page + 2) * 20))::text
union all
select 'window ids missing from the pages (0 = no skipped id)',         (select count(*) from whole w where w.seq between (:first_page - 1) * 20 + 1 and (:first_page + 2) * 20 and not exists (select 1 from paged p where p.id = w.id))::text
union all
select 'rows at the wrong position versus the whole order',             (select count(*) from paged p join whole w on w.id = p.id where w.seq <> p.seq)::text
union all
select 'adjacent pairs violating count DESC, last_seen DESC, id DESC',  (select out_of_order from ordered)::text
union all
select 'tie groups on (count, last_seen) inside the 60 (id DESC load-bearing)', (select count(*) from (select 1 from paged group by affected_user_count, last_seen having count(*) > 1) t)::text;

\echo ''
\echo '== M1 §2 SECONDARY — EXPLAIN (ANALYZE, BUFFERS), page 1 then page 3'

\echo '-- page 1'
explain (analyze, buffers)
select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status", "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count"
from "issues"
where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000a2'
   and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3'))
order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
limit 20;

\echo '-- page 3'
explain (analyze, buffers)
select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status", "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count"
from "issues"
where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000a2'
   and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3'))
order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
limit 20 offset 40;

\echo '-- the same request with the environment FILTER (?environment=production, flow L8) — the index'
\echo '-- has environment as its second key, so this is the shape under which its key order can serve the ORDER BY'
explain (analyze, buffers)
select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status", "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count"
from "issues"
where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000a2'
   and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3')
   and "issues"."environment" = 'production')
order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
limit 20;

\echo '-- the one line §13 asks for (page 1, the request as specified; and the environment-filtered variant beside it)'
do $$
declare
	line text;
	plan text := '';
	plan_env text := '';
begin
	for line in execute $q$
		explain (format text)
		select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status", "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count"
		from "issues"
		where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000a2'
		   and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3'))
		order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
		limit 20
	$q$ loop
		plan := plan || line || E'\n';
	end loop;
	for line in execute $q$
		explain (format text)
		select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status", "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count"
		from "issues"
		where ("issues"."project_id" = '00000000-0000-4000-8000-0000000000a2'
		   and "issues"."project_id" in ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3')
		   and "issues"."environment" = 'production')
		order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
		limit 20
	$q$ loop
		plan_env := plan_env || line || E'\n';
	end loop;
	if plan like '%idx_issues_project_affected_users%' then
		raise notice 'VERDICT: idx_issues_project_affected_users IS used by the page-1 plan (project=<A>, status=all, sort=affected_users) — BUG-01 resolved';
	elsif plan like '%idx_issues_project_env_affected_users%' then
		raise notice 'VERDICT: idx_issues_project_env_affected_users is used by the page-1 plan (project=<A>, status=all, sort=affected_users) — unexpected without an environment equality; paste the plan';
	else
		raise notice 'VERDICT: NEITHER affected_users index is used by the page-1 plan (project=<A>, status=all, sort=affected_users) — BUG-01''s shape; is 1724600000 applied?';
	end if;
	if plan_env like '%idx_issues_project_env_affected_users%' then
		raise notice 'BESIDE: with environment=production added, idx_issues_project_env_affected_users IS used';
	elsif plan_env like '%idx_issues_project_affected_users%' then
		raise notice 'BESIDE: with environment=production added, idx_issues_project_affected_users is used (environment as a row filter, not an index key)';
	else
		raise notice 'BESIDE: with environment=production added, neither index is used';
	end if;
end $$;

\echo ''
\echo '== M1 §3 TERTIARY — p50/p95 over :runs runs, sort=affected_users beside sort=last_seen (page 1)'

create temp table if not exists m1_timing (variant text, run int, ms double precision);
truncate m1_timing;
-- psql variables are not expanded inside a dollar-quoted DO body; hand :runs over as a GUC
select set_config('m1.runs', :'runs', false) as runs;

do $$
declare
	runs int := current_setting('m1.runs')::int;
	affected_sql constant text := $q$
		select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status", "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count"
		from "issues"
		where ("issues"."project_id" = $1 and "issues"."project_id" in ($2, $3))
		order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
		limit $4
	$q$;
	last_seen_sql constant text := $q$
		select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status", "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count"
		from "issues"
		where ("issues"."project_id" = $1 and "issues"."project_id" in ($2, $3))
		order by "issues"."last_seen" desc
		limit $4
	$q$;
	affected_env_sql constant text := $q$
		select "id", "project_id", "fingerprint", "message", "error_class", "environment", "status", "first_seen", "last_seen", "count", "affected_user_count", "affected_tenant_count"
		from "issues"
		where ("issues"."project_id" = $1 and "issues"."project_id" in ($2, $3) and "issues"."environment" = $5)
		order by "issues"."affected_user_count" desc, "issues"."last_seen" desc, "issues"."id" desc
		limit $4
	$q$;
	a constant uuid := '00000000-0000-4000-8000-0000000000a2';
	b constant uuid := '00000000-0000-4000-8000-0000000000a3';
	t0 timestamptz;
	i int;
begin
	-- warm both once so the first run's relcache miss is not in either sample
	execute affected_sql using a, a, b, 20;
	execute last_seen_sql using a, a, b, 20;
	execute affected_env_sql using a, a, b, 20, 'production';
	for i in 1..runs loop
		t0 := clock_timestamp();
		execute affected_sql using a, a, b, 20;
		insert into m1_timing values ('sort=affected_users', i, extract(epoch from clock_timestamp() - t0) * 1000);

		t0 := clock_timestamp();
		execute last_seen_sql using a, a, b, 20;
		insert into m1_timing values ('sort=last_seen', i, extract(epoch from clock_timestamp() - t0) * 1000);

		-- beside, not §13's request: the same sort with the environment FILTER, the shape the index serves
		t0 := clock_timestamp();
		execute affected_env_sql using a, a, b, 20, 'production';
		insert into m1_timing values ('sort=affected_users&environment=production (beside)', i, extract(epoch from clock_timestamp() - t0) * 1000);
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

select round((a.p95 / nullif(b.p95, 0))::numeric, 2) as "p95 ratio affected_users / last_seen (bound 2.0x)"
from (select percentile_cont(0.95) within group (order by ms) as p95 from m1_timing where variant = 'sort=affected_users') a,
     (select percentile_cont(0.95) within group (order by ms) as p95 from m1_timing where variant = 'sort=last_seen') b;

select round((a.p95 / nullif(b.p95, 0))::numeric, 2) as "p95 ratio (beside) env=production / last_seen"
from (select percentile_cont(0.95) within group (order by ms) as p95 from m1_timing where variant like 'sort=affected_users&environment=production%') a,
     (select percentile_cont(0.95) within group (order by ms) as p95 from m1_timing where variant = 'sort=last_seen') b;

drop table m1_timing;
