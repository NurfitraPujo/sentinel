-- sentinel-releases-snooze §4.5, file 1 of 2 — the `releases` table, the four snooze / release
-- columns on `issues`, their three CHECKs and the two partial indexes.
--
-- `releases` is one row per (project, version) the processor has seen an occurrence for: it is
-- upserted inside StoreEvent's transaction (§4.3, decision 5), read by the detail page's picker and
-- the releases page (§4.1, §4.4), and ordered by `first_seen`, never by parsing the version
-- (decision 1) -- which is what idx_releases_project_first_seen serves. ON DELETE CASCADE from
-- `projects` only: a release outlives every occurrence that carried it (§12).
--
-- The four `issues` columns are nullable with NO DEFAULT: NULL is the correct value for every
-- pre-feature row and every write path that does not know about them (the dashboard's manual-issue
-- inserts write no release and no snooze). A snooze is `status = 'ignored'` with a condition; there
-- is no fourth status and check_status is untouched (decision 2). The issue_activity event_type
-- CHECK is untouched too (decision 18: a snooze ending is a status_changed row).
--
-- DDL ONLY. The one-time backfill of `releases` and `first_release_version` from error_occurrences
-- is the NEXT file (1725000000_backfill_releases_and_first_release_version.sql), outside any
-- transaction, for the reason its header gives (decision 22).
--
-- IF NOT EXISTS throughout, and a pg_constraint guard on conname for the three ADD CONSTRAINTs
-- (which have no IF NOT EXISTS form): same re-runnability rationale as every migration since
-- 1722000000_add_api_key_management.sql -- one flat migrations directory serves several goose
-- ledgers against the SAME physical database (A1, hazard sentinel_goose_target), so every statement
-- here is replayed per target. The CHECK and the composite PRIMARY KEY on `releases` are inline on
-- CREATE TABLE IF NOT EXISTS, so they exist exactly when the table does.

-- +goose Up
-- +goose StatementBegin
-- Same values and reasoning as 1724500000/1724700000: ADD COLUMN and ADD CONSTRAINT take ACCESS
-- EXCLUSIVE on `issues`, the table every ingest writes to, and the two CREATE INDEX … ON issues take
-- SHARE -- fail fast and visibly rather than stall ingest. Everything below runs in this one
-- transaction under this guard; the backfill is the next file, outside any transaction.
SET lock_timeout = '5s';
SET statement_timeout = '5min';
-- +goose StatementEnd

-- +goose StatementBegin
-- PRIMARY KEY (project_id, version) is the ON CONFLICT target of §4.3's upsert and the lookup both
-- readers make. CHECK (version <> '') is the tripwire: the processor maps an empty version to
-- "absent" BEFORE the statement (§1's empty-version rules, decision 12), and the constraint makes a
-- regression of that loud instead of a phantom release named "". VARCHAR(100) matches
-- error_occurrences.release_version and issues.resolved_in_version, the columns these values were
-- always meant for. TIMESTAMPTZ: first_seen/last_seen are the occurrence's event timestamp
-- (decision 7), the same clock as error_occurrences.created_at.
CREATE TABLE IF NOT EXISTS releases (
    project_id        UUID         NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    version           VARCHAR(100) NOT NULL CHECK (version <> ''),
    first_seen        TIMESTAMPTZ  NOT NULL,
    last_seen         TIMESTAMPTZ  NOT NULL,
    occurrence_count  BIGINT       NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, version)
);
-- §4.1's picker (the 50 newest) and §4.4's page (the 200 most recent), both ORDER BY first_seen
-- DESC under a project predicate (decisions 1, 24).
CREATE INDEX IF NOT EXISTS idx_releases_project_first_seen ON releases (project_id, first_seen DESC);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE issues
  ADD COLUMN IF NOT EXISTS snoozed_until            TIMESTAMPTZ  NULL,
  ADD COLUMN IF NOT EXISTS snooze_occurrence_target BIGINT       NULL,
  ADD COLUMN IF NOT EXISTS snooze_count_at          BIGINT       NULL,
  ADD COLUMN IF NOT EXISTS first_release_version    VARCHAR(100) NULL;
-- +goose StatementEnd

