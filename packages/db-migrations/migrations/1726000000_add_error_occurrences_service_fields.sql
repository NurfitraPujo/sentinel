-- +goose NO TRANSACTION
-- sentinel-reporting-and-dashboard D18: `service`, `tag_source` and `request_scoped` become first-class columns on
-- error_occurrences, so the dashboard can tell an API error from a worker error without reading JSONB.
--
-- NEVER `source`: error_occurrences.source already exists (1725300000) and is a different field, the wire's top-level
-- capture origin. This file does not add, alter or touch it. `tag_source` is the value copied from tags.source.
--
-- Lock profile (error_occurrences is the hot ingest table). ONE ALTER adds all three columns. service and tag_source are
-- nullable with no default; request_scoped is NOT NULL DEFAULT false, a constant default, which PG 11+ stores in the
-- catalog without a table rewrite. So the ALTER is catalog-only. lock_timeout = '5s' makes it fail fast rather than
-- queue every ingest insert behind it; the next sync re-runs the file.
--
-- Idempotency: one flat directory serves every goose target, each with its own ledger against the SAME database, so this
-- file is replayed. ADD COLUMN IF NOT EXISTS. The batched backfill is its own file (1726000100).

-- +goose Up
-- +goose StatementBegin
SET lock_timeout = '5s';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE error_occurrences
    ADD COLUMN IF NOT EXISTS service TEXT,
    ADD COLUMN IF NOT EXISTS tag_source TEXT,
    ADD COLUMN IF NOT EXISTS request_scoped BOOLEAN NOT NULL DEFAULT false;
-- +goose StatementEnd

-- +goose StatementBegin
RESET lock_timeout;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE error_occurrences
    DROP COLUMN IF EXISTS request_scoped,
    DROP COLUMN IF EXISTS tag_source,
    DROP COLUMN IF EXISTS service;
-- +goose StatementEnd
