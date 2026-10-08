-- +goose Up
-- +goose StatementBegin
-- sentinel-reporting-and-dashboard D6: the hourly denominators of the dashboard's SLIs and error rates, copied from
-- VictoriaMetrics by POST /api/cron/sli-collect so Databend (which receives this table through CDC, D2) can divide
-- like by like. One row per (sli, environment, hour): total requests / job runs / NATS messages, how many were
-- errors, and (http only) how many finished inside the latency objective. `fast` is NULL for kinds with no latency
-- objective.
--
-- IDEMPOTENCY: one flat directory serves several goose ledgers against the SAME physical database, so this file is
-- replayed per target. IF NOT EXISTS throughout.
CREATE TABLE IF NOT EXISTS sli_hourly (
    sli_id      TEXT        NOT NULL,
    environment TEXT        NOT NULL,
    hour_start  TIMESTAMPTZ NOT NULL,
    total       BIGINT      NOT NULL,
    errors      BIGINT      NOT NULL,
    fast        BIGINT,
    PRIMARY KEY (sli_id, environment, hour_start)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS sli_hourly;
-- +goose StatementEnd
