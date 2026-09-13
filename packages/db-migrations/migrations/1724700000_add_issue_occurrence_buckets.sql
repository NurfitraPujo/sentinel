-- sentinel-issue-trend §4.6, file 1 of 2 — the per-(issue, UTC hour) occurrence bucket table.
--
-- One row per (issue_id, bucket_start) that saw at least one occurrence: the processor upserts it
-- inside StoreEvent's transaction (§4.3, decision 1), the two dashboard pages read it as dense
-- 24-hour and 30-day series (§4.1, §4.2), and the retention sweep ages it out on
-- `bucket_start < cutoff` (§4.5) — the same clock as error_occurrences.created_at, because
-- bucket_start IS the occurrence's event timestamp truncated to the UTC hour (§3, decision 2),
-- never now(). No column is added to `issues` (decision 3): the series is always read from here.
--
-- DDL ONLY. The one-time backfill from error_occurrences is the NEXT file
-- (1724800000_backfill_issue_occurrence_buckets.sql), outside any transaction, for the reason its
-- header gives (decision 19): the locks this file takes on `issues` must not be held across a full
-- scan of the largest table.
--
-- IF NOT EXISTS throughout: same re-runnability rationale as every migration since
-- 1722000000_add_api_key_management.sql — one flat migrations directory serves several goose ledgers
-- against the SAME physical database (A1, hazard sentinel_goose_target), so every statement here is
-- replayed per target. Nothing needs a pg_constraint guard: the CHECK and the composite PRIMARY KEY
-- are inline on CREATE TABLE IF NOT EXISTS, so they exist exactly when the table does.

-- +goose Up
-- +goose StatementBegin
-- Same values and reasoning as 1724500000/1724600000: the FK takes SHARE ROW EXCLUSIVE and the two
-- CREATE INDEX … ON issues take SHARE on `issues`, the table every ingest writes to — fail fast
-- and visibly rather than stall ingest. NOTHING ELSE runs in this transaction; the backfill is the
-- next file, outside any transaction, for the reason its header gives.
SET lock_timeout = '5s';
SET statement_timeout = '5min';
-- +goose StatementEnd

-- +goose StatementBegin
-- PRIMARY KEY (issue_id, bucket_start) is the ON CONFLICT target of §4.3's upsert and the range
-- scan both readers use (`issue_id = … AND bucket_start >= …`); it also serves the cascade.
-- CHECK (count > 0) is the tripwire: a row exists because at least one occurrence did.
-- TIMESTAMPTZ so the `< cutoff` comparison is the same comparison retention makes on
-- error_occurrences.created_at.
CREATE TABLE IF NOT EXISTS issue_occurrence_buckets (
    issue_id     UUID        NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    bucket_start TIMESTAMPTZ NOT NULL,
    count        INTEGER     NOT NULL CHECK (count > 0),
    PRIMARY KEY (issue_id, bucket_start)
);
-- §4.5's sweep (`WHERE bucket_start < cutoff`), otherwise a sequential scan every hour.
CREATE INDEX IF NOT EXISTS idx_issue_occurrence_buckets_bucket_start
    ON issue_occurrence_buckets (bucket_start);
-- +goose StatementEnd

-- +goose StatementBegin
-- §4.1's two new orders under the existing project (and project+environment) predicate. BOTH SHIP
-- AS WRITTEN (decision 21). `count` changes on EVERY occurrence, so idx_issues_project_count is
-- maintained on every StoreEvent; `first_seen` never changes after insert. §13 M5's with/without
-- plans are the evidence for a follow-up item PLANNING names (repo: sentinel) that strikes
-- idx_issues_project_count if its read delta does not justify that write cost. Its own block, so
-- striking it is one file — the BUG-01 precedent.
CREATE INDEX IF NOT EXISTS idx_issues_project_count
    ON issues (project_id, count DESC, last_seen DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_issues_project_first_seen
    ON issues (project_id, first_seen DESC NULLS LAST, id DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SET lock_timeout = '5s';
SET statement_timeout = '5min';
DROP INDEX IF EXISTS idx_issues_project_first_seen;
DROP INDEX IF EXISTS idx_issues_project_count;
DROP INDEX IF EXISTS idx_issue_occurrence_buckets_bucket_start;
DROP TABLE IF EXISTS issue_occurrence_buckets;
-- +goose StatementEnd
