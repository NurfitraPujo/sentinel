-- sentinel-occurrence-dedup-project §13 — the ONE seed behind M1 and M2 (the labelled fallback on
-- the local compose stack, contract decision 10). The shape of issue-trend-seed.sql /
-- affected-users-seed.sql: own organization, deleted first; fixed ids; deterministic; counts in
-- this header.
--
-- WHAT IT BUILDS (the counts §13 fixes — production is 769,590 rows / 1421 MB on 2026-09-16,
-- decision 23, and the seed must not be smaller than the table it stands in for):
--
--   organizations       1          00000000-0000-4000-8000-0000000000e1  (slug occurrence-dedup-seed)
--   projects           10          00000000-0000-4000-8000-0000000000e2 .. ...00eb  (name occurrence-dedup-seed-01 .. -10)
--   issues          1,000          100 per project, 00000000-0000-4000-8000-00000e<pp><nnnn> (pp = project 01..0a, nnnn = n 0001..0064)
--   error_occurrences 1,000,000    1,000 per issue, EVERY ROW A DISTINCT event_id:
--                                  5eed0000-0000-4000-8000-<12 hex digits of the global row index 1..1,000,000>
--                                  — distinct by construction (a bijection of the row index), not by hashing,
--                                  and asserted at the end of this file.
--   project_api_keys    1          one project-scoped `ingest` key on project 01, plaintext
--                                  sk_seed_dedup_0123456789abcdef0123456789abcdef (dev-only constant, the
--                                  row stores sha256), for tests/load (M2's continuous posting) and any
--                                  curl. project_key for the wire = the project NAME, occurrence-dedup-seed-01.
--
-- WHEN IT RUNS. Against the compose database at the PREVIOUS tag's schema (v0.2.18, c5082e3 —
-- decisions 17 and 26), i.e. BEFORE 1725200000_add_error_occurrences_project_id_dedup.sql is applied:
-- the file names no project_id column, so it loads on either side of that migration, and when it is
-- loaded before it the migration's backfill (block (b)) has these 1,000,000 rows to rewrite — which is
-- what dedup_backfill_seconds measures. Loaded after it, the rows carry project_id = NULL and sit
-- outside the new key (contract §6: seed scripts insert without project_id — fine for seeds).
--
-- Row shape: a three-frame golang stacktrace and a small metadata object per row (~0.5 kB of JSONB,
-- smaller than production's ~1.85 kB mean; the index cost the migration pays is per ROW, and the
-- heap rewrite the backfill pays is per row too, so the row count is the number that must not be
-- smaller). trace_id is set (idx_error_occurrences_trace_id is maintained), release_version is set
-- (idx_occurrences_issue_release is maintained), created_at is spread over the last ~30 days by the
-- row's ordinal within its issue. Deterministic (no random()) and now()-relative.
--
-- Idempotent: deletes its own organization's rows first (never anything else's), in FK order —
-- issues.project_id has no ON DELETE CASCADE (1716508800_init.sql), so occurrences and issues go
-- explicitly; keys and projects cascade from the organization but are deleted explicitly anyway so
-- the order reads. A re-run against a database that has had 1725200000 applied is fine: the delete
-- takes the rows out of both unique indexes.
--
-- Run it against the COMPOSE stack's database for §13 (that database is the labelled fallback's
-- venue and is disposable by construction — `docker compose down -v` recreates it), or any
-- disposable Postgres with the processor migrations applied up to 1725100000; never a database you
-- are not prepared to re-seed:
--
--   psql postgres://sentinel:changeme@localhost:5432/sentinel \
--     -v ON_ERROR_STOP=1 -f apps/dashboard-web/scripts/occurrence-dedup-seed.sql
--
-- Loading 1,000,000 rows through seven maintained indexes takes a few minutes; the counts are
-- printed at the end (the "row count read back" §13 asks to paste) and the file FAILS if any count
-- is below the floor or any event_id repeats.

BEGIN;

-- Idempotency: remove what a previous run created, by this seed's own ids only, in FK order.
DELETE FROM error_search_index WHERE occurrence_id IN (
	SELECT eo.id FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id
	 WHERE i.project_id IN (SELECT id FROM projects WHERE organization_id = '00000000-0000-4000-8000-0000000000e1'));
