-- +goose NO TRANSACTION
-- sentinel-occurrence-dedup-project §4.2 -- `error_occurrences.project_id` and the unique index
-- that widens the occurrence dedup key from (issue_id, event_id) to (project_id, event_id),
-- built CONCURRENTLY: the first concurrent build in this repository, on the table every ingest
-- writes to (769,590 rows / 1421 MB on production, decision 23). The old index
-- `uq_error_occurrences_issue_event` is NOT dropped here (decision 5; a later release's file).
--
-- The no-transaction directive is line 1 and appears nowhere else in this file, not even quoted:
-- goose scans EVERY comment line for its annotation marker, and 1722200000's header records that
-- file shipping unparseable once for exactly that reason (contract §3). Each StatementBegin /
-- StatementEnd block below is one `database/sql` Exec in autocommit, which is what CREATE INDEX
-- CONCURRENTLY and the per-batch COMMIT of the backfill both require. Outside a transaction a
-- SET is session-scoped: the SETs here rely on goose handing one sequential caller its single
-- pooled connection back for every block (the 1724800000 precedent relies on the same), so the
-- session block (c) asserts is the session block (a) configured. Gate (c) checks
-- current_setting('statement_timeout') by name so a lost session setting fails loudly rather
-- than letting the backfill or the build run unbounded (decision 16).
--
-- Blocks, in order, and why the order (contract §4.2 "Why the guard order"):
--   lock_timeout = '5s' + statement_timeout = '15min' -- the session's guards (decision 16).
--   (a) ADD COLUMN IF NOT EXISTS project_id UUID -- nullable, no default, no FK (decision 3):
--       catalog-only under ACCESS EXCLUSIVE for milliseconds; lock_timeout fails it fast if an
--       ingest holds the table, and the next sync re-runs the file.
--   (b) the backfill -- project_id := issues.project_id via issue_id, WHERE project_id IS NULL, in
--       batches of 10,000 rows by id range (decision 20): each batch is `the next 10,000 rows
--       with project_id IS NULL ordered by id`, one UPDATE over that id range, one COMMIT. Only
--       row locks on the rows rewritten; a concurrent StoreEvent inserts new rows, so ingest is
--       not stalled. Idempotent and resumable: a statement_timeout mid-loop keeps every
--       committed batch and the re-run continues from the remaining NULLs; the second and third
--       ledgers find no NULL row and exit after one index scan. statement_timeout = '15min'
--       bounds the whole DO block; the per-batch duration at >= 1,000,000 rows is §13 M2's
--       measurement and is recorded in the feature's status/be.md, not here.
--   (c) the duplicate gate -- RAISE EXCEPTION naming the number of (project_id, event_id) groups
--       with more than one row, on the index's own predicate and grouped on the COLUMN, not via
--       the issues join (decision 21). A unique CONCURRENTLY build over a violated key fails
--       AFTER its scan and leaves an INVALID index behind; the gate fails by name BEFORE any
--       index is touched. Nothing here deletes a row (decision 4): the RUNBOOK recipe (O2) does,
--       run by an operator with the number in front of them, and then the sync is re-run.
--   (d) drop-if-invalid -- a previous run's failed concurrent build (a violation the gate could
--       not see, a killed Job, a timeout) leaves uq_error_occurrences_project_event EXISTING with
--       pg_index.indisvalid = false; IF NOT EXISTS would then skip the build and the key would
--       never be enforced, silently, on every ledger that replays this file. DROP INDEX
--       CONCURRENTLY cannot run inside a function body, and there is no top-level IF in SQL, so
--       the guard is a DO block that RENAMES the index when, and only when, it is invalid, and
--       the drop is a top-level `DROP INDEX CONCURRENTLY IF EXISTS` of the renamed index (decision
--       21: every DROP INDEX in this file is CONCURRENTLY). A valid index keeps its name and its
--       OID and is never dropped. The same DROP runs once before the rename as well, so a
--       leftover from a concurrent drop that itself failed can never block the rename.
--   lock_timeout = 0 -- immediately before the build and never in force for it (decision 16):
--       CONCURRENTLY waits at its two scan points for older transactions to end, and a 5s
--       lock_timeout would fail a build that is merely waiting for a slow StoreEvent.
--   CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS -- SHARE UPDATE EXCLUSIVE; inserts continue.
--   (e) the indisvalid check -- RAISE by name if the index is missing or not valid, so a build
--       that failed leaves the Job red and the ledger unadvanced; the next run's (d) drops it.
--   RESET both timeouts -- the ledger must not rely on the next file setting its own.
--
-- Re-runnable at every block: a failure leaves everything before it applied and the ledger
-- unadvanced, and the next sync re-runs the file top to bottom. Three ledgers replay it (hazard
-- sentinel_goose_target): the second and third find the column, a backfill with nothing left to
-- do, zero groups, a valid index (untouched), IF NOT EXISTS skips the build, (e) passes.
--
-- Deploy order (§8): this file before any processor build that writes project_id, which the
-- migrate Job (ArgoCD wave 1) and compose's `depends_on: migrate` already enforce. Down is paired
-- with a processor rollback FIRST (a new-tag processor against a database that ran Down gets
-- 42703 on project_id -- retryable, then dead-lettered); RUNBOOK: processor tag first, Down second.

