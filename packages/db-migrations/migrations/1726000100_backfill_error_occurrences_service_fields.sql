-- +goose NO TRANSACTION
-- sentinel-reporting-and-dashboard D18: one-time BATCHED backfill of error_occurrences.service, .tag_source and
-- .request_scoped from `tags`, for the rows Postgres still holds (retention keeps <= 30 days). It writes those three
-- columns only: never `source`.
--
-- The derivation is the SQL twin of apps/processor-go/event.DeriveOccurrenceOrigin and must stay identical to it:
--   service         = tags.service, trimmed, at most 64 characters, NULL when absent or empty
--   tag_source      = tags.source, the same way
--   request_scoped  = tag_source IN ('http_handler','http_panic')
--                     OR (tag_source = 'logger' AND transaction set AND request.url set)
--                     and false (never NULL) when tag_source is absent: the column is NOT NULL, and NULL IN (...)
--                     OR (NULL = 'logger' AND ...) is NULL, so the expression is wrapped in COALESCE(..., false)
--
-- Batches of 5,000 rows, keyset-paginated on the primary key, each batch committed on its own, so no lock outlives one
-- batch and none is held across the whole table. (A DO block may COMMIT only when it is not inside a transaction block,
-- which is why this file is NO TRANSACTION.) lock_timeout = '5s': a batch that cannot get its row locks fails fast and
-- the next sync re-runs the file.
--
-- Idempotent: a re-run recomputes the same values from the same tags, so a repeated batch writes what is already there.
-- The IS DISTINCT FROM guard skips rows already correct, so a re-run writes nothing.

-- +goose Up
-- +goose StatementBegin
SET lock_timeout = '5s';
-- +goose StatementEnd

-- +goose StatementBegin
DO $backfill$
DECLARE
    last_id    uuid := NULL;
    next_id    uuid;
    batch_size constant int := 5000;
BEGIN
    LOOP
        SELECT page.id INTO next_id FROM (
            SELECT id FROM error_occurrences
             WHERE tags IS NOT NULL AND (last_id IS NULL OR id > last_id)
             ORDER BY id
             LIMIT batch_size
        ) page ORDER BY page.id DESC LIMIT 1;
        EXIT WHEN next_id IS NULL;

        UPDATE error_occurrences o
           SET service = NULLIF(left(btrim(o.tags->>'service'), 64), ''),
               tag_source = NULLIF(left(btrim(o.tags->>'source'), 64), ''),
               request_scoped = COALESCE((
                   NULLIF(left(btrim(o.tags->>'source'), 64), '') IN ('http_handler', 'http_panic')
                   OR (
                       NULLIF(left(btrim(o.tags->>'source'), 64), '') = 'logger'
                       AND COALESCE(o.transaction, '') <> ''
                       AND COALESCE(o.request->>'url', '') <> ''
                   )
               ), false)
         WHERE o.tags IS NOT NULL
           AND (last_id IS NULL OR o.id > last_id)
           AND o.id <= next_id
           AND (o.service, o.tag_source, o.request_scoped) IS DISTINCT FROM (
                   NULLIF(left(btrim(o.tags->>'service'), 64), ''),
                   NULLIF(left(btrim(o.tags->>'source'), 64), ''),
                   COALESCE((
                       NULLIF(left(btrim(o.tags->>'source'), 64), '') IN ('http_handler', 'http_panic')
                       OR (
                           NULLIF(left(btrim(o.tags->>'source'), 64), '') = 'logger'
                           AND COALESCE(o.transaction, '') <> ''
                           AND COALESCE(o.request->>'url', '') <> ''
                       )
                   ), false)
               );

        COMMIT;
        last_id := next_id;
    END LOOP;
END
$backfill$;
-- +goose StatementEnd

-- +goose StatementBegin
RESET lock_timeout;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