DELETE FROM error_occurrences WHERE issue_id IN (
	SELECT id FROM issues WHERE project_id IN (
		SELECT id FROM projects WHERE organization_id = '00000000-0000-4000-8000-0000000000e1'));
DELETE FROM issue_activity WHERE issue_id IN (
	SELECT id FROM issues WHERE project_id IN (
		SELECT id FROM projects WHERE organization_id = '00000000-0000-4000-8000-0000000000e1'));
DELETE FROM issues WHERE project_id IN (
	SELECT id FROM projects WHERE organization_id = '00000000-0000-4000-8000-0000000000e1');
DELETE FROM project_api_keys WHERE organization_id = '00000000-0000-4000-8000-0000000000e1';
DELETE FROM projects WHERE organization_id = '00000000-0000-4000-8000-0000000000e1';
DELETE FROM organizations WHERE id = '00000000-0000-4000-8000-0000000000e1';

INSERT INTO organizations (id, name, slug)
VALUES ('00000000-0000-4000-8000-0000000000e1', 'occurrence-dedup-seed', 'occurrence-dedup-seed');

-- Ten projects: ids ...00e2 .. ...00eb (p = 1..10 → e1 + p), names -01 .. -10.
INSERT INTO projects (id, name, organization_id, api_key, api_key_hash)
SELECT ('00000000-0000-4000-8000-0000000000' || to_hex(225 + p))::uuid,
       'occurrence-dedup-seed-' || lpad(p::text, 2, '0'),
       '00000000-0000-4000-8000-0000000000e1',
       'pk_occurrence_dedup_seed_' || lpad(p::text, 2, '0'),
       'hash_occurrence_dedup_seed_' || lpad(p::text, 2, '0')
FROM generate_series(1, 10) AS p;

-- The load harness's credential: a project-scoped ingest key on project 01 (the credential fixes the
-- tenant, B7; the events name the project by its NAME in project_key). The shape sourcemaps-seed.sh
-- writes: the row stores sha256(secret), which is what the ingestor's authenticator looks up.
INSERT INTO project_api_keys
	(project_id, organization_id, key_hash, key_prefix, name, scope, status, rate_limit_rpm, created_by)
VALUES ('00000000-0000-4000-8000-0000000000e2', '00000000-0000-4000-8000-0000000000e1',
        encode(digest('sk_seed_dedup_0123456789abcdef0123456789abcdef'::bytea, 'sha256'), 'hex'),
        substr('sk_seed_dedup_0123456789abcdef0123456789abcdef', 1, 12),
        'occurrence-dedup-seed', 'ingest', 'active', 100000, 'occurrence-dedup-seed');

-- 1,000 issues: 100 per project, n = 1..100. Deterministic ids name the project and n, so a row can
-- be named; the fingerprint is unique per (project, n) as the (project_id, fingerprint) key needs;
-- count = the 1,000 occurrences each will carry.
INSERT INTO issues (id, project_id, environment, fingerprint, message, error_class, issue_type, status,
                    first_seen, last_seen, count)
SELECT ('00000000-0000-4000-8000-00000e' || lpad(to_hex(p), 2, '0') || lpad(to_hex(n), 4, '0'))::uuid,
       ('00000000-0000-4000-8000-0000000000' || to_hex(225 + p))::uuid,
       'production',
       'fp-dedup-' || lpad(p::text, 2, '0') || '-' || n,
       'pq: connection reset by peer #' || n,
       'PostgresError',
       'system_error',
       'unresolved',
       now() - interval '30 days',
       now() - ((n % 24) * interval '1 hour'),
       1000
FROM generate_series(1, 10) AS p
CROSS JOIN LATERAL generate_series(1, 100) AS n;

-- 1,000,000 occurrences: k = 1..1,000 per issue. The global row index g = ((p-1)*100 + (n-1))*1000 + k
-- runs 1..1,000,000 exactly once, and event_id is its 12-hex-digit encoding under a fixed prefix —
-- distinct by construction. trace_id is g as 32 hex digits. created_at steps back 43 minutes per k
-- (1,000 × 43 min ≈ 30 days), so the created_at index is not one hot page.
INSERT INTO error_occurrences
	(issue_id, environment, platform, release_version, stacktrace, metadata, trace_id, span_id, created_at, event_id)
