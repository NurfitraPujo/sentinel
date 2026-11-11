-- +goose Up
-- +goose StatementBegin
-- M6 Feature A (docs/plans/M6_PRESIGNED_UPLOADS_AND_TOOLBAR_PLAN.md), Stage A.
--
-- IDEMPOTENCY: one flat migration directory serves several goose ledgers against the same
-- physical database (A1), so this file is replayed per target -- every statement below is
-- written to be a no-op on a schema that already has it applied (IF NOT EXISTS on
-- columns; a pg_constraint catalog guard for the status CHECK, since Postgres has no
-- ADD CONSTRAINT IF NOT EXISTS).

-- 'pending' | 'ready'. Presigned-upload rows start 'pending' and only flip to 'ready' once
-- finalize has ranged-GET'd the object and run the same sniffContentType/resolveContentType
-- allowlist as the proxy path. Existing rows (all validated inline via the proxy path) default
-- to 'ready' -- correct, they were already validated synchronously at upload time.
ALTER TABLE attachments
  ADD COLUMN IF NOT EXISTS status varchar(16) NOT NULL DEFAULT 'ready';

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'attachments_status_check' AND conrelid = 'attachments'::regclass
  ) THEN
    ALTER TABLE attachments ADD CONSTRAINT attachments_status_check CHECK (
      status IN ('pending', 'ready')
    );
  END IF;
END $$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'attachments_status_check' AND conrelid = 'attachments'::regclass
  ) THEN
    ALTER TABLE attachments DROP CONSTRAINT attachments_status_check;
  END IF;
END $$;

ALTER TABLE attachments DROP COLUMN IF EXISTS status;

-- +goose StatementEnd
