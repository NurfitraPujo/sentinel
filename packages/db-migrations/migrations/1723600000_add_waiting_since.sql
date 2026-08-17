-- +goose Up
-- +goose StatementBegin
-- N9 (docs/plans/AGENT_WORKER_PLAN.md, C12): a blocking agent question sets issues.waiting_on but
-- records no timestamp, so an agent nagging a stale question had to reconstruct "waiting since when"
-- from the comment thread. This column records the moment the CURRENT blocking question was asked;
-- it is set in the SAME transaction as waiting_on (queries/comments.ts blocking branch) and cleared
-- to NULL in the same transaction that clears waiting_on (any human reply, or a resolve/ignore
-- transition -- queries/issues.ts). It is only meaningful while waiting_on IS NOT NULL; the list
-- query surfaces it as `waitingSince` only for waiting rows.
--
-- No backfill: issues already waiting when this ships get waiting_since = NULL (the age of an
-- in-flight question is unknowable after the fact), which the list surfaces as null.
--
-- IDEMPOTENCY (A1): one flat migration directory serves several goose ledgers against the SAME
-- physical database, so this file is replayed per target -- `ADD COLUMN IF NOT EXISTS` is
-- idempotent on its own, unlike `ADD CONSTRAINT`.
ALTER TABLE issues ADD COLUMN IF NOT EXISTS waiting_since TIMESTAMPTZ;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE issues DROP COLUMN IF EXISTS waiting_since;
-- +goose StatementEnd
