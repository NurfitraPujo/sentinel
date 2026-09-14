-- re-run immediately before every browser or measurement session: every timestamp in this file is
-- now()-relative AT LOAD TIME, and the rows the measurements and the browser rows look at — the
-- ~20 date snoozes (half lapsed, half still running), the `next` resolutions inside the last day,
-- the newest release "yesterday" — age within hours of loading. A seed loaded yesterday makes
-- every date snooze read as lapsed and reads as a false deviation.
--
-- sentinel-releases-snooze §13 (decisions 35, 47) — the ONE seed behind M2, M3 and fe's `run`/`shot`
-- rows. Committed so every side loads the same data set instead of hand-writing its own; the shape
-- of scripts/issue-trend-seed.sql (own organization, deleted first; fixed ids; deterministic;
-- now()-relative). `be` writes it, `fe` runs the same file.
--
-- What it builds, AFTER the two §4.5 migrations (1724900000_add_releases_and_issue_snooze.sql and
-- 1725000000_backfill_releases_and_first_release_version.sql) have run — this file writes its own
-- `releases` rows and `first_release_version` values; the backfill's NOT EXISTS guards never see
-- them because it ran first:
--
--   * one organization, three projects and three users (decision 47):
--       A `checkout` (semver)  300 `releases` rows v0.1.0 … v3.8.3 (minor 1–10, patch 0–7), `first_seen` spread over
--                              180 days ending YESTERDAY (n = 1 oldest, n = 300 newest, 14 h 24 min
--                              apart), ONE pair with identical `first_seen` and different versions
--                              (n = 150 and n = 151 — the tie case, decision 3), `occurrence_count`
--                              skewed (90 % at 1–20, 9 % at 100–1,000, 1 % at 10,000–100,000);
--       B `api` (git SHA)      40 `releases` rows of 7-hex-char versions (`substr(md5(…), 1, 7)`),
--                              `first_seen` spread over 30 days — the ordering-by-date case that
--                              parsing the version cannot do (decision 1);
--       C `worker`             NO `releases` rows: one `unresolved` issue with
--                              `first_release_version = NULL` and one occurrence whose
--                              `release_version` is '' — R3's empty state and D3's disabled picker;
--   * 2,000 `issues` in A (n = 1..2000) in THREE environments — n % 5 = 0 → `staging` (400,
--     20 %), n % 5 = 1 → `canary` (400), else `production` (1,200) — so the all-environments
--     `issuesIntroduced` is visibly larger than any one environment's and the staging-only users
--     have issues to open. `first_release_version` drawn from A's releases:
--       n = 1..60      → release n = 100  (60 issues)   ─┐ the "50+" releases
--       n = 61..130    → release n = 200  (70 issues)    │
--       n = 131..210   → release n = 300  (80 issues)   ─┘
--       n = 211..800   → release 1 + (n × 7) % 297, skipping 250 (≈ 2 per release: the "0–3" set)
--       n = 801..805   → NULL, but their occurrence carries release n = 250's version — release
--                        250 is the one whose issues are NULL-versioned only, so it reads 0
--       n = 806..2000  → NULL (pre-feature rows: no first release known)
--     Status by n % 200 (ten issues per residue, disjoint by construction):
--       1, 2, 3   → `resolved`, `resolved_in_version = 'next'`, `resolved_at` inside the last 21 h
--                   (AFTER the newest release, so they genuinely wait: nothing seeded qualifies for
--                   the catch-up — the M3 set and the I11 case; 30 issues)
--       4, 5      → `resolved` in an EXISTING release (1 + n % 300)                (20 issues)
--       6         → `resolved` in a version TYPED and never ingested, 'v9.<n>.0' (10 issues — the
--                   fallback row of §4.3's table)
--       7         → `ignored`, `snoozed_until` 1–5 days AGO (lapsed; ends on the next occurrence)
--       8         → `ignored`, `snoozed_until` 1–5 days AHEAD (running)          (10 + 10 issues)
--       9         → `ignored`, `snooze_occurrence_target = 10`, `snooze_count_at = count − 9`
--                   (ONE occurrence away — the I9 shape)
--       10        → `ignored`, target 100, `count_at = count − (n % 30)`            (10 + 10 issues)
--       11        → `ignored` indefinitely (no snooze columns)                       (10 issues)
--       otherwise → `unresolved`                                                     (1,890 issues)
--     `first_seen` is the first release's `first_seen` + (n % 20) h when attributed and spread
--     over 170 days otherwise; `last_seen` is a 60-value grid inside the last 10 days (never
--     before `first_seen`); `count` skewed like the trend seed's, except the target-snoozed set
--     (50–89, so `count_at` stays ≥ 0);
--   * 300 `issues` in B (n = 1..300), all `production`, `first_release_version` = release
--     1 + n % 40; n % 30 = 1 → `resolved` in the SHA of release 1 + (n × 3) % 40 (10 issues), so
--     flows I4–I6 have data;
--   * ONE `error_occurrences` row per issue at the issue's `last_seen` carrying the issue's first
--     release (NULL where it has none; release 250's version for n = 801..805 in A; '' in C), so
--     retention's orphan rule ("no occurrences and first_seen < cutoff") does not delete the set
--     on the first sweep — the trend seed's own rule;
--   * declared environments: A `production` (default), `staging`, `canary`; B and C `production`.
--
-- THE THREE USERS — how to sign in to a local dashboard as each of them.
--
--   measuring user     releases-snooze-engineer@example.invalid
--                      id releases-snooze-seed-engineer — org role `engineer`, a `project_members`
--                      row on A, B and C, NO environment grant (null scope). The ONLY caller shape
--                      that reaches the releases loader (decision 20) — M2 runs as this user. Sees
--                      the control block (`canWrite = true`) and the picker (D5).
--   scoped viewer      releases-snooze-viewer@example.invalid
--                      id releases-snooze-seed-viewer — org role `viewer`, a `project_members` row
--                      on A (role `viewer`), a `staging`-only grant on A. No write role, a non-null
--                      scope: the releases page answers `404 Project not found` (R5, decision 20)
--                      and the detail page has no control block (`canWrite = false`, D2).
--   scoped engineer    releases-snooze-staging-engineer@example.invalid
--                      id releases-snooze-seed-staging-engineer — org role `engineer` and a
--                      `staging`-only grant on A, no `project_members` row. A write role AND a
--                      non-null scope — the only shape that reaches decision 26's empty picker in
--                      a browser: `canWrite = true`, `releases: []`, the picker disabled with
--                      "No releases seen for this project yet — pick next release or type a
--                      version" (D3's third clause). Never for a count: no scoped
--                      `issuesIntroduced` exists (decision 36).
--
--   A local dashboard has no password. Sign in through the REAL Auth.js magic-link exchange, the
--   mechanism tests/e2e/dashboard_test.go: TestU24_MagicLinkSignIn documents: @auth/core compares
--   the URL token against `verification_token.token` as sha256(token + AUTH_SECRET), so mint the
--   row yourself and load the callback (AUTH_SECRET is docker-compose.yml's plaintext dev value;
--   the row is single-use — mint a new one per sign-in):
--
--     TOKEN=$(openssl rand -hex 16)
--     EMAIL=releases-snooze-engineer@example.invalid
--     SECRET=dev-only-insecure-secret-change-me-please-32chars   # AUTH_SECRET
--     HASH=$(printf '%s%s' "$TOKEN" "$SECRET" | sha256sum | cut -d' ' -f1)
--     psql "$DATABASE_URL" -c "INSERT INTO verification_token (identifier, token, expires)
--                              VALUES ('$EMAIL', '$HASH', now() + interval '15 minutes')"
--     open "http://localhost:3000/auth/callback/email?token=$TOKEN&email=$EMAIL&callbackUrl=/"
--
--   The browser lands on `/` with the session cookie; the projects list at
--   `/releases-snooze-seed/projects` shows the three projects and the *Releases* link.
--
-- Everything is DETERMINISTIC (no random(); the SHA versions are md5 of a fixed string) and
-- NOW()-RELATIVE (no literal calendar date anywhere in this file): two runs in the same minute
-- produce identical rows; two runs an hour apart produce identical rows shifted by an hour.
-- Idempotent by deleting its own organization and users first.
--
-- Run it against a Postgres that has had ALL the migrations applied (the compose stack's database
-- for M2/M3 — decision 35 — or a disposable one for the load test); never a database you are not
-- prepared to re-seed:
--
--   psql postgres://sentinel:changeme@localhost:5432/sentinel \
--     -f apps/dashboard-web/scripts/releases-snooze-seed.sql
--
--   (disposable: docker run -d --name sentinel-rs-pg -p 15771:5432 \
--     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
--    then, from packages/db-migrations, with DB_URL_PROCESSOR / DB_URL_INGESTOR / DB_URL_DASHBOARD all
--    set to postgres://sentinel:changeme@localhost:15771/sentinel?sslmode=disable:
--      GOWORK=off go run ./cmd/migrate up -target=processor   (then ingestor, then dashboard))
--
-- Fixed ids:
--   organization       00000000-0000-4000-8000-0000000000c1   (slug releases-snooze-seed)
--   project A          00000000-0000-4000-8000-0000000000c2   (checkout — 300 releases, 2,000 issues)
--   project B          00000000-0000-4000-8000-0000000000c3   (api — 40 SHA releases, 300 issues)
--   project C          00000000-0000-4000-8000-0000000000c4   (worker — no releases, 1 issue)
--   users              releases-snooze-seed-engineer | -viewer | -staging-engineer
--   issue ids          00000000-0000-4000-8000-00000c<p><nnnnn>  p = 2 (A) | 3 (B) | 4 (C), nnnnn = hex(n)
--     `next` #1        00000000-0000-4000-8000-00000c200001   (A, n = 1)
--     lapsed snooze #1 00000000-0000-4000-8000-00000c200007   (A, n = 7)
--     one-away #1      00000000-0000-4000-8000-00000c200009   (A, n = 9)
--     NULL-only rel.   00000000-0000-4000-8000-00000c200321   (A, n = 801 — occurrence in release 250)
--     C's issue        00000000-0000-4000-8000-00000c400001

BEGIN;

-- The clock, taken once so every statement below agrees on what "now" is.
CREATE TEMP TABLE seed_clock ON COMMIT DROP AS
SELECT now() AS load_time;

-- Idempotency: remove what a previous run created. issues.project_id has no ON DELETE CASCADE
-- (1716508800_init.sql), so issues and their occurrences go explicitly; releases, environments,
-- grants and memberships cascade from the project or the user; projects from the organization.
DELETE FROM error_occurrences WHERE issue_id IN (
	SELECT id FROM issues WHERE project_id IN (
		'00000000-0000-4000-8000-0000000000c2', '00000000-0000-4000-8000-0000000000c3',
		'00000000-0000-4000-8000-0000000000c4'
	)
);
DELETE FROM issues WHERE project_id IN (
	'00000000-0000-4000-8000-0000000000c2', '00000000-0000-4000-8000-0000000000c3',
	'00000000-0000-4000-8000-0000000000c4'
);
DELETE FROM projects WHERE organization_id = '00000000-0000-4000-8000-0000000000c1';
DELETE FROM organization_members WHERE organization_id = '00000000-0000-4000-8000-0000000000c1';
DELETE FROM organizations WHERE id = '00000000-0000-4000-8000-0000000000c1';
DELETE FROM verification_token WHERE identifier IN (
	'releases-snooze-engineer@example.invalid', 'releases-snooze-viewer@example.invalid',
	'releases-snooze-staging-engineer@example.invalid'
);
DELETE FROM "user" WHERE id IN (
	'releases-snooze-seed-engineer', 'releases-snooze-seed-viewer', 'releases-snooze-seed-staging-engineer'
);

INSERT INTO organizations (id, name, slug)
VALUES ('00000000-0000-4000-8000-0000000000c1', 'releases-snooze-seed', 'releases-snooze-seed');

INSERT INTO projects (id, name, organization_id, api_key, api_key_hash)
VALUES ('00000000-0000-4000-8000-0000000000c2', 'checkout', '00000000-0000-4000-8000-0000000000c1',
        'pk_releases_snooze_seed_a', 'hash_releases_snooze_seed_a'),
       ('00000000-0000-4000-8000-0000000000c3', 'api', '00000000-0000-4000-8000-0000000000c1',
        'pk_releases_snooze_seed_b', 'hash_releases_snooze_seed_b'),
       ('00000000-0000-4000-8000-0000000000c4', 'worker', '00000000-0000-4000-8000-0000000000c1',
        'pk_releases_snooze_seed_c', 'hash_releases_snooze_seed_c');

-- The three users (decision 47; the header says who each is for).
INSERT INTO "user" (id, name, email)
VALUES ('releases-snooze-seed-engineer', 'Releases Snooze Engineer', 'releases-snooze-engineer@example.invalid'),
       ('releases-snooze-seed-viewer', 'Releases Snooze Viewer', 'releases-snooze-viewer@example.invalid'),
       ('releases-snooze-seed-staging-engineer', 'Releases Snooze Staging Engineer', 'releases-snooze-staging-engineer@example.invalid');
INSERT INTO organization_members (organization_id, user_id, role)
VALUES ('00000000-0000-4000-8000-0000000000c1', 'releases-snooze-seed-engineer', 'engineer'),
       ('00000000-0000-4000-8000-0000000000c1', 'releases-snooze-seed-viewer', 'viewer'),
       ('00000000-0000-4000-8000-0000000000c1', 'releases-snooze-seed-staging-engineer', 'engineer');
-- The measuring user is a member of all three projects (null scope: no grant row); the viewer has
-- a project_members row on A only (read, conveyed by the row — never write).
INSERT INTO project_members (project_id, user_id, role)
VALUES ('00000000-0000-4000-8000-0000000000c2', 'releases-snooze-seed-engineer', 'developer'),
       ('00000000-0000-4000-8000-0000000000c3', 'releases-snooze-seed-engineer', 'developer'),
       ('00000000-0000-4000-8000-0000000000c4', 'releases-snooze-seed-engineer', 'developer'),
       ('00000000-0000-4000-8000-0000000000c2', 'releases-snooze-seed-viewer', 'viewer');
-- The two staging-only grants on A (sentinel-project-environments §2.2: a row narrows; absence
-- means every environment).
INSERT INTO project_environment_grants (user_id, project_id, environments, created_by)
VALUES ('releases-snooze-seed-viewer', '00000000-0000-4000-8000-0000000000c2', ARRAY['staging'], 'releases-snooze-seed'),
       ('releases-snooze-seed-staging-engineer', '00000000-0000-4000-8000-0000000000c2', ARRAY['staging'], 'releases-snooze-seed');

-- Declared environments (sentinel-project-environments decision 8): A has the three the issues
-- use; B and C have production.
INSERT INTO project_environments (project_id, name, display_name, is_default)
VALUES ('00000000-0000-4000-8000-0000000000c2', 'production', 'Production', true),
       ('00000000-0000-4000-8000-0000000000c2', 'staging', 'Staging', false),
       ('00000000-0000-4000-8000-0000000000c2', 'canary', 'Canary', false),
       ('00000000-0000-4000-8000-0000000000c3', 'production', 'Production', true),
       ('00000000-0000-4000-8000-0000000000c4', 'production', 'Production', true);

-- Project A's 300 releases, n = 1..300. version = 'v' || major.minor.patch with major = (n − 1) / 80
-- (0..3), minor = 1 + ((n − 1) % 80) / 8 (1..10), patch = (n − 1) % 8 — v0.1.0 for n = 1, v3.8.3
-- for n = 300, every one distinct. first_seen = yesterday − (300 − n) × 14 h 24 min: n = 300 is the
-- newest (load − 1 day), n = 1 the oldest (load − 180.4 days); n = 151 takes n = 150's instant —
-- the tie pair. last_seen = first_seen + (n % 40) days, clamped to a minute before load time.
CREATE TEMP TABLE seed_releases_a ON COMMIT DROP AS
SELECT n AS rn,
       'v' || ((n - 1) / 80) || '.' || (1 + ((n - 1) % 80) / 8) || '.' || ((n - 1) % 8) AS version,
       c.load_time - interval '1 day'
         - ((300 - CASE WHEN n = 151 THEN 150 ELSE n END) * interval '864 minutes') AS first_seen,
       c.load_time AS load_time
FROM seed_clock c
CROSS JOIN generate_series(1, 300) AS n;

INSERT INTO releases (project_id, version, first_seen, last_seen, occurrence_count)
SELECT '00000000-0000-4000-8000-0000000000c2',
       r.version,
       r.first_seen,
       LEAST(r.load_time - interval '1 minute', r.first_seen + ((r.rn % 40) * interval '1 day')),
       CASE
         WHEN r.rn % 100 = 0 THEN 10000 + (r.rn * 977) % 90001   -- 1 %: 10,000–100,000
         WHEN r.rn % 11 = 0  THEN 100 + (r.rn * 13) % 901        -- ~9 %: 100–1,000
         ELSE 1 + (r.rn * 7) % 20                               -- the rest: 1–20
       END
FROM seed_releases_a r;

-- Project B's 40 SHA releases, n = 1..40: a 7-hex-char version that is NOT orderable by parsing,
-- first_seen = yesterday − (40 − n) × 17 h (n = 40 newest, n = 1 at load − 28.6 days).
CREATE TEMP TABLE seed_releases_b ON COMMIT DROP AS
SELECT n AS rn,
       substr(md5('releases-snooze-seed-api-' || n), 1, 7) AS version,
       c.load_time - interval '1 day' - ((40 - n) * interval '17 hours') AS first_seen,
       c.load_time AS load_time
FROM seed_clock c
CROSS JOIN generate_series(1, 40) AS n;

INSERT INTO releases (project_id, version, first_seen, last_seen, occurrence_count)
SELECT '00000000-0000-4000-8000-0000000000c3',
       r.version,
       r.first_seen,
       LEAST(r.load_time - interval '1 minute', r.first_seen + ((r.rn % 5) * interval '1 day')),
       1 + (r.rn * 37) % 400
FROM seed_releases_b r;

-- Project A's 2,000 issues, every column a deterministic function of n (see the header). Built in
-- a temp table first so the occurrence insert below can read the same derived values.
CREATE TEMP TABLE seed_issues_a ON COMMIT DROP AS
SELECT n,
       ('00000000-0000-4000-8000-00000c2' || lpad(to_hex(n), 5, '0'))::uuid AS id,
       CASE n % 5 WHEN 0 THEN 'staging' WHEN 1 THEN 'canary' ELSE 'production' END AS environment,
       rel.rn AS release_n,
       rel.version AS first_release_version,
       -- the occurrence's release: the first release, or release 250 for the NULL-only set
       CASE WHEN n BETWEEN 801 AND 805 THEN (SELECT version FROM seed_releases_a WHERE seed_releases_a.rn = 250)
            ELSE rel.version END AS occurrence_release,
       fs.first_seen,
       LEAST(c.load_time - interval '1 minute',
             GREATEST(fs.first_seen + interval '1 hour',
                      c.load_time - ((n % 6) * interval '1 hour') - (((n / 6) % 10) * interval '1 day'))) AS last_seen,
       cnt.count,
       CASE
         WHEN n % 200 IN (1, 2, 3, 4, 5, 6) THEN 'resolved'
         WHEN n % 200 IN (7, 8, 9, 10, 11)  THEN 'ignored'
         ELSE 'unresolved'
       END AS status,
       CASE
         WHEN n % 200 IN (1, 2, 3) THEN 'next'
         WHEN n % 200 IN (4, 5)    THEN (SELECT version FROM seed_releases_a WHERE seed_releases_a.rn = 1 + n % 300)
         WHEN n % 200 = 6          THEN 'v9.' || n || '.0'
         ELSE NULL
       END AS resolved_in_version,
       CASE
         WHEN n % 200 IN (1, 2, 3)    THEN c.load_time - ((1 + n % 20) * interval '1 hour')
         WHEN n % 200 IN (4, 5, 6)    THEN c.load_time - ((n % 30) * interval '1 day') - ((n % 7) * interval '1 hour')
         ELSE NULL
       END AS resolved_at,
       CASE
         WHEN n % 200 = 7 THEN c.load_time - ((1 + n % 5) * interval '1 day')   -- lapsed
         WHEN n % 200 = 8 THEN c.load_time + ((1 + n % 5) * interval '1 day')   -- running
         ELSE NULL
       END AS snoozed_until,
       CASE WHEN n % 200 = 9 THEN 10 WHEN n % 200 = 10 THEN 100 ELSE NULL END AS snooze_occurrence_target,
       CASE WHEN n % 200 = 9 THEN cnt.count - 9 WHEN n % 200 = 10 THEN cnt.count - (n % 30) ELSE NULL END AS snooze_count_at
FROM seed_clock c
CROSS JOIN generate_series(1, 2000) AS n
LEFT JOIN seed_releases_a rel ON rel.rn = CASE
	WHEN n <= 60  THEN 100
	WHEN n <= 130 THEN 200
	WHEN n <= 210 THEN 300
	WHEN n <= 800 THEN CASE WHEN 1 + (n * 7) % 297 >= 250 THEN 2 + (n * 7) % 297 ELSE 1 + (n * 7) % 297 END
	ELSE NULL
END
CROSS JOIN LATERAL (
	SELECT CASE
		WHEN rel.rn IS NOT NULL         THEN rel.first_seen + ((n % 20) * interval '1 hour')
		WHEN n BETWEEN 801 AND 805     THEN (SELECT first_seen FROM seed_releases_a WHERE seed_releases_a.rn = 250) + ((n % 20) * interval '1 hour')
		ELSE c.load_time - ((n % 170) * interval '1 day') - ((n % 24) * interval '1 hour')
	END AS first_seen
) AS fs
CROSS JOIN LATERAL (
	SELECT CASE
		WHEN n % 200 IN (9, 10) THEN 50 + n % 40                -- the target-snoozed set: count_at ≥ 0
		WHEN n % 100 = 0        THEN 10000 + (n * 977) % 90001   -- 1 %
		WHEN n % 11 = 0         THEN 100 + (n * 13) % 901        -- ~9 %
		ELSE 1 + (n * 7) % 20                                    -- the rest
	END::bigint AS count
) AS cnt;

INSERT INTO issues (id, project_id, environment, fingerprint, message, error_class, issue_type, status,
                    first_seen, last_seen, count, first_release_version,
                    resolved_in_version, resolved_at, resolved_by_type, resolved_by,
                    snoozed_until, snooze_occurrence_target, snooze_count_at)
SELECT i.id,
       '00000000-0000-4000-8000-0000000000c2',
       i.environment,
       'fp-rs-a-' || i.n,
       CASE i.n % 4
         WHEN 0 THEN 'payment authorisation timed out #' || i.n
         WHEN 1 THEN 'cart total mismatch #' || i.n
         WHEN 2 THEN 'inventory reservation failed #' || i.n
         ELSE        'context deadline exceeded #' || i.n
       END,
       CASE i.n % 4 WHEN 0 THEN 'TimeoutError' WHEN 1 THEN 'InvariantError' WHEN 2 THEN 'ReservationError' ELSE 'DeadlineExceeded' END,
       'system_error',
       i.status,
       i.first_seen,
       i.last_seen,
       i.count,
       i.first_release_version,
       i.resolved_in_version,
       i.resolved_at,
       CASE WHEN i.status = 'resolved' THEN 'user' ELSE NULL END,
       CASE WHEN i.status = 'resolved' THEN 'releases-snooze-seed-engineer' ELSE NULL END,
       i.snoozed_until,
       i.snooze_occurrence_target,
       i.snooze_count_at
FROM seed_issues_a i;

-- Project B's 300 issues, n = 1..300, all production; first release 1 + n % 40; ~10 resolved in a
-- SHA (n % 30 = 1) that exists as a release.
CREATE TEMP TABLE seed_issues_b ON COMMIT DROP AS
SELECT n,
       ('00000000-0000-4000-8000-00000c3' || lpad(to_hex(n), 5, '0'))::uuid AS id,
       rel.version AS first_release_version,
       rel.first_seen + ((n % 20) * interval '1 hour') AS first_seen,
       LEAST(c.load_time - interval '1 minute',
             GREATEST(rel.first_seen + ((n % 20) * interval '1 hour') + interval '1 hour',
                      c.load_time - ((n % 6) * interval '1 hour') - (((n / 6) % 10) * interval '1 day'))) AS last_seen,
       (1 + (n * 7) % 20)::bigint AS count,
       CASE WHEN n % 30 = 1 THEN 'resolved' ELSE 'unresolved' END AS status,
       CASE WHEN n % 30 = 1 THEN (SELECT version FROM seed_releases_b WHERE seed_releases_b.rn = 1 + (n * 3) % 40) ELSE NULL END AS resolved_in_version,
       CASE WHEN n % 30 = 1 THEN c.load_time - ((n % 10) * interval '1 day') - interval '2 hours' ELSE NULL END AS resolved_at
FROM seed_clock c
CROSS JOIN generate_series(1, 300) AS n
JOIN seed_releases_b rel ON rel.rn = 1 + n % 40;

INSERT INTO issues (id, project_id, environment, fingerprint, message, error_class, issue_type, status,
                    first_seen, last_seen, count, first_release_version,
                    resolved_in_version, resolved_at, resolved_by_type, resolved_by)
SELECT i.id,
       '00000000-0000-4000-8000-0000000000c3',
       'production',
       'fp-rs-b-' || i.n,
       'upstream returned 502 #' || i.n,
       'UpstreamError',
       'system_error',
       i.status,
       i.first_seen,
       i.last_seen,
       i.count,
       i.first_release_version,
       i.resolved_in_version,
       i.resolved_at,
       CASE WHEN i.status = 'resolved' THEN 'user' ELSE NULL END,
       CASE WHEN i.status = 'resolved' THEN 'releases-snooze-seed-engineer' ELSE NULL END
FROM seed_issues_b i;

-- Project C: one unresolved issue with no first release (decision 47; three INSERTs in all).
INSERT INTO issues (id, project_id, environment, fingerprint, message, error_class, issue_type, status,
                    first_seen, last_seen, count, first_release_version)
SELECT '00000000-0000-4000-8000-00000c400001',
       '00000000-0000-4000-8000-0000000000c4',
       'production',
       'fp-rs-c-1',
       'job runner panicked #1',
       'PanicError',
       'system_error',
       'unresolved',
       c.load_time - interval '3 days',
       c.load_time - interval '2 hours',
       4,
       NULL
FROM seed_clock c;

-- One occurrence per issue at its last_seen (see the header for why), carrying the issue's first
-- release — release 250's version for the NULL-only set, NULL where the issue has none, '' in C.
INSERT INTO error_occurrences (issue_id, environment, platform, release_version, created_at)
SELECT i.id, i.environment, 'go', i.occurrence_release, i.last_seen
FROM seed_issues_a i
UNION ALL
SELECT i.id, 'production', 'go', i.first_release_version, i.last_seen
FROM seed_issues_b i
UNION ALL
SELECT '00000000-0000-4000-8000-00000c400001', 'production', 'go', '', c.load_time - interval '2 hours'
FROM seed_clock c;

COMMIT;

ANALYZE releases;
ANALYZE issues;
ANALYZE error_occurrences;
