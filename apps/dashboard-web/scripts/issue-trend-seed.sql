-- re-run immediately before every browser or measurement session: every timestamp in this file is
-- now()-relative AT LOAD TIME, and the rows the browser and the measurements look at (the ~50
-- new-in-24h issues, the spike in the last 6 hours, the "new" shape 4 hours ago, the 24-point
-- sparkline, the row at trunc(now) + 1h) age out within hours of loading. A seed loaded yesterday
-- makes `new=24h` return nothing and reads as a false deviation.
--
-- sentinel-issue-trend §13 — the ONE seed behind M1, M3, M4, M5 and fe's browser rows. Committed so
-- every side loads the same data set instead of hand-writing its own; the shape of
-- scripts/affected-users-seed.sql (own organization, deleted first; fixed ids; deterministic).
--
-- What it builds, AFTER both §4.6 migrations (1724700000_add_issue_occurrence_buckets.sql and
-- 1724800000_backfill_issue_occurrence_buckets.sql) have run — this file writes its own bucket
-- rows; the backfill never sees them:
--
--   * one organization, one measuring user who is a member of BOTH projects below (so the
--     issue-list guard's `inArray` branch is in the baseline and M5's `project=all` form has two
--     projects to walk), each project with one declared environment `production`;
--   * project A: 2,000 `system_error` issues in `production` with `count` drawn from a SKEWED
--     distribution — 90 % at 1–20, 9 % at 100–1,000, 1 % at 10,000–100,000; `first_seen` spread
--     over ~60 days, ~50 of them inside the last 24 hours (the `new=24h` set — they are the "new"
--     shape below) and ONE with `first_seen = NULL` (the `NULLS LAST` case); `last_seen` on a
--     60-value grid (six hours × ten days) so `sort=count` ties on the first key are common and
--     ties on the first two keys exist by construction (n and n + 60 share both), which is what
--     makes `id DESC` load-bearing; ~10 issues `regression_status = 'regressed'` with
--     `last_regressed_at` inside the last 24 hours and `first_seen` weeks old (the REGRESSED-badge
--     set, decision 22) and ~10 more with `last_regressed_at` older than 24 hours (no badge);
--   * bucket rows (`issue_occurrence_buckets`) for ~400 of project A's issues over the last 30
--     days in four recognisable shapes, by issue number n:
--       steady   n = 1..120     every hour for 30 days, a diurnal curve (busier 07–18 UTC)
--       spiking  n = 121..240   flat at 1–2 per hour, then 50× in the last 6 hours
--       stopped  n = 241..350   traffic until 3 days ago, then nothing
--       new      n = 351..400   nothing until 4 hours ago (these ARE the new=24h issues)
--     plus n = 401, the FULLY-POPULATED issue: every hourly bucket of the 30-day calendar window
--     (`window_start` = 00:00Z of UTC-today − 29 days, through trunc(now), ≤ 720 rows — the M3
--     worst case) PLUS one row at trunc(now) + 1h with count 999 (decision 18's future-dated
--     case: it must appear in no series and in no sum);
--   * `bucket_start` values before the 30-day cutoff for ~50 issues (stopped n = 241..290 get 48
--     extra hours at 30–32 days ago) — the M4 set;
--   * one `error_occurrences` row per issue at the issue's `last_seen`, so retention's orphan rule
--     ("no occurrences and first_seen < cutoff") does not delete the set on the first sweep;
--   * project B: 200 issues, same count distribution, bucket rows for n = 1..20 (every third
--     hour), so the second project has trend data for the `project=all` forms.
--
-- Everything is DETERMINISTIC (no random()) and NOW()-RELATIVE (no literal calendar date anywhere
-- in this file): two runs in the same hour produce identical counts; two runs an hour apart
-- produce identical counts shifted by an hour. `trunc(now)` below is the current UTC hour's start,
-- spelled `date_trunc('hour', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'` — §3's rule — so the
-- session TimeZone cannot move a bucket. Idempotent by deleting its own organization first.
--
-- Run it against a DISPOSABLE Postgres that has had the dashboard migrations applied — never the
-- shared dev database (CLAUDE.md):
--
--   docker run -d --name sentinel-trend-pg -p 15542:5432 \
--     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
--   cd packages/db-migrations && GOWORK=off \
--     DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15542/sentinel?sslmode=disable \
--     go run ./cmd/migrate up -target dashboard
--   psql postgres://sentinel:changeme@localhost:15542/sentinel \
--     -f apps/dashboard-web/scripts/issue-trend-seed.sql
--
-- Fixed ids:
--   organization      00000000-0000-4000-8000-0000000000b1   (slug issue-trend-seed)
--   project A         00000000-0000-4000-8000-0000000000b2   (2,000 issues)
--   project B         00000000-0000-4000-8000-0000000000b3   (200 issues)
--   user              issue-trend-seed-user                  (member of both, admin)
--   issue ids         00000000-0000-4000-8000-00000b<p><nnnnn>  p = 2 (A) | 3 (B), nnnnn = hex(n)
--     steady #1       00000000-0000-4000-8000-00000b200001   (n = 1)
--     spiking #1      00000000-0000-4000-8000-00000b200079   (n = 121)
--     stopped #1      00000000-0000-4000-8000-00000b2000f1   (n = 241)
--     new #1          00000000-0000-4000-8000-00000b20015f   (n = 351)
--     fully populated 00000000-0000-4000-8000-00000b200191   (n = 401; the trunc(now) + 1h row)
--     null first_seen 00000000-0000-4000-8000-00000b2003e7   (n = 999)
--     regressed <24h  n % 200 = 7    (n = 7, 207, … 1807)
--     regressed >24h  n % 200 = 13   (n = 13, 213, … 1813)