-- +goose StatementBegin
-- Three CHECKs, every one added NOT VALID and then VALIDATEd (decision 31; the rationale is
-- corrected by decision 48). This file is a transactional goose file, so the ACCESS EXCLUSIVE
-- locks that ADD COLUMN and ADD CONSTRAINT take are held to COMMIT whatever follows them: the
-- VALIDATEs below scan while ingest is already blocked, and NOT VALID buys no lock relief here
-- (it would only in a NO TRANSACTION file, where VALIDATE's SHARE UPDATE EXCLUSIVE lets ingest's
-- ROW EXCLUSIVE through). What it buys in this file is the replay guard -- VALIDATE runs only
-- where convalidated is false, so the three ledgers replay the block as a no-op -- and the scan
-- costs ≈ 0 today: every pre-feature row has all four columns NULL and issues is small. All
-- three sit inside the same lock_timeout guard as the two indexes below (the 1724700000 shape).
-- Guarded by a pg_constraint lookup on conname.
-- The target and its baseline travel together, and a snooze exists only on an ignored issue:
-- tripwires that make a write path which forgets one of them fail loudly. check_snooze_only_when_
-- ignored is enforced on every UPDATE, so the resolve/unresolve arms of updateIssueStatus and the
-- regression arm of StoreEvent must null the snooze columns IN THE SAME STATEMENT that changes
-- status (§4.2, §4.3) -- and it is why the rollback recipe nulls them before rolling images back
-- (decision 37, deploy/helm/sentinel/RUNBOOK.md §3).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'check_snooze_target_positive' AND conrelid = 'issues'::regclass
    ) THEN
        ALTER TABLE issues ADD CONSTRAINT check_snooze_target_positive
            CHECK (snooze_occurrence_target > 0) NOT VALID;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'check_snooze_pair' AND conrelid = 'issues'::regclass
    ) THEN
        ALTER TABLE issues ADD CONSTRAINT check_snooze_pair
            CHECK ((snooze_occurrence_target IS NULL) = (snooze_count_at IS NULL)) NOT VALID;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'check_snooze_only_when_ignored' AND conrelid = 'issues'::regclass
    ) THEN
        ALTER TABLE issues ADD CONSTRAINT check_snooze_only_when_ignored
            CHECK (status = 'ignored' OR (snoozed_until IS NULL AND snooze_occurrence_target IS NULL)) NOT VALID;
    END IF;
    -- VALIDATE only where convalidated is false: the replay guard decision 48 keeps NOT VALID for.
    IF EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'check_snooze_target_positive' AND conrelid = 'issues'::regclass AND NOT convalidated
    ) THEN
        ALTER TABLE issues VALIDATE CONSTRAINT check_snooze_target_positive;
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'check_snooze_pair' AND conrelid = 'issues'::regclass AND NOT convalidated
    ) THEN
        ALTER TABLE issues VALIDATE CONSTRAINT check_snooze_pair;
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'check_snooze_only_when_ignored' AND conrelid = 'issues'::regclass AND NOT convalidated
    ) THEN
        ALTER TABLE issues VALIDATE CONSTRAINT check_snooze_only_when_ignored;
    END IF;
END $$;

-- §4.4's "issues introduced" count (`WHERE project_id = … AND first_release_version = …`, grouped
-- per version) and §4.3's `next` rewrite / retention's hourly catch-up (`WHERE project_id = … AND
-- resolved_in_version = 'next'`, decisions 15 and 30). Both partial: the predicate each reader
-- carries is the predicate the index carries, so pre-feature rows (all NULL) cost nothing.
CREATE INDEX IF NOT EXISTS idx_issues_project_first_release
  ON issues (project_id, first_release_version) WHERE first_release_version IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_issues_project_resolved_next
  ON issues (project_id) WHERE resolved_in_version = 'next';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SET lock_timeout = '5s';
SET statement_timeout = '5min';
DROP INDEX IF EXISTS idx_issues_project_resolved_next;
DROP INDEX IF EXISTS idx_issues_project_first_release;
ALTER TABLE issues DROP CONSTRAINT IF EXISTS check_snooze_only_when_ignored;
ALTER TABLE issues DROP CONSTRAINT IF EXISTS check_snooze_pair;
ALTER TABLE issues DROP CONSTRAINT IF EXISTS check_snooze_target_positive;
ALTER TABLE issues
  DROP COLUMN IF EXISTS first_release_version,
  DROP COLUMN IF EXISTS snooze_count_at,
  DROP COLUMN IF EXISTS snooze_occurrence_target,
  DROP COLUMN IF EXISTS snoozed_until;
DROP INDEX IF EXISTS idx_releases_project_first_seen;
DROP TABLE IF EXISTS releases;
-- +goose StatementEnd
