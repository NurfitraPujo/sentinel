-- +goose Up
-- +goose StatementBegin
-- Manual Issues M2 (docs/plans/MANUAL_ISSUES_DESIGN.md §4): attachments storage.
--
-- IDEMPOTENCY: one flat migration directory serves several goose ledgers against the same
-- physical database (A1), so this file is replayed per target -- every statement below is
-- written to be a no-op on a schema that already has it applied (IF NOT EXISTS on
-- tables/indexes; a pg_constraint catalog guard for the "at most one parent" CHECK, since
-- Postgres has no ADD CONSTRAINT IF NOT EXISTS).
--
-- org_id NOT NULL: tenant scope for the download access check must never depend on walking
-- through issue_id/comment_id (both nullable while drafting -- see the parent CHECK below).
-- issue_id/comment_id are both nullable and mutually exclusive-or-neither: an attachment can
-- be uploaded before it is linked to its parent (design §4 "none while drafting"), but once
-- linked it may point at exactly one of an issue or a comment, never both.
CREATE TABLE IF NOT EXISTS attachments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    issue_id UUID REFERENCES issues(id) ON DELETE CASCADE,
    comment_id UUID REFERENCES issue_comments(id) ON DELETE CASCADE,
    uploader_type VARCHAR(20) NOT NULL CHECK (uploader_type IN ('user', 'agent')),
    uploader_id VARCHAR(255) NOT NULL,
    filename VARCHAR(512) NOT NULL,
    content_type VARCHAR(255) NOT NULL,
    size_bytes BIGINT NOT NULL,
    storage_key VARCHAR(1024) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- storage_key UNIQUE: one object per row, and the natural lookup key for the download route.
-- A separate unique index (rather than an inline UNIQUE column constraint) so the guard below
-- can check pg_constraint uniformly with the other catalog-guarded constraints in this file.
CREATE UNIQUE INDEX IF NOT EXISTS idx_attachments_storage_key ON attachments (storage_key);

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'attachments_single_parent_check' AND conrelid = 'attachments'::regclass
  ) THEN
    ALTER TABLE attachments ADD CONSTRAINT attachments_single_parent_check CHECK (
      NOT (issue_id IS NOT NULL AND comment_id IS NOT NULL)
    );
  END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_attachments_issue ON attachments (issue_id);
CREATE INDEX IF NOT EXISTS idx_attachments_comment ON attachments (comment_id);
CREATE INDEX IF NOT EXISTS idx_attachments_org_created ON attachments (org_id, created_at);
-- Orphan reaper (design §4, invitation-reaper pattern D42): sweeps rows with both parents
-- still NULL older than 24h. This partial index keeps that scan cheap regardless of table size.
CREATE INDEX IF NOT EXISTS idx_attachments_orphan_scan ON attachments (created_at)
  WHERE issue_id IS NULL AND comment_id IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS attachments;

-- +goose StatementEnd
