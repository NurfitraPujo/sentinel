-- +goose Up
-- +goose StatementBegin
-- sentinel-occurrence-spans TASK-02: pull a trace's spans ON DEMAND (dashboard button, agent API) instead of waiting for
-- the per-issue-throttled automatic pull (1725700000: ~3-4% of traced occurrences ever got spans).
--
-- span_pull_queue gains the on-demand marker, who asked, and the worker's TERMINAL OUTCOME:
--   on_demand          the last (re)arm of this row came from a person/agent, not from MaybeEnqueue (metrics label only;
--                      the worker treats the row exactly like an automatic one: allow-list re-check, size limits, retries)
--   requested_by/kind  that requester: a user id ('user') or an agent id ('agent')
--   requested_at       when
--   outcome            what the worker's last fetch of this row ended in, so the UI can tell "finished with no spans" from
--                      "not pulled yet": '' (nothing finished since the row was queued/re-armed), 'stored' (spans written),
--                      'not_found' (the trace store has no such trace), 'too_large', 'not_allowed' (organization delisted),
--                      'gave_up' (retries exhausted). Free text on purpose: a CHECK would need a pg_constraint guard to stay
--                      replayable, and the only writers are the worker and the dashboard.
ALTER TABLE span_pull_queue ADD COLUMN IF NOT EXISTS on_demand         BOOLEAN     NOT NULL DEFAULT false;
ALTER TABLE span_pull_queue ADD COLUMN IF NOT EXISTS requested_by      TEXT        NOT NULL DEFAULT '';
ALTER TABLE span_pull_queue ADD COLUMN IF NOT EXISTS requested_by_kind TEXT        NOT NULL DEFAULT '';
ALTER TABLE span_pull_queue ADD COLUMN IF NOT EXISTS requested_at      TIMESTAMPTZ;
ALTER TABLE span_pull_queue ADD COLUMN IF NOT EXISTS outcome           TEXT        NOT NULL DEFAULT '';

-- One row per ACCEPTED on-demand request (a request that changed the queue). It is both the audit trail (who asked for
-- which trace, when) and the rate-limit ledger: the dashboard counts an organization's rows of the last hour under a
-- per-organization advisory lock, so the limit holds across dashboard replicas. org_id carries no FK, like spans and
-- span_pull_queue; rows age out with the retention sweep.
--
-- IDEMPOTENCY: this file is replayed (one flat directory, several goose ledgers, A1). Every statement is IF NOT EXISTS.
CREATE TABLE IF NOT EXISTS span_pull_requests (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id            UUID        NOT NULL,
    trace_id          TEXT        NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$'),
    action            TEXT        NOT NULL,
    requested_by      TEXT        NOT NULL,
    requested_by_kind TEXT        NOT NULL CHECK (requested_by_kind IN ('user', 'agent')),
    requested_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_span_pull_requests_org_requested_at ON span_pull_requests (org_id, requested_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS span_pull_requests;
ALTER TABLE span_pull_queue DROP COLUMN IF EXISTS outcome;
ALTER TABLE span_pull_queue DROP COLUMN IF EXISTS requested_at;
ALTER TABLE span_pull_queue DROP COLUMN IF EXISTS requested_by_kind;
ALTER TABLE span_pull_queue DROP COLUMN IF EXISTS requested_by;
ALTER TABLE span_pull_queue DROP COLUMN IF EXISTS on_demand;
-- +goose StatementEnd
