-- +goose Up
-- +goose StatementBegin
-- Manual Issues M1 (docs/plans/MANUAL_ISSUES_DESIGN.md, DATABASE ONLY stage).
--
-- IDEMPOTENCY: one flat migration directory serves several goose ledgers against the same
-- physical database (A1), so this file is replayed per target -- every statement below is
-- written to be a no-op on a schema that already has it applied (IF NOT EXISTS on
-- tables/columns/indexes; a pg_constraint catalog guard for the event_type CHECK, since
-- Postgres has no ADD CONSTRAINT IF NOT EXISTS).

-- §2: a manual issue is an `issues` row (issue_type='user_report') + this 1:1 companion.
-- reporter_id references "user".id (better-auth's TEXT id) -- kept VARCHAR(255) to match
-- every other *_user_id/invited_by column in this schema (see 1722500000's comment).
CREATE TABLE IF NOT EXISTS manual_issue_reports (
    issue_id UUID PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    reporter_id VARCHAR(255) NOT NULL REFERENCES "user"(id),
    body_md TEXT NOT NULL,
    severity VARCHAR(20) NOT NULL DEFAULT 'medium' CHECK (severity IN ('low', 'medium', 'high', 'critical')),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- §2 Triage inbox: a durable per-org marker rather than a name convention. Inbox projects
-- are excluded from the error dashboard and alert evaluation and get no API key.
ALTER TABLE projects
  ADD COLUMN IF NOT EXISTS is_inbox BOOLEAN NOT NULL DEFAULT false;

-- §2 waiting flag (Q11): set by an agent's blocking question, auto-cleared on any human
-- reply. Not a new `status` value -- the existing check_status constraint is untouched.
ALTER TABLE issues
  ADD COLUMN IF NOT EXISTS waiting_on VARCHAR(20) DEFAULT NULL;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'issues_waiting_on_check' AND conrelid = 'issues'::regclass
  ) THEN
    ALTER TABLE issues ADD CONSTRAINT issues_waiting_on_check CHECK (waiting_on IN ('reporter', 'team'));
  END IF;
END $$;

-- §5: Slack-like one-level threads on ANY issue (both issue_type values). parent_id =
-- reply attaches to the same parent as the comment it replies to (Slack-style, not
-- infinitely nested). `blocking` marks an agent question that also sets issues.waiting_on.
CREATE TABLE IF NOT EXISTS issue_comments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    issue_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    parent_id UUID REFERENCES issue_comments(id) ON DELETE CASCADE,
    author_type VARCHAR(20) NOT NULL CHECK (author_type IN ('user', 'agent')),
    author_id VARCHAR(255) NOT NULL,
    blocking BOOLEAN NOT NULL DEFAULT false,
    body_md TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    edited_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_issue_comments_issue_created ON issue_comments (issue_id, created_at);
CREATE INDEX IF NOT EXISTS idx_issue_comments_parent ON issue_comments (parent_id);

-- §6: extend issue_activity.event_type with the new activity types this feature emits.
-- Postgres has no ALTER CONSTRAINT, so drop-then-recreate the named CHECK, guarded by a
-- pg_constraint lookup so a replay that already sees the wider set is a no-op (the DROP is
-- itself IF EXISTS, and the recreate only fires when the target definition is absent).
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'issue_activity_event_type_check' AND conrelid = 'issue_activity'::regclass
  ) THEN
    ALTER TABLE issue_activity DROP CONSTRAINT issue_activity_event_type_check;
  END IF;

  ALTER TABLE issue_activity ADD CONSTRAINT issue_activity_event_type_check CHECK (
    event_type IN (
      'status_changed', 'assigned', 'unassigned', 'regressed', 'ai_analysis', 'linked',
      'commented', 'claimed', 'claim_released', 'progress_update', 'question_asked',
      'question_answered', 'moved', 'attachment_added', 'report_edited'
    )
  );
END $$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'issue_activity_event_type_check' AND conrelid = 'issue_activity'::regclass
  ) THEN
    ALTER TABLE issue_activity DROP CONSTRAINT issue_activity_event_type_check;
  END IF;

  ALTER TABLE issue_activity ADD CONSTRAINT issue_activity_event_type_check CHECK (
    event_type IN ('status_changed', 'assigned', 'unassigned', 'regressed', 'ai_analysis', 'linked')
  );
END $$;

DROP TABLE IF EXISTS issue_comments;

ALTER TABLE issues DROP CONSTRAINT IF EXISTS issues_waiting_on_check;
ALTER TABLE issues DROP COLUMN IF EXISTS waiting_on;

ALTER TABLE projects DROP COLUMN IF EXISTS is_inbox;

DROP TABLE IF EXISTS manual_issue_reports;

-- +goose StatementEnd