SELECT ('00000000-0000-4000-8000-00000e' || lpad(to_hex(p), 2, '0') || lpad(to_hex(n), 4, '0'))::uuid,
       'production',
       'golang',
       'v1.0.' || (n % 5),
       jsonb_build_array(
           jsonb_build_object('file', 'internal/db/pool.go', 'function', 'acquire', 'line', 100 + (k % 40), 'in_app', true),
           jsonb_build_object('file', 'internal/http/handler.go', 'function', 'handleRequest', 'line', 250 + (n % 12), 'in_app', true),
           jsonb_build_object('file', 'net/http/server.go', 'function', 'serverHandler.ServeHTTP', 'line', 2938, 'in_app', false)),
       jsonb_build_object('seed', 'occurrence-dedup', 'project', p, 'issue', n, 'k', k),
       lpad(to_hex(((p - 1) * 100 + (n - 1)) * 1000 + k), 32, '0'),
       lpad(to_hex(k), 16, '0'),
       now() - (k * interval '43 minutes'),
       '5eed0000-0000-4000-8000-' || lpad(to_hex(((p - 1) * 100 + (n - 1)) * 1000 + k), 12, '0')
FROM generate_series(1, 10) AS p
CROSS JOIN LATERAL generate_series(1, 100) AS n
CROSS JOIN LATERAL generate_series(1, 1000) AS k;

COMMIT;

ANALYZE issues;
ANALYZE error_occurrences;

-- What was built — the counts to paste. The floors are §13's (≥ 1,000,000 / ≥ 1,000 / ≥ 10, every
-- event_id distinct); the DO block below makes a short load a named failure, not a smaller number.
SELECT 'projects' AS what, count(*) AS rows
  FROM projects WHERE organization_id = '00000000-0000-4000-8000-0000000000e1'
UNION ALL
SELECT 'issues', count(*)
  FROM issues WHERE project_id IN (SELECT id FROM projects WHERE organization_id = '00000000-0000-4000-8000-0000000000e1')
UNION ALL
SELECT 'error_occurrences', count(*)
  FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id
 WHERE i.project_id IN (SELECT id FROM projects WHERE organization_id = '00000000-0000-4000-8000-0000000000e1')
UNION ALL
SELECT 'error_occurrences with a distinct event_id', count(DISTINCT eo.event_id)
  FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id
 WHERE i.project_id IN (SELECT id FROM projects WHERE organization_id = '00000000-0000-4000-8000-0000000000e1')
UNION ALL
SELECT 'error_occurrences (whole table)', count(*) FROM error_occurrences
UNION ALL
SELECT 'error_occurrences total relation size (MB)', pg_total_relation_size('error_occurrences') / 1048576;

DO $$
DECLARE
	projects_n bigint;
	issues_n bigint;
	rows_n bigint;
	distinct_n bigint;
BEGIN
	SELECT count(*) INTO projects_n FROM projects WHERE organization_id = '00000000-0000-4000-8000-0000000000e1';
	SELECT count(*) INTO issues_n FROM issues
	 WHERE project_id IN (SELECT id FROM projects WHERE organization_id = '00000000-0000-4000-8000-0000000000e1');
	SELECT count(*), count(DISTINCT eo.event_id) INTO rows_n, distinct_n
	  FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id
	 WHERE i.project_id IN (SELECT id FROM projects WHERE organization_id = '00000000-0000-4000-8000-0000000000e1');
	IF projects_n < 10 OR issues_n < 1000 OR rows_n < 1000000 OR distinct_n <> rows_n THEN
		RAISE EXCEPTION 'occurrence-dedup-seed: built % projects / % issues / % occurrences (% distinct event_id); §13 wants >= 10 / >= 1,000 / >= 1,000,000 with every event_id distinct',
			projects_n, issues_n, rows_n, distinct_n;
	END IF;
END $$;
