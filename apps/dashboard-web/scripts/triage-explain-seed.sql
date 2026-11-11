-- sentinel-user-report-issue §4.3 / be.md criterion 8 — the seed behind the committed
-- `EXPLAIN (ANALYZE, BUFFERS)` plan in `status/be.md`.
--
-- This is CODE EVIDENCE, not a rig measurement. No registered rig reaches a Sentinel deployment
-- (contract *Decisions taken*, OQ-9/OQ-11/OQ-12), so the substitute the contract names is a plan
-- checked against a locally seeded dataset at the stated scale: ONE project holding
--   >= 2,000 `issues` rows of `issue_type = 'system_error'`, and
--   >= 100,000 `error_occurrences` rows spread over the last 30 days.
--
-- Run it against a DISPOSABLE Postgres that has had the dashboard migrations applied — never the
-- shared dev database (CLAUDE.md):
--
--   docker run -d --name sentinel-task04-pg -p 15433:5432 \
--     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
--   cd packages/db-migrations && GOWORK=off \
--     DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15433/sentinel?sslmode=disable \
--     go run ./cmd/migrate up -target dashboard
--   psql postgres://sentinel:changeme@localhost:15433/sentinel \
--     -f apps/dashboard-web/scripts/triage-explain-seed.sql
--
-- Everything it creates lives under the fixed ids below and is dropped by the last statement of
-- this file being re-run: it is idempotent by deleting its own organization first.

BEGIN;

DELETE FROM error_occurrences WHERE issue_id IN (
	SELECT id FROM issues WHERE project_id = '00000000-0000-4000-8000-0000000000e2'
);
DELETE FROM issues WHERE project_id = '00000000-0000-4000-8000-0000000000e2';
DELETE FROM projects WHERE id = '00000000-0000-4000-8000-0000000000e2';
DELETE FROM organizations WHERE id = '00000000-0000-4000-8000-0000000000e1';

INSERT INTO organizations (id, name, slug)
VALUES ('00000000-0000-4000-8000-0000000000e1', 'triage-explain', 'triage-explain');

INSERT INTO projects (id, name, organization_id, api_key, api_key_hash)
VALUES ('00000000-0000-4000-8000-0000000000e2', 'triage-explain', '00000000-0000-4000-8000-0000000000e1',
        'pk_triage_explain', 'hash_triage_explain');

-- 2,000 system_error issues in the one project.
INSERT INTO issues (id, project_id, fingerprint, message, error_class, issue_type)
SELECT gen_random_uuid(),
       '00000000-0000-4000-8000-0000000000e2',
       'fp-' || n,
       'pq: deadlock detected #' || n,
       'PostgresError',
       'system_error'
FROM generate_series(1, 2000) AS n;

-- 100,000 occurrences spread evenly over the last 30 days, 50 per issue. `release_version` cycles
-- so that a release match is neither universal nor absent, and `environment` alternates, which is
-- what makes the two ORDER BY signal expressions do real work rather than collapsing to constants.
INSERT INTO error_occurrences (id, issue_id, environment, platform, release_version, created_at)
SELECT gen_random_uuid(),
       i.id,
       CASE WHEN k % 3 = 0 THEN 'staging' ELSE 'production' END,
       'go',
       'v31.' || (k % 7),
       now() - (random() * interval '30 days')
FROM issues i
CROSS JOIN generate_series(1, 50) AS k
WHERE i.project_id = '00000000-0000-4000-8000-0000000000e2';

COMMIT;

ANALYZE issues;
ANALYZE error_occurrences;

SELECT 'issues (system_error, this project)' AS what, count(*) AS rows
FROM issues WHERE project_id = '00000000-0000-4000-8000-0000000000e2' AND issue_type = 'system_error'
UNION ALL
SELECT 'error_occurrences (this project)', count(*)
FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id
WHERE i.project_id = '00000000-0000-4000-8000-0000000000e2'
UNION ALL
SELECT 'occurrence window (days)',
       (max(eo.created_at)::date - min(eo.created_at)::date)
FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id
WHERE i.project_id = '00000000-0000-4000-8000-0000000000e2';

-- §4.3's candidate query, verbatim from `findTriageCandidates` in
-- `src/lib/server/triage.ts`, with the window anchored on "30 minutes ago" so it always lands
-- inside the seeded 30 days.
EXPLAIN (ANALYZE, BUFFERS)
SELECT
	i.id AS issue_id,
	i.message AS message,
	i.error_class AS error_class,
	occ.latest_occurrence_at AS latest_occurrence_at,
	occ.release_versions AS release_versions,
	occ.environments AS environments
FROM issues i
CROSS JOIN LATERAL (
	SELECT
		max(eo.created_at) AS latest_occurrence_at,
		array_remove(array_agg(DISTINCT eo.release_version), NULL) AS release_versions,
		array_remove(array_agg(DISTINCT eo.environment), NULL) AS environments
	FROM error_occurrences eo
	WHERE eo.issue_id = i.id
	  AND eo.created_at >= (now() - interval '60 minutes')::timestamp
	  AND eo.created_at <= (now() - interval '0 minutes')::timestamp
) occ
WHERE i.project_id = '00000000-0000-4000-8000-0000000000e2'
  AND i.issue_type = 'system_error'
  AND occ.latest_occurrence_at IS NOT NULL
ORDER BY
	coalesce('v31.3' = ANY(occ.release_versions), false) DESC,
	coalesce('production' = ANY(occ.environments), false) DESC,
	occ.latest_occurrence_at DESC,
	i.id ASC
LIMIT 5;
