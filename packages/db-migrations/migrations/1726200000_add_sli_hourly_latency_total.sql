-- +goose Up
-- +goose StatementBegin
-- sentinel-latency-excludes-streams D2: the latency ratio's denominator. An http SLI with a `streamSource` counts
-- availability over `source` + `streamSource` (so `total` includes SSE streams) but latency over `source` alone, so
-- `fast / total` no longer holds. `latency_total` is the requests on `source` alone; NULL for jobs/nats and for rows
-- written before this column existed (readers use coalesce(latency_total, total), which keeps their old meaning).
--
-- IDEMPOTENCY: one flat directory serves several goose ledgers against the SAME physical database, so this file is
-- replayed per target. IF NOT EXISTS / IF EXISTS throughout.
ALTER TABLE sli_hourly ADD COLUMN IF NOT EXISTS latency_total BIGINT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sli_hourly DROP COLUMN IF EXISTS latency_total;
-- +goose StatementEnd