BEGIN;

-- The clock, taken once so every statement below agrees on what "now" is (now() is the
-- transaction's start time anyway; the temp table just names the derived instants).
CREATE TEMP TABLE seed_clock ON COMMIT DROP AS
SELECT now() AS load_time,
       date_trunc('hour', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS trunc_now,
       (date_trunc('day', now() AT TIME ZONE 'UTC') - interval '29 days') AT TIME ZONE 'UTC' AS window_start;

-- Idempotency: remove what a previous run created. issues.project_id has no ON DELETE CASCADE
-- (1716508800_init.sql), so issues and their occurrences go explicitly; the bucket rows cascade
-- from issues (§4.6), members/environments cascade from the project, projects from the org.
DELETE FROM error_occurrences WHERE issue_id IN (
	SELECT id FROM issues WHERE project_id IN (
		'00000000-0000-4000-8000-0000000000b2', '00000000-0000-4000-8000-0000000000b3'
	)
);
DELETE FROM issues WHERE project_id IN (
	'00000000-0000-4000-8000-0000000000b2', '00000000-0000-4000-8000-0000000000b3'
);
DELETE FROM projects WHERE organization_id = '00000000-0000-4000-8000-0000000000b1';
DELETE FROM organizations WHERE id = '00000000-0000-4000-8000-0000000000b1';
DELETE FROM "user" WHERE id = 'issue-trend-seed-user';

INSERT INTO organizations (id, name, slug)
VALUES ('00000000-0000-4000-8000-0000000000b1', 'issue-trend-seed', 'issue-trend-seed');

INSERT INTO projects (id, name, organization_id, api_key, api_key_hash)
VALUES ('00000000-0000-4000-8000-0000000000b2', 'issue-trend-seed-a', '00000000-0000-4000-8000-0000000000b1',
        'pk_issue_trend_seed_a', 'hash_issue_trend_seed_a'),
       ('00000000-0000-4000-8000-0000000000b3', 'issue-trend-seed-b', '00000000-0000-4000-8000-0000000000b1',
        'pk_issue_trend_seed_b', 'hash_issue_trend_seed_b');

-- The measuring user: an org member and a project member of BOTH projects, so `/`'s guard
-- resolves to two project ids and takes the `inArray` branch.
INSERT INTO "user" (id, name, email)
VALUES ('issue-trend-seed-user', 'Issue Trend Seed', 'issue-trend-seed@example.invalid');
INSERT INTO organization_members (organization_id, user_id, role)
VALUES ('00000000-0000-4000-8000-0000000000b1', 'issue-trend-seed-user', 'admin');
INSERT INTO project_members (project_id, user_id, role)
VALUES ('00000000-0000-4000-8000-0000000000b2', 'issue-trend-seed-user', 'admin'),
       ('00000000-0000-4000-8000-0000000000b3', 'issue-trend-seed-user', 'admin');

-- One declared environment per project (sentinel-project-environments decision 8).
INSERT INTO project_environments (project_id, name, display_name, is_default)
VALUES ('00000000-0000-4000-8000-0000000000b2', 'production', 'Production', true),
       ('00000000-0000-4000-8000-0000000000b3', 'production', 'Production', true);

-- Issues. Per project, n runs 1..N and every column is a deterministic function of n:
--
--   count       the first 90 % of n -> 1..20            (1 + (n * 7) % 20)
--               the next   9 % of n -> 100..1,000       (100 + (n * 13) % 901)
--               the last   1 % of n -> 10,000..100,000  (10000 + (n * 977) % 90001)
--   last_seen   trunc(now) − (n % 6) h − ((n / 6) % 10) d: a 60-value grid, so n and n + 60 tie on
--               (count, last_seen) — except the shaped issues, whose last_seen is their last bucket
--               (steady/spiking/fully populated: trunc(now); stopped: trunc(now) − 72 h; new: the
--               load time itself).
--   first_seen  last_seen − (1 + n % 50) d − (n % 24) h: spread over ~60 days, always before
--               last_seen — except the new shape (n = 351..400 in A: trunc(now) − 4 h + (n % 45)
--               min, i.e. inside the last 24 hours), the regressed sets (weeks old: last_seen −
--               (21 + n % 30) d) and n = 999 in A, which is NULL.
--   regression  (project A only)
--               n % 200 = 7  -> 'regressed', last_regressed_at = now() − (1 + n % 20) h   (badge)
--               n % 200 = 13 -> 'regressed', last_regressed_at = now() − (2 + n % 20) d   (no badge)
INSERT INTO issues (id, project_id, environment, fingerprint, message, error_class, issue_type, status,
                    first_seen, last_seen, count, regression_status, regression_count, last_regressed_at)
SELECT ('00000000-0000-4000-8000-00000b' || p.letter || lpad(to_hex(n), 5, '0'))::uuid,
       p.id,
       'production',
       'fp-' || p.letter || '-' || n,
       'context deadline exceeded #' || n,
       'TimeoutError',
       'system_error',
       'unresolved',
       CASE
         WHEN p.letter = '2' AND n = 999 THEN NULL
         WHEN p.letter = '2' AND n BETWEEN 351 AND 400 THEN c.trunc_now - interval '4 hours' + ((n % 45) * interval '1 minute')
         WHEN p.letter = '2' AND n % 200 IN (7, 13) THEN ls.last_seen - ((21 + n % 30) * interval '1 day')
         ELSE ls.last_seen - ((1 + n % 50) * interval '1 day') - ((n % 24) * interval '1 hour')
       END,
       ls.last_seen,
       CASE
         WHEN n <= p.total * 90 / 100 THEN 1 + (n * 7) % 20
         WHEN n <= p.total * 99 / 100 THEN 100 + (n * 13) % 901
         ELSE 10000 + (n * 977) % 90001
       END,
       CASE WHEN p.letter = '2' AND n % 200 IN (7, 13) THEN 'regressed' ELSE 'none' END,
       CASE WHEN p.letter = '2' AND n % 200 IN (7, 13) THEN 1 ELSE 0 END,
       CASE
         WHEN p.letter = '2' AND n % 200 = 7  THEN c.load_time - ((1 + n % 20) * interval '1 hour')
         WHEN p.letter = '2' AND n % 200 = 13 THEN c.load_time - ((2 + n % 20) * interval '1 day')
         ELSE NULL
       END
FROM (VALUES ('00000000-0000-4000-8000-0000000000b2'::uuid, '2', 2000),
             ('00000000-0000-4000-8000-0000000000b3'::uuid, '3', 200)) AS p(id, letter, total)
CROSS JOIN seed_clock c
CROSS JOIN LATERAL generate_series(1, p.total) AS n
CROSS JOIN LATERAL (
	SELECT CASE
		WHEN p.letter = '2' AND n BETWEEN 1 AND 240   THEN c.trunc_now                        -- steady, spiking
		WHEN p.letter = '2' AND n BETWEEN 241 AND 350 THEN c.trunc_now - interval '72 hours'  -- stopped
		WHEN p.letter = '2' AND n BETWEEN 351 AND 400 THEN c.load_time                        -- new
		WHEN p.letter = '2' AND n = 401               THEN c.trunc_now                        -- fully populated
		ELSE c.trunc_now - ((n % 6) * interval '1 hour') - (((n / 6) % 10) * interval '1 day')
	END AS last_seen
) AS ls;

-- One occurrence per issue at its last_seen (see the header for why).
INSERT INTO error_occurrences (issue_id, environment, platform, release_version, created_at)
SELECT i.id, i.environment, 'go', 'v32.0', i.last_seen
FROM issues i
WHERE i.project_id IN ('00000000-0000-4000-8000-0000000000b2', '00000000-0000-4000-8000-0000000000b3');

-- Bucket rows, project A. `k` is hours before trunc(now); bucket_start = trunc(now) − k h is a UTC
-- hour start by construction. Shapes by n (see the header). Counts are > 0 everywhere (the CHECK).
INSERT INTO issue_occurrence_buckets (issue_id, bucket_start, count)
-- steady: every hour of the last 30 days, a diurnal curve keyed on the bucket's UTC hour.
SELECT ('00000000-0000-4000-8000-00000b2' || lpad(to_hex(n), 5, '0'))::uuid,
       c.trunc_now - (k * interval '1 hour'),
       1 + (n % 3)
         + CASE WHEN extract(hour from (c.trunc_now - (k * interval '1 hour')) AT TIME ZONE 'UTC') BETWEEN 7 AND 18
                THEN 2 + ((k + n) % 4) ELSE 0 END
FROM seed_clock c
CROSS JOIN generate_series(1, 120) AS n
CROSS JOIN generate_series(0, 719) AS k
UNION ALL
-- spiking: flat at 1–2 per hour for 30 days, 50× that in the last 6 hours.
SELECT ('00000000-0000-4000-8000-00000b2' || lpad(to_hex(n), 5, '0'))::uuid,
       c.trunc_now - (k * interval '1 hour'),
       (1 + (n % 2)) * CASE WHEN k < 6 THEN 50 ELSE 1 END
FROM seed_clock c
CROSS JOIN generate_series(121, 240) AS n
CROSS JOIN generate_series(0, 719) AS k
UNION ALL
-- stopped: traffic until 3 days ago (k >= 72), then nothing.
SELECT ('00000000-0000-4000-8000-00000b2' || lpad(to_hex(n), 5, '0'))::uuid,
       c.trunc_now - (k * interval '1 hour'),
       1 + (n % 5) + ((k / 24) % 3)
FROM seed_clock c
CROSS JOIN generate_series(241, 350) AS n
CROSS JOIN generate_series(72, 719) AS k
UNION ALL
-- the M4 set: 48 more hours at 30–32 days ago for stopped n = 241..290, i.e. bucket_start before
-- the 30-day cutoff (now − 30 × 24 h), so a sweep at DATA_RETENTION_DAYS=30 has rows to remove.
SELECT ('00000000-0000-4000-8000-00000b2' || lpad(to_hex(n), 5, '0'))::uuid,
       c.trunc_now - (k * interval '1 hour'),
       1 + (n % 5)
FROM seed_clock c
CROSS JOIN generate_series(241, 290) AS n
CROSS JOIN generate_series(721, 768) AS k
UNION ALL
-- new: nothing until 4 hours ago, then four buckets climbing towards the current hour.
SELECT ('00000000-0000-4000-8000-00000b2' || lpad(to_hex(n), 5, '0'))::uuid,
       c.trunc_now - (k * interval '1 hour'),
       5 + (n % 10) + (3 - k) * 2
FROM seed_clock c
CROSS JOIN generate_series(351, 400) AS n
CROSS JOIN generate_series(0, 3) AS k
UNION ALL
-- fully populated (n = 401): every hour from window_start through trunc(now) inclusive.
SELECT '00000000-0000-4000-8000-00000b200191'::uuid,
       h,
       1 + (extract(hour from h AT TIME ZONE 'UTC')::int % 7)
FROM seed_clock c
CROSS JOIN LATERAL generate_series(c.window_start, c.trunc_now, interval '1 hour') AS h
UNION ALL
-- ... plus decision 18's future-dated row at trunc(now) + 1h, which no series and no sum may show.
SELECT '00000000-0000-4000-8000-00000b200191'::uuid,
       c.trunc_now + interval '1 hour',
       999
FROM seed_clock c;

-- Bucket rows, project B: n = 1..20, every third hour over the last 30 days.
INSERT INTO issue_occurrence_buckets (issue_id, bucket_start, count)
SELECT ('00000000-0000-4000-8000-00000b3' || lpad(to_hex(n), 5, '0'))::uuid,
       c.trunc_now - (k * interval '1 hour'),
       1 + ((n + k) % 3)
FROM seed_clock c
CROSS JOIN generate_series(1, 20) AS n
CROSS JOIN generate_series(0, 719, 3) AS k;

COMMIT;

ANALYZE issues;
ANALYZE issue_occurrence_buckets;
ANALYZE error_occurrences;

-- What was built — the numbers "run twice, identical counts" compares.
SELECT 'issues (project A)' AS what, count(*) AS rows
FROM issues WHERE project_id = '00000000-0000-4000-8000-0000000000b2'
UNION ALL
SELECT 'issues (project B)', count(*)
FROM issues WHERE project_id = '00000000-0000-4000-8000-0000000000b3'
UNION ALL
SELECT 'error_occurrences (both)', count(*)
FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id
WHERE i.project_id IN ('00000000-0000-4000-8000-0000000000b2', '00000000-0000-4000-8000-0000000000b3')
UNION ALL
SELECT 'issue_occurrence_buckets (both)', count(*)
FROM issue_occurrence_buckets b JOIN issues i ON i.id = b.issue_id
WHERE i.project_id IN ('00000000-0000-4000-8000-0000000000b2', '00000000-0000-4000-8000-0000000000b3')
UNION ALL
SELECT 'issues with bucket rows (A)', count(DISTINCT b.issue_id)
FROM issue_occurrence_buckets b JOIN issues i ON i.id = b.issue_id
WHERE i.project_id = '00000000-0000-4000-8000-0000000000b2'
UNION ALL
SELECT 'issues with bucket rows (B)', count(DISTINCT b.issue_id)
FROM issue_occurrence_buckets b JOIN issues i ON i.id = b.issue_id
WHERE i.project_id = '00000000-0000-4000-8000-0000000000b3'
UNION ALL
SELECT 'new in 24h (A)', count(*)
FROM issues WHERE project_id = '00000000-0000-4000-8000-0000000000b2' AND first_seen >= now() - interval '24 hours'
UNION ALL
SELECT 'null first_seen (A)', count(*)
FROM issues WHERE project_id = '00000000-0000-4000-8000-0000000000b2' AND first_seen IS NULL
UNION ALL
SELECT 'regressed within 24h (A)', count(*)
FROM issues WHERE project_id = '00000000-0000-4000-8000-0000000000b2'
  AND regression_status = 'regressed' AND last_regressed_at >= now() - interval '24 hours'
UNION ALL
SELECT 'regressed earlier (A)', count(*)
FROM issues WHERE project_id = '00000000-0000-4000-8000-0000000000b2'
  AND regression_status = 'regressed' AND last_regressed_at < now() - interval '24 hours'
UNION ALL
SELECT 'issues with pre-cutoff bucket rows (A)', count(DISTINCT b.issue_id)
FROM issue_occurrence_buckets b JOIN issues i ON i.id = b.issue_id
WHERE i.project_id = '00000000-0000-4000-8000-0000000000b2'
  AND b.bucket_start < now() - interval '30 days'
UNION ALL
SELECT 'fully populated issue: rows in the calendar window', count(*)
FROM issue_occurrence_buckets
WHERE issue_id = '00000000-0000-4000-8000-00000b200191'
  AND bucket_start <= date_trunc('hour', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
UNION ALL
SELECT 'fully populated issue: rows after trunc(now)', count(*)
FROM issue_occurrence_buckets
WHERE issue_id = '00000000-0000-4000-8000-00000b200191'
  AND bucket_start > date_trunc('hour', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
UNION ALL
SELECT 'count ties on (count, last_seen) — groups of 2+ (A)', count(*)
FROM (
	SELECT 1 FROM issues WHERE project_id = '00000000-0000-4000-8000-0000000000b2'
	GROUP BY count, last_seen HAVING count(*) > 1
) t
ORDER BY 1;

-- §13's count distribution, project A: the three bands and their share.
SELECT CASE
         WHEN count <= 20 THEN '1-20'
         WHEN count <= 1000 THEN '100-1000'
         ELSE '10000-100000'
       END AS band,
       count(*) AS issues,
       round(100.0 * count(*) / sum(count(*)) OVER (), 1) AS pct,
       min(count) AS min_count,
       max(count) AS max_count
FROM issues WHERE project_id = '00000000-0000-4000-8000-0000000000b2'
GROUP BY 1 ORDER BY min(count);

-- The four shapes, first issue of each: rows, hours covered, and the last 6 hours' share.
SELECT s.shape,
       count(*) AS rows,
       min(b.bucket_start) AS first_bucket,
       max(b.bucket_start) AS last_bucket,
       sum(b.count) AS total,
       sum(b.count) FILTER (WHERE b.bucket_start > date_trunc('hour', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' - interval '6 hours') AS last_6h
FROM (VALUES ('steady',  '00000000-0000-4000-8000-00000b200001'::uuid),
             ('spiking', '00000000-0000-4000-8000-00000b200079'::uuid),
             ('stopped', '00000000-0000-4000-8000-00000b2000f1'::uuid),
             ('new',     '00000000-0000-4000-8000-00000b20015f'::uuid)) AS s(shape, id)
JOIN issue_occurrence_buckets b ON b.issue_id = s.id
GROUP BY s.shape ORDER BY s.shape;
