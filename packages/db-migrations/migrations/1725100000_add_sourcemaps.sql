-- sentinel-sourcemaps §4.5 -- the `sourcemaps` table: one row per (project, release, minified
-- path) map that `POST /api/sourcemaps` (§4.1) has stored, read by the processor's symbolication
-- lookup (§4.4) on the same three columns. One file, one table, no column on `issues` or
-- `error_occurrences`, no backfill.
--
-- NO LOCK GUARD, and why: every earlier migration that carries `SET lock_timeout = '5s'` does so
-- because it ALTERs or indexes `issues` / `error_occurrences` -- the tables every ingest writes
-- to -- and a stalled ACCESS EXCLUSIVE there is an ingest outage. This file only CREATEs a table
-- nothing yet references and two indexes on that new table: no lock is taken on any existing
-- table, so there is nothing for a guard to protect and none is copied in (contract §4.5, *Lock
-- profile*). The FK to `projects` takes a SHARE ROW EXCLUSIVE lock on `projects` for the length
-- of the CREATE TABLE, which is a catalog-sized moment, not an ingest-path wait.
--
-- IF NOT EXISTS throughout, constraints inline on CREATE TABLE IF NOT EXISTS: same re-runnability
-- rationale as every migration since 1722000000_add_api_key_management.sql -- one flat migrations
-- directory serves several goose ledgers against the SAME physical database (A1, hazard
-- sentinel_goose_target), so every statement here is replayed per target. The inline CHECKs, the
-- composite UNIQUE and the FK exist exactly when the table does, so no pg_constraint guard is
-- needed either.

-- +goose Up
-- +goose StatementBegin
-- UNIQUE (project_id, release_version, minified_path) is §4.1's upsert target and §4.4's lookup.
-- VARCHAR(100) / VARCHAR(512) are the bounds the ingest path already enforces on the same
-- strings (error_occurrences.release_version, MaxFilePathLength). The two CHECK (<> '') are
-- tripwires: the route rejects an empty release_version (§4.1, stored byte for byte -- decision
-- 25) and normalises minified_path to a non-empty path (decision 22) BEFORE the statement, and the
-- constraint makes a regression of that loud instead of a phantom release named "".
--
-- NO FK to `releases`: a map is uploaded BEFORE the first occurrence of its version exists (CI
-- uploads at build time; `releases` rows are created by StoreEvent), so the FK would refuse the
-- normal case. ON DELETE CASCADE from `projects` deletes rows; the objects stay -- nothing walks
-- the bucket (§4.1 *Orphan objects*; retention of maps is decision 12's follow-on).
--
-- sha256 is CHAR(64): the hex digest of the stored bytes, verified by the processor after the
-- object GET (§4.4 step 4) and compared by the route to decide `unchanged` vs `replaced` (§4.1).
-- size_bytes > 0: zero bytes cannot pass §4.1's structural sniff (`"version": 3`, a string
-- `mappings`, an array `sources`), so a 0 here can only be a write path that lost the count.
CREATE TABLE IF NOT EXISTS sourcemaps (
    id               UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id       UUID          NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    release_version  VARCHAR(100)  NOT NULL CHECK (release_version <> ''),
    minified_path    VARCHAR(512)  NOT NULL CHECK (minified_path <> ''),
    storage_key      VARCHAR(1024) NOT NULL,
    size_bytes       BIGINT        NOT NULL CHECK (size_bytes > 0),
    sha256           CHAR(64)      NOT NULL,
    uploaded_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, release_version, minified_path)
);
-- storage_key UNIQUE, like attachments.storage_key and for the same reason: each row owns exactly
-- one object, so a `replaced` cleanup is a plain delete of the old key. It holds because the key
-- folds the minified path in (decision 17: `…/{h16(minified_path)}-{sha256}.map`) -- two rows of
-- one release with identical bytes under two paths are two objects, never one shared object this
-- index would refuse.
CREATE UNIQUE INDEX IF NOT EXISTS idx_sourcemaps_storage_key ON sourcemaps (storage_key);
-- §4.2's list (`WHERE project_id = … AND release_version = …`) and §4.6's grouped count per
-- release; the composite UNIQUE above already serves the three-column point lookup.
CREATE INDEX IF NOT EXISTS idx_sourcemaps_project_release ON sourcemaps (project_id, release_version);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_sourcemaps_project_release;
DROP INDEX IF EXISTS idx_sourcemaps_storage_key;
DROP TABLE IF EXISTS sourcemaps;
-- +goose StatementEnd