-- +goose Up
-- +goose StatementBegin
SET statement_timeout = '15min';
SET lock_timeout = '5s';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE error_occurrences ADD COLUMN IF NOT EXISTS project_id UUID;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE
    batch_size CONSTANT integer := 10000;
    last_id uuid := NULL;
    hi uuid;
BEGIN
    LOOP
        -- The next batch: the first batch_size rows still NULL past the previous batch's upper
        -- bound, by id (the primary key, so this is one index range scan); hi is its upper
        -- bound (there is no max(uuid), hence the ordered LIMIT 1).
        SELECT b.id INTO hi
          FROM (SELECT id FROM error_occurrences
                 WHERE (last_id IS NULL OR id > last_id) AND project_id IS NULL
                 ORDER BY id
                 LIMIT batch_size) b
         ORDER BY b.id DESC
         LIMIT 1;
        EXIT WHEN hi IS NULL;
        UPDATE error_occurrences o
           SET project_id = i.project_id
          FROM issues i
         WHERE i.id = o.issue_id
           AND (last_id IS NULL OR o.id > last_id) AND o.id <= hi
           AND o.project_id IS NULL;
        COMMIT;
        last_id := hi;
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE
    st text := current_setting('statement_timeout');
    groups bigint;
BEGIN
    IF st <> '15min' THEN
        RAISE EXCEPTION '1725200000 gate (c): statement_timeout is % in this session, want ''15min'' -- the SET of the first block did not reach this block (decision 16)', st;
    END IF;
    SELECT count(*) INTO groups
      FROM (SELECT 1
              FROM error_occurrences
             WHERE project_id IS NOT NULL AND event_id IS NOT NULL
             GROUP BY project_id, event_id
            HAVING count(*) > 1) g;
    IF groups > 0 THEN
        RAISE EXCEPTION '1725200000 gate (c): % (project_id, event_id) group(s) in error_occurrences have more than one row; uq_error_occurrences_project_event cannot be built until the RUNBOOK recipe (O2) has handled them -- no index was touched', groups;
    END IF;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS uq_error_occurrences_project_event_invalid;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$
BEGIN
    -- to_regclass is NULL when no index of that name exists: nothing to rename, the build
    -- below creates it.
    IF EXISTS (
        SELECT 1 FROM pg_index
         WHERE indexrelid = to_regclass('uq_error_occurrences_project_event')
           AND NOT indisvalid
    ) THEN
        ALTER INDEX uq_error_occurrences_project_event RENAME TO uq_error_occurrences_project_event_invalid;
    END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS uq_error_occurrences_project_event_invalid;
-- +goose StatementEnd

-- +goose StatementBegin
SET lock_timeout = 0;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_error_occurrences_project_event
    ON error_occurrences (project_id, event_id)
    WHERE project_id IS NOT NULL AND event_id IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE
    valid boolean;
BEGIN
    SELECT indisvalid INTO valid FROM pg_index
     WHERE indexrelid = to_regclass('uq_error_occurrences_project_event');
    IF valid IS NULL THEN
        RAISE EXCEPTION '1725200000 check (e): uq_error_occurrences_project_event does not exist after the build';
    END IF;
    IF NOT valid THEN
        RAISE EXCEPTION '1725200000 check (e): uq_error_occurrences_project_event exists but pg_index.indisvalid = false -- the concurrent build failed and the key is NOT enforced; the next run drops and rebuilds it';
    END IF;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
RESET lock_timeout;
RESET statement_timeout;
-- +goose StatementEnd

-- +goose Down
-- The index first (concurrently, outside a transaction, same reason as the build), then the
-- column. The column drop takes ACCESS EXCLUSIVE for a catalog moment and drops any index still
-- on the column with it. The processor is rolled back BEFORE this runs (§8, RUNBOOK).
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS uq_error_occurrences_project_event;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS uq_error_occurrences_project_event_invalid;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE error_occurrences DROP COLUMN IF EXISTS project_id;
-- +goose StatementEnd
