-- +goose Up
-- +goose StatementBegin
-- PR #13 review remediation (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md), Stage A.
--
-- IDEMPOTENCY: one flat migration directory serves several goose ledgers against the same
-- physical database (A1), so this file is replayed per target -- every statement below is
-- written to be a no-op on a schema that already has it applied (IF NOT EXISTS on
-- columns/indexes; a pg_constraint catalog guard for the event_type CHECK swap, since Postgres
-- has no ADD CONSTRAINT IF NOT EXISTS).

-- R2: Triage inbox race -- SELECT-then-INSERT with no uniqueness let two concurrent
-- findOrCreateTriageProject calls both insert an inbox project for the same org. A plain UNIQUE
-- on organization_id would forbid an org having a non-inbox project AND an inbox project (every
-- org needs both), so this is PARTIAL: only one row WHERE is_inbox may exist per org.
CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_org_inbox_unique
  ON projects (organization_id)
  WHERE is_inbox;

-- R5: email throttle needs to track actual send attempts, not count notification ROWS (which
-- includes rows that were themselves throttled and never emailed) -- see notify.ts's isThrottled.
ALTER TABLE notifications
  ADD COLUMN IF NOT EXISTS emailed_at TIMESTAMP WITH TIME ZONE;

-- R12 groundwork: `report_edited` mislabels manual-issue CREATION (createManualIssue writes it
-- for the initial report, not an edit). Widen the CHECK to also allow `report_created` here, in
-- the same migration as R2/R5/R20, so a single idempotent file carries every Stage A schema
-- change; the application-code switch to actually using `report_created` for creation (and
-- reserving `report_edited` for R11 edits) is R12, done in Stage C.
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
      'question_answered', 'moved', 'attachment_added', 'report_edited', 'report_created'
    )
  );
END $$;

-- R20: hot-path indexes missing for the assignee filter and the "Needs input" tab.
CREATE INDEX IF NOT EXISTS idx_issues_assigned_to ON issues (assigned_to);
CREATE INDEX IF NOT EXISTS idx_issues_waiting_on ON issues (waiting_on) WHERE waiting_on IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_issues_waiting_on;
DROP INDEX IF EXISTS idx_issues_assigned_to;

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

ALTER TABLE notifications DROP COLUMN IF EXISTS emailed_at;

DROP INDEX IF EXISTS idx_projects_org_inbox_unique;

-- +goose StatementEnd
