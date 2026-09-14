-- +goose NO TRANSACTION
-- sentinel-releases-snooze §4.5, file 2 of 2 — the one-time backfill of `releases` and of
-- `issues.first_release_version` from retained occurrences (decisions 22, 31).
--
-- Decision 22: the backfill from error_occurrences, in ITS OWN file after the DDL's and outside
-- the DDL transaction -- the 1724800000_backfill_issue_occurrence_buckets.sql shape exactly. Inside
-- file 1 the ACCESS EXCLUSIVE locks its ADD COLUMN / ADD CONSTRAINT take on `issues` would be held
-- across a full scan of the largest table; every StoreEvent takes ROW EXCLUSIVE on `issues` at its
-- step 1, so ingest on both pods would stall for the scan's duration -- and past statement_timeout
-- the whole file would roll back, table and columns included, the migrate Job would fail 3×
-- identically and the ArgoCD sync would fail with no pod rolled. Out here each statement is its own
-- transaction and a timeout fails only this file: the schema stays, this version stays unrecorded,
-- the next sync re-runs only this file, and the processor may already write.
--
-- Two statements, each guarded. The INSERT builds `releases` from every retained versioned
-- occurrence: (project, version) → earliest created_at, latest created_at, count — each clamped
-- to LEAST(…, now()) (decision 50, `release-first-seen-clock-guard`, the backfill half of the
-- clamp step 4c applies at write). A retained occurrence whose client clock ran ahead would
-- otherwise seed a permanent "newest" release whose first_seen sits in the future: every `next`
-- resolution of the project is rewritten to it and a genuine later release reads as older, so
-- regression detection is silently off until the clock catches up — and `releases` is never
-- swept (§12). now() is the statement's transaction start, so the clamp is one instant across the
-- whole scan and reads as "no later than this backfill". A past clock still yields an "oldest"
-- row, which is harmless, so only the future side is clamped. The UPDATE sets
-- first_release_version from the EARLIEST retained occurrence of each issue REGARDLESS of version
-- and keeps its version only if it has one (NULLIF(release_version, '')): an issue whose creating
-- occurrence carried no version reads `—` as §4.1 says it must, and is never attributed to a later
-- release that happened to be the first VERSIONED one (decision 31).
--
-- THE UPDATE TAKES A ROW LOCK ON EVERY ISSUE IT REWRITES -- every issue with a retained versioned
-- occurrence -- while the old-tag processor pods, still running at ArgoCD wave 1 (the migrate Job
-- runs before any pod rolls, §8), take the same rows at StoreEvent step 1. Bounded (30-day
-- retention; empty at v0.2.10), and unlike the 1724800000 precedent, which inserted into a side
-- table and took only FOR KEY SHARE on `issues`. Under lock_timeout = '5s' a busy row can fail the
-- statement: the migrate Job fails, the sync stalls at wave 1, and the old pods keep running --
-- safe; re-syncing re-runs the file, and because each statement is its own transaction the NOT
-- EXISTS guards skip what the first attempt finished (a failed UPDATE set nothing, so its guard
-- still passes). Decision 42; the §12 row; the migration test's comment says the same.
--
-- Backfilled first_seen is exact only inside the retention window: a release whose earliest
-- occurrences were swept before this file ran reads the earliest RETAINED one, which is later than
-- the truth, and every ordering that reads first_seen (decisions 1, 6) inherits that for
-- pre-feature releases. Stated in the release note (deploy/helm/sentinel/RUNBOOK.md §3).
--
-- Zero cost on today's clusters (`error_occurrences` empty at v0.2.10) -- both statements exist for
-- any database with history: a local stack with load-harness data, an environment seeded from a
-- dump. WHERE NOT EXISTS on BOTH statements: three ledgers replay this directory (hazard
-- sentinel_goose_target); the second and third find rows -- the first ledger's backfill, or the
-- processor's own writes -- and skip the scan of error_occurrences. ON CONFLICT DO NOTHING: a
-- release the new processor has already written keeps its live counts. Idempotent; a goose version
-- like every other, so it never re-runs for occurrences stored by old-tag pods during the rollout's
-- last minutes or a rollback (§8; the RUNBOOK says so).
--
-- Outside a transaction SET is session-scoped and SET LOCAL would be a no-op, so this file RESETs
-- what it set; every other migration sets its own values anyway, but the ledger should not rely
-- on that. goose runs a NO TRANSACTION file's statements one at a time through database/sql,
-- which hands a single sequential caller its one idle connection back each time, so the SET the
-- first block makes is the session the INSERT and the UPDATE run in.

-- +goose Up
-- +goose StatementBegin
SET lock_timeout = '5s';
SET statement_timeout = '15min';
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO releases (project_id, version, first_seen, last_seen, occurrence_count)
SELECT i.project_id, o.release_version,
       LEAST(MIN(o.created_at), now()), LEAST(MAX(o.created_at), now()), COUNT(*)
  FROM error_occurrences o JOIN issues i ON i.id = o.issue_id
 WHERE o.release_version IS NOT NULL AND o.release_version <> ''
   AND NOT EXISTS (SELECT 1 FROM releases)
 GROUP BY i.project_id, o.release_version
ON CONFLICT (project_id, version) DO NOTHING;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issues i SET first_release_version = f.release_version
  FROM (SELECT DISTINCT ON (issue_id) issue_id, NULLIF(release_version, '') AS release_version
          FROM error_occurrences
         WHERE NOT EXISTS (SELECT 1 FROM issues WHERE first_release_version IS NOT NULL)
         ORDER BY issue_id, created_at ASC) f
 WHERE f.issue_id = i.id AND i.first_release_version IS NULL AND f.release_version IS NOT NULL;
-- +goose StatementEnd
-- +goose StatementBegin
RESET lock_timeout;
RESET statement_timeout;
-- +goose StatementEnd

-- +goose Down
-- Nothing to undo: the rows belong to the table and the columns, and file 1's own Down
-- (1724900000_add_releases_and_issue_snooze.sql) drops them with it (decision 31). goose v3 accepts
-- an empty Down section (zero statements, the version row is still deleted), so no SELECT 1 no-op
-- is needed.
