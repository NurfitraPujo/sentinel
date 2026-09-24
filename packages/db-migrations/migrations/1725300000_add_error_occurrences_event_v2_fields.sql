-- +goose NO TRANSACTION
-- sentinel-event-schema-v2 §5 (TASK-04) -- the 17 per-occurrence columns that store the v2 event
-- fields (Sentry-parity metadata), and a partial GIN index on `tags` for Phase 2 facets. Spec:
-- sentinel-event-schema-v2/spec.md §5.1-§5.7; the Drizzle mirror is
-- apps/dashboard-web/src/lib/db/schema.ts `errorOccurrences` (hand-maintained; this file is the
-- source of truth).
--
-- The no-transaction directive is line 1 and appears nowhere else in this file, not even quoted:
-- goose scans every comment line for its annotation marker (the 1722200000 / 1725200000 headers
-- record why). Every statement and every DO body below sits in its own StatementBegin /
-- StatementEnd pair: a DO body contains semicolons, and goose would otherwise split it at the first
-- one. Each pair is one autocommit Exec, which CREATE INDEX CONCURRENTLY requires.
--
-- Lock profile (error_occurrences is the hot ingest table: 769,590 rows / 1421 MB on production
-- when 1725200000 was written):
--   * ONE ALTER TABLE adds all 17 columns, every one nullable, with no default and no CHECK, so it
--     is catalog-only: ACCESS EXCLUSIVE for milliseconds, no rewrite, no validating scan. The
--     value sets are enforced by the ingestor's bounds step and by the proto's CEL rules already.
--     lock_timeout = '5s' makes the ALTER fail fast rather than queue every ingest insert behind
--     it; the next sync re-runs the file.
--   * the GIN index is built CONCURRENTLY (SHARE UPDATE EXCLUSIVE; inserts continue), with
--     lock_timeout = 0 for the build, because CONCURRENTLY waits for older transactions at its two
--     scan points and a 5s lock_timeout would fail a build that is merely waiting.
--   * the index is partial (WHERE tags IS NOT NULL): every existing row is NULL, so the build scans
--     the table but indexes nothing old. A `tags @> '...'` or `tags ? 'k'` predicate is strict, so
--     the planner proves `tags IS NOT NULL` from it and the partial index serves it. Default
--     jsonb_ops, so both `?` and `@>` are indexable.
--
-- Idempotency: one flat directory serves every goose target (processor, ingestor, dashboard), each
-- with its own ledger against the same physical database, so this file is replayed. ADD COLUMN IF
-- NOT EXISTS, DROP INDEX CONCURRENTLY IF EXISTS and CREATE INDEX CONCURRENTLY IF NOT EXISTS make every
-- block re-runnable; a failure leaves everything before it applied and the ledger unadvanced.
-- drop-if-invalid: a previous run's failed concurrent build leaves idx_error_occurrences_tags
-- existing with pg_index.indisvalid = false, and IF NOT EXISTS would then skip the build for ever.
-- The DO block renames an invalid index (only an invalid one) and the top-level DROP INDEX
-- CONCURRENTLY removes the renamed leftover; the final check RAISEs by name unless the index exists
-- and is valid.
--
-- No backfill: old occurrence rows keep NULL in every new column. No `issues` change.
--
-- DEPLOY ORDER (spec §5.7): this migration first (compose's one-shot `migrate` service and the
-- migrate Job already run before the app containers), then the processor, then the ingestor and the
-- dashboard. An old processor ignores the new columns and an old ingestor never sends the new
-- fields, so every intermediate state works.
--
-- ROLLBACK ORDER: the reverse, and Down runs LAST. First roll the processor AND the dashboard back
-- to a build that does not name the new columns, then run Down. Running Down while the new
-- processor is up makes every occurrence INSERT (which names all 17 columns) fail with SQLSTATE
-- 42703 -- not class 22/23, so it is retried and, once the delivery limit is spent, dead-lettered;
-- the new dashboard's agent reads fail the same way. Down sets lock_timeout before its ALTER, whose
-- DROP COLUMNs take ACCESS EXCLUSIVE on the hot table.

