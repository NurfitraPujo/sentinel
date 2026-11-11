-- sentinel-affected-users §13 / decision 25 — the ONE seed behind M1 (the sort's plan), M3 (the
-- sweep's plan) and fe's browser rows. Committed so every side loads the same data set instead of
-- hand-writing its own.
--
-- What it builds, after the §4.6 migration (1724500000_add_issue_affected_rollup.sql):
--
--   * one organization, one measuring user who is a member of BOTH projects below (so the
--     issue-list guard's `inArray` branch is in the baseline), each project with one declared
--     environment `production`;
--   * project A: 2,000 `system_error` issues in `production` with `affected_user_count` drawn from
--     a SKEWED distribution — 90 % at 0–3, 9 % at 10–100, 1 % at 500–5,000 — the skew is the
--     load-bearing part: a uniform column sorts trivially and measures nothing;
--   * project B: 200 issues, same shape, smaller;
--   * `issue_affected_users` / `issue_affected_tenants` rows that make each issue's counters
--     EXACTLY the cardinality of its side rows (the §4.6 invariant, checked at the end of this
--     file), with `last_seen` spread evenly over the last 60 days so half the side rows fall
--     outside a 30-day cutoff (M3);
--   * one `error_occurrences` row per issue at the issue's `last_seen`, so retention's orphan rule
--     ("no occurrences and first_seen < cutoff") does not delete the whole set on the first sweep —
--     an issue survives the sweep exactly when its newest occurrence is inside the window, which is
--     what §4.5's recompute path needs to have something to recompute.
--
-- Everything is DETERMINISTIC (no random()): two runs produce identical counts and identical
-- distributions, which is what makes "run twice, same numbers" checkable. Fixed ids throughout;
-- idempotent by deleting its own organization first (the projects, memberships, environments,
-- issues, occurrences and side rows cascade or are deleted explicitly below).
--
-- Run it against a DISPOSABLE Postgres that has had the dashboard migrations applied — never the
-- shared dev database (CLAUDE.md):
--
--   docker run -d --name sentinel-affected-pg -p 15537:5432 \
--     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
--   cd packages/db-migrations && GOWORK=off \
--     DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15537/sentinel?sslmode=disable \
--     go run ./cmd/migrate up -target dashboard
--   psql postgres://sentinel:changeme@localhost:15537/sentinel \
--     -f apps/dashboard-web/scripts/affected-users-seed.sql
--
-- Fixed ids:
--   organization  00000000-0000-4000-8000-0000000000a1   (slug affected-users-seed)
--   project A     00000000-0000-4000-8000-0000000000a2   (2,000 issues)
--   project B     00000000-0000-4000-8000-0000000000a3   (200 issues)
--   user          affected-users-seed-user               (member of both, admin)
--   issue ids     00000000-0000-4000-8000-00000a<pppp>   deterministic per (project, n); see below

BEGIN;

-- Idempotency: remove what a previous run created. issues.project_id has no ON DELETE CASCADE
-- (1716508800_init.sql), so issues and their occurrences go explicitly; the side tables cascade
-- from issues (§4.6), members/environments cascade from the project, projects from the org.
DELETE FROM error_occurrences WHERE issue_id IN (
	SELECT id FROM issues WHERE project_id IN (
		'00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3'
	)
);
DELETE FROM issues WHERE project_id IN (
	'00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3'
);
DELETE FROM projects WHERE organization_id = '00000000-0000-4000-8000-0000000000a1';
DELETE FROM organizations WHERE id = '00000000-0000-4000-8000-0000000000a1';
DELETE FROM "user" WHERE id = 'affected-users-seed-user';

INSERT INTO organizations (id, name, slug)
VALUES ('00000000-0000-4000-8000-0000000000a1', 'affected-users-seed', 'affected-users-seed');

INSERT INTO projects (id, name, organization_id, api_key, api_key_hash)
VALUES ('00000000-0000-4000-8000-0000000000a2', 'affected-users-seed-a', '00000000-0000-4000-8000-0000000000a1',
        'pk_affected_users_seed_a', 'hash_affected_users_seed_a'),
       ('00000000-0000-4000-8000-0000000000a3', 'affected-users-seed-b', '00000000-0000-4000-8000-0000000000a1',
        'pk_affected_users_seed_b', 'hash_affected_users_seed_b');

-- The measuring user: an org member and a project member of BOTH projects, so `/`'s guard
-- resolves to two project ids and takes the `inArray` branch.
INSERT INTO "user" (id, name, email)
VALUES ('affected-users-seed-user', 'Affected Users Seed', 'affected-users-seed@example.invalid');
INSERT INTO organization_members (organization_id, user_id, role)
VALUES ('00000000-0000-4000-8000-0000000000a1', 'affected-users-seed-user', 'admin');
INSERT INTO project_members (project_id, user_id, role)
VALUES ('00000000-0000-4000-8000-0000000000a2', 'affected-users-seed-user', 'admin'),
       ('00000000-0000-4000-8000-0000000000a3', 'affected-users-seed-user', 'admin');

-- One declared environment per project (sentinel-project-environments decision 8: a registry row
-- exists because a person created it — this file is that person).
INSERT INTO project_environments (project_id, name, display_name, is_default)
VALUES ('00000000-0000-4000-8000-0000000000a2', 'production', 'Production', true),
       ('00000000-0000-4000-8000-0000000000a3', 'production', 'Production', true);

-- Issues. Per project, n runs 1..N and the target counts are a deterministic function of n:
--
--   the first 90 % of n      -> affected_user_count in 0..3        ((n * 7) % 4)
--   the next   9 % of n      -> 10..100                            (10 + (n * 13) % 91)
--   the last   1 % of n      -> 500..5,000                         (500 + (n * 977) % 4501)
--
-- affected_tenant_count is smaller and never exceeds the user count: 0 when users are 0,
-- otherwise 1 + (users - 1) / 10 (so 1..3 users -> 1 tenant, 100 users -> 10, 5,000 -> 500).
--
-- last_seen is spread over 60 days by (n % 60) with an hour offset by (n % 24): 1,800 issues at
-- 0..3 users across 1,440 distinct timestamps GUARANTEES ties on the first two sort keys
-- (affected_user_count, last_seen), which is what makes `id DESC` load-bearing (decision 22, M1).
-- Deterministic ids: the last 8 hex digits are the project letter and n, so a row can be named.
INSERT INTO issues (id, project_id, environment, fingerprint, message, error_class, issue_type, status,
                    first_seen, last_seen, count, affected_user_count, affected_tenant_count)
SELECT ('00000000-0000-4000-8000-00000a' || p.letter || lpad(to_hex(n), 5, '0'))::uuid,
       p.id,
       'production',
       'fp-' || p.letter || '-' || n,
       'pq: deadlock detected #' || n,
       'PostgresError',
       'system_error',
       'unresolved',
       now() - ((n % 60) * interval '1 day') - ((n % 24) * interval '1 hour') - interval '3 days',
       now() - ((n % 60) * interval '1 day') - ((n % 24) * interval '1 hour'),
       1,
       u.users,
       CASE WHEN u.users = 0 THEN 0 ELSE 1 + (u.users - 1) / 10 END
FROM (VALUES ('00000000-0000-4000-8000-0000000000a2'::uuid, '2', 2000),
             ('00000000-0000-4000-8000-0000000000a3'::uuid, '3', 200)) AS p(id, letter, total)
CROSS JOIN LATERAL generate_series(1, p.total) AS n
CROSS JOIN LATERAL (
	SELECT CASE
		WHEN n <= p.total * 90 / 100 THEN (n * 7) % 4
		WHEN n <= p.total * 99 / 100 THEN 10 + (n * 13) % 91
		ELSE 500 + (n * 977) % 4501
	END AS users
) AS u;

-- One occurrence per issue at its last_seen (see the header for why).
INSERT INTO error_occurrences (issue_id, environment, platform, release_version, created_at)
SELECT i.id, i.environment, 'go', 'v31.0', i.last_seen
FROM issues i
WHERE i.project_id IN ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3');

-- Side rows: exactly affected_user_count of them per issue, so the §4.6 invariant holds by
-- construction. last_seen is spread over 60 days by (n + k) % 60 — evenly, so about half of all
-- side rows fall outside a 30-day cutoff (M3's data condition). first_seen precedes last_seen.
INSERT INTO issue_affected_users (issue_id, user_id, first_seen, last_seen)
SELECT i.id,
       'user-' || k,
       now() - (((i.n + k) % 60) * interval '1 day') - ((k % 30) * interval '1 day'),
       now() - (((i.n + k) % 60) * interval '1 day') - ((k % 24) * interval '1 hour')
FROM (
	SELECT id, affected_user_count, ('x' || right(id::text, 5))::bit(20)::int AS n
	FROM issues
	WHERE project_id IN ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3')
) AS i
CROSS JOIN LATERAL generate_series(1, i.affected_user_count) AS k;

INSERT INTO issue_affected_tenants (issue_id, tenant_id, first_seen, last_seen)
SELECT i.id,
       'tenant-' || k,
       now() - (((i.n + k) % 60) * interval '1 day') - ((k % 30) * interval '1 day'),
       now() - (((i.n + k) % 60) * interval '1 day') - ((k % 24) * interval '1 hour')
FROM (
	SELECT id, affected_tenant_count, ('x' || right(id::text, 5))::bit(20)::int AS n
	FROM issues
	WHERE project_id IN ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3')
) AS i
CROSS JOIN LATERAL generate_series(1, i.affected_tenant_count) AS k;

COMMIT;

ANALYZE issues;
ANALYZE issue_affected_users;
ANALYZE issue_affected_tenants;
ANALYZE error_occurrences;

-- What was built — the numbers "run twice, identical counts" compares.
SELECT 'issues (project A)' AS what, count(*) AS rows
FROM issues WHERE project_id = '00000000-0000-4000-8000-0000000000a2'
UNION ALL
SELECT 'issues (project B)', count(*)
FROM issues WHERE project_id = '00000000-0000-4000-8000-0000000000a3'
UNION ALL
SELECT 'issue_affected_users (both)', count(*)
FROM issue_affected_users a JOIN issues i ON i.id = a.issue_id
WHERE i.project_id IN ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3')
UNION ALL
SELECT 'issue_affected_tenants (both)', count(*)
FROM issue_affected_tenants a JOIN issues i ON i.id = a.issue_id
WHERE i.project_id IN ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3')
UNION ALL
SELECT 'error_occurrences (both)', count(*)
FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id
WHERE i.project_id IN ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3')
UNION ALL
SELECT 'side rows older than 30 days (users)', count(*)
FROM issue_affected_users a JOIN issues i ON i.id = a.issue_id
WHERE i.project_id IN ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-0000000000a3')
  AND a.last_seen < now() - interval '30 days'
UNION ALL
SELECT 'issues last_seen window (days)',
       (max(last_seen)::date - min(last_seen)::date)
FROM issues WHERE project_id = '00000000-0000-4000-8000-0000000000a2';

-- §13's distribution, project A: the three bands and their share.
SELECT CASE
         WHEN affected_user_count <= 3 THEN '0-3'
         WHEN affected_user_count <= 100 THEN '10-100'
         ELSE '500-5000'
       END AS band,
       count(*) AS issues,
       round(100.0 * count(*) / sum(count(*)) OVER (), 1) AS pct,
       min(affected_user_count) AS min_users,
       max(affected_user_count) AS max_users
FROM issues WHERE project_id = '00000000-0000-4000-8000-0000000000a2'
GROUP BY 1 ORDER BY min(affected_user_count);

-- The §4.6 invariant, both side tables: both counts must be 0.
SELECT 'users: issues whose count <> side rows' AS invariant, count(*) AS violations
FROM issues i
WHERE affected_user_count <> (SELECT count(*) FROM issue_affected_users a WHERE a.issue_id = i.id)
UNION ALL
SELECT 'tenants: issues whose count <> side rows', count(*)
FROM issues i
WHERE affected_tenant_count <> (SELECT count(*) FROM issue_affected_tenants a WHERE a.issue_id = i.id);
