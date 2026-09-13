-- +goose NO TRANSACTION
-- sentinel-issue-trend §4.6, file 2 of 2 — the one-time backfill of issue_occurrence_buckets.
--
-- Decision 14/19: the one-time backfill of issue_occurrence_buckets from error_occurrences, in ITS
-- OWN file after the table's and outside the DDL transaction. Inside it, the SHARE ROW EXCLUSIVE
-- (FK) and SHARE (CREATE INDEX … ON issues) locks the table file takes on `issues` would be held
-- across a full scan of the largest table; every StoreEvent takes ROW EXCLUSIVE on `issues` at its
-- step 1, so ingest on both pods would stall for the scan's duration — and past statement_timeout
-- the whole file would roll back, table included, the migrate Job would fail 3× identically and
-- the ArgoCD sync would fail with no pod rolled. Out here the inserts take FOR KEY SHARE on
-- `issues` rows only (the FK), which does not conflict with StoreEvent's non-key UPDATE, and a
-- timeout fails only this file: the table stays, this version stays unrecorded, the next sync
-- re-runs only this file, and the processor may already write.
-- Zero cost on today's clusters (`issues` empty at v0.2.10) — this statement exists for any
-- database with history: a local stack with load-harness data, an environment seeded from a dump.
-- WHERE NOT EXISTS: three ledgers replay this directory (hazard sentinel_goose_target); the second
-- and third find rows — the first ledger's backfill, or the processor's own writes — and skip the
-- scan. ON CONFLICT DO NOTHING: an hour the new processor has already bucketed keeps its live
-- count. Idempotent; the same statement WITHOUT the NOT EXISTS guard is the RUNBOOK one-liner
-- (deploy/helm/sentinel/RUNBOOK.md §3).
--
-- `date_trunc('hour', created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'` is the SQL spelling of
-- §3's Go rule (`occ.CreatedAt.UTC().Truncate(time.Hour)`): the inner AT TIME ZONE 'UTC' turns the
-- timestamptz into a UTC wall-clock timestamp, date_trunc truncates THAT (the session's TimeZone is
-- irrelevant), and the outer one turns it back. A bare date_trunc('hour', timestamptz) truncates in
-- the session zone, which for +05:30 / +05:45 lands in a different hour than the processor's.
--
-- Outside a transaction SET is session-scoped and SET LOCAL would be a no-op, so this file RESETs
-- what it set; every other migration sets its own values anyway, but the ledger should not rely
-- on that. goose runs a NO TRANSACTION file's statements one at a time through database/sql,
-- which hands a single sequential caller its one idle connection back each time, so the SET the
-- first block makes is the session the INSERT runs in.

-- +goose Up
-- +goose StatementBegin
SET lock_timeout = '5s';
SET statement_timeout = '15min';
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO issue_occurrence_buckets (issue_id, bucket_start, count)
SELECT issue_id,
       date_trunc('hour', created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
       count(*)
  FROM error_occurrences
 WHERE NOT EXISTS (SELECT 1 FROM issue_occurrence_buckets)
 GROUP BY 1, 2
ON CONFLICT (issue_id, bucket_start) DO NOTHING;
-- +goose StatementEnd
-- +goose StatementBegin
RESET lock_timeout;
RESET statement_timeout;
-- +goose StatementEnd

-- +goose Down
-- Nothing to undo: the rows belong to the table, and the table's own migration
-- (1724700000_add_issue_occurrence_buckets.sql) drops them with it. goose v3 accepts an empty
-- Down section (zero statements, the version row is still deleted), so no SELECT 1 no-op is needed.