-- +goose Up
-- +goose StatementBegin
SET statement_timeout = '15min';
-- +goose StatementEnd
-- +goose StatementBegin
SET lock_timeout = '5s';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE error_occurrences
    ADD COLUMN IF NOT EXISTS level VARCHAR(16),
    ADD COLUMN IF NOT EXISTS message TEXT,
    ADD COLUMN IF NOT EXISTS error_class VARCHAR(255),
    ADD COLUMN IF NOT EXISTS logger VARCHAR(64),
    ADD COLUMN IF NOT EXISTS server_name VARCHAR(256),
    ADD COLUMN IF NOT EXISTS transaction VARCHAR(200),
    ADD COLUMN IF NOT EXISTS dist VARCHAR(64),
    ADD COLUMN IF NOT EXISTS source VARCHAR(64),
    ADD COLUMN IF NOT EXISTS trace_flags BIGINT,
    ADD COLUMN IF NOT EXISTS received_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS sdk JSONB,
    ADD COLUMN IF NOT EXISTS event_user JSONB,
    ADD COLUMN IF NOT EXISTS tags JSONB,
    ADD COLUMN IF NOT EXISTS contexts JSONB,
    ADD COLUMN IF NOT EXISTS request JSONB,
    ADD COLUMN IF NOT EXISTS breadcrumbs JSONB,
    ADD COLUMN IF NOT EXISTS exception JSONB;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_tags_invalid;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$
BEGIN
    -- to_regclass is NULL when no index of that name exists: nothing to rename, the build below
    -- creates it. A valid index keeps its name and its OID and is never dropped.
    IF EXISTS (
        SELECT 1 FROM pg_index
         WHERE indexrelid = to_regclass('idx_error_occurrences_tags')
           AND NOT indisvalid
    ) THEN
        ALTER INDEX idx_error_occurrences_tags RENAME TO idx_error_occurrences_tags_invalid;
    END IF;
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_tags_invalid;
-- +goose StatementEnd

-- +goose StatementBegin
SET lock_timeout = 0;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_error_occurrences_tags
    ON error_occurrences USING GIN (tags)
    WHERE tags IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE
    valid boolean;
BEGIN
    SELECT indisvalid INTO valid FROM pg_index
     WHERE indexrelid = to_regclass('idx_error_occurrences_tags');
    IF valid IS NULL THEN
        RAISE EXCEPTION '1725300000_add_error_occurrences_event_v2_fields: idx_error_occurrences_tags does not exist after the build';
    END IF;
    IF NOT valid THEN
        RAISE EXCEPTION '1725300000_add_error_occurrences_event_v2_fields: idx_error_occurrences_tags exists but pg_index.indisvalid = false -- the concurrent build failed; the next run drops and rebuilds it';
    END IF;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
RESET lock_timeout;
-- +goose StatementEnd
-- +goose StatementBegin
RESET statement_timeout;
-- +goose StatementEnd

-- +goose Down
-- Runs only after the processor and the dashboard are rolled back (header, ROLLBACK ORDER).
-- +goose StatementBegin
SET lock_timeout = '5s';
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_tags;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS idx_error_occurrences_tags_invalid;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE error_occurrences
    DROP COLUMN IF EXISTS exception,
    DROP COLUMN IF EXISTS breadcrumbs,
    DROP COLUMN IF EXISTS request,
    DROP COLUMN IF EXISTS contexts,
    DROP COLUMN IF EXISTS tags,
    DROP COLUMN IF EXISTS event_user,
    DROP COLUMN IF EXISTS sdk,
    DROP COLUMN IF EXISTS received_at,
    DROP COLUMN IF EXISTS trace_flags,
    DROP COLUMN IF EXISTS source,
    DROP COLUMN IF EXISTS dist,
    DROP COLUMN IF EXISTS transaction,
    DROP COLUMN IF EXISTS server_name,
    DROP COLUMN IF EXISTS logger,
    DROP COLUMN IF EXISTS error_class,
    DROP COLUMN IF EXISTS message,
    DROP COLUMN IF EXISTS level;
-- +goose StatementEnd
-- +goose StatementBegin
RESET lock_timeout;
-- +goose StatementEnd
