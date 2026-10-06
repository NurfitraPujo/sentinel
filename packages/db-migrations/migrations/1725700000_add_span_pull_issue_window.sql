-- +goose Up
-- +goose StatementBegin
-- sentinel-aws-error-reporting TASK-02: at most ONE span pull per issue per SPAN_PULL_ISSUE_WINDOW (default 10m).
-- span_pull_queue dedupes only by (org_id, trace_id), so one noisy error whose every occurrence carries its own trace
-- (~100/min on 2026-10-06) queued 1,806 pulls of ~7,000-span traces in one hour. One row per issue that has queued a
-- pull: the processor's single INSERT ... ON CONFLICT DO UPDATE ... WHERE last_enqueued_at <= now - window
-- RETURNING both decides and records, atomically across processor replicas (spanpull.Queue.MaybeEnqueue).
--
-- A table of its own rather than a column on issues: the processor already updates the issues row on every event,
-- and a second write to it here would add lock contention on the hottest row of the ingest path. The rows cascade
-- with their issue; one row per erroring issue keeps the table small.
--
-- IDEMPOTENCY: one flat directory serves several goose ledgers against the SAME physical database (A1), so this file
-- is replayed. Every statement is IF NOT EXISTS.
CREATE TABLE IF NOT EXISTS span_pull_issue_window (
    issue_id         UUID        PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    last_enqueued_at TIMESTAMPTZ NOT NULL
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS span_pull_issue_window;
-- +goose StatementEnd
