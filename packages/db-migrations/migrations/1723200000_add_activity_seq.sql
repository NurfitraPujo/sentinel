-- +goose Up
-- +goose StatementBegin
-- N1a (AI-agent-native Sentinel): a monotonically increasing cursor for the agent events feed on
-- issue_activity. IDEMPOTENCY: one flat migration directory serves several goose ledgers against
-- the SAME physical database (A1), so this file is replayed per target. `ADD COLUMN IF NOT EXISTS`
-- cannot be used here because Postgres does not support adding a GENERATED ALWAYS AS IDENTITY
-- column with that clause, so the add is instead guarded by an information_schema.columns check.
--
-- `seq` backfills existing rows in the table's physical storage order, which is approximately but
-- not exactly `created_at` order -- acceptable, because the cursor only needs to be
-- strictly-increasing FROM NOW ON, not a faithful historical ordering. Like any identity/serial
-- sequence, `seq` can have gaps (aborted transactions consume a value) and brief commit-order
-- inversion under concurrency (a lower seq can commit fractionally after a higher one). Consumers
-- of the agent events feed must apply a short (2s) `created_at` lag guard when polling by `seq` so
-- an in-flight lower-seq row can still be observed before it is skipped past.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'issue_activity' AND column_name = 'seq'
  ) THEN
    ALTER TABLE issue_activity ADD COLUMN seq BIGINT GENERATED ALWAYS AS IDENTITY;
  END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS idx_issue_activity_seq ON issue_activity (seq);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_issue_activity_seq;
ALTER TABLE issue_activity DROP COLUMN IF EXISTS seq;

-- +goose StatementEnd
