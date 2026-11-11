-- +goose Up
-- +goose StatementBegin
-- N7a (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md, A01/A06/R2): agents currently have no
-- reliable way to discover a brand-new issue or a repeat-occurrence burst from the events feed --
-- StoreEvent never wrote an issue_activity row for either case. This migration only widens the
-- CHECK constraint; the processor writes ('created', 'occurrence_burst') land in the same change
-- (apps/processor-go/store/store.go StoreEvent).
--
-- No backfill: synthesizing 'created' rows for pre-existing issues would misdate them and produce
-- a seq stampede on first deploy. Agents bootstrap pre-existing issues via GET /api/agent/issues
-- (N7b); the events feed is a forward-looking discovery stream, not a full history.
--
-- IDEMPOTENCY (A1): one flat migration directory serves several goose ledgers against the SAME
-- physical database, so this file is replayed per target -- the pg_constraint catalog guard
-- (Postgres has no `ADD CONSTRAINT IF NOT EXISTS`) makes the drop+re-add a no-op on replay,
-- mirroring 1723000000_pr13_remediation.sql's event-type widening.
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
      'question_answered', 'moved', 'attachment_added', 'report_edited', 'report_created',
      'created', 'occurrence_burst'
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
    event_type IN (
      'status_changed', 'assigned', 'unassigned', 'regressed', 'ai_analysis', 'linked',
      'commented', 'claimed', 'claim_released', 'progress_update', 'question_asked',
      'question_answered', 'moved', 'attachment_added', 'report_edited', 'report_created'
    )
  );
END $$;

-- +goose StatementEnd
