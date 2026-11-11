-- +goose Up
-- +goose StatementBegin
-- N8 (docs/audits/AGENT_AUTOMATION_AUDIT_2026-08-14.md A04): retention deletes issue rows, and
-- `issue_activity` is FK'd to `issues` with ON DELETE CASCADE (schema.ts) -- so a deleted issue's
-- entire history vanishes from `GET /api/agent/events` (events.ts joins issue_activity -> issues).
-- An agent holding a claim or awaiting a human answer just starts getting 404s with no terminal
-- signal on the feed. A tombstone cannot live in `issue_activity` (it would cascade away with the
-- issue), so it lives in this sibling table that carries NO FK to `issues` and is surfaced in the
-- feed via UNION with a synthetic eventType 'issue_deleted' (DECISIONS.md D20).
--
-- `seq` shares the SAME identity sequence as issue_activity.seq (via pg_get_serial_sequence), so a
-- tombstone interleaves into the one monotonic ordering the events-feed cursor reads by -- an agent
-- polling `?after=<seq>` sees the deletion in seq order alongside real activity, never before an
-- event it has already consumed. organization_id/project_id are denormalized onto the row because
-- the owning issue (and its join path to projects) is gone by the time this is read. assignee_type/
-- assigned_to are captured at deletion time so a claim-holding agent can still discover the deletion
-- via `?claimed=me` even though the assignment row it lived on has been deleted.
--
-- IDEMPOTENCY (A1): one flat migration directory serves several goose ledgers against the SAME
-- physical database, so this file is replayed per target -- every statement is guarded
-- (CREATE TABLE/INDEX IF NOT EXISTS) so replay is a no-op.
CREATE TABLE IF NOT EXISTS issue_tombstones (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  issue_id        uuid NOT NULL,
  organization_id uuid NOT NULL,
  project_id      uuid NOT NULL,
  issue_message   text,
  issue_type      varchar(50),
  assignee_type   varchar(20),
  assigned_to     varchar(255),
  reason          varchar(50) NOT NULL DEFAULT 'retention',
  deleted_at      timestamptz NOT NULL DEFAULT now(),
  seq             bigint NOT NULL DEFAULT nextval(pg_get_serial_sequence('issue_activity', 'seq'))
);

-- Feed read path: org-scoped, seq-cursored.
CREATE INDEX IF NOT EXISTS issue_tombstones_org_seq_idx ON issue_tombstones (organization_id, seq);
-- Tombstone retention prune path (bounded to TOMBSTONE_RETENTION_DAYS, default 30).
CREATE INDEX IF NOT EXISTS issue_tombstones_deleted_at_idx ON issue_tombstones (deleted_at);
-- +goose StatementEnd

-- +goose StatementBegin
-- The 'issue_deleted' eventType is synthesized in the feed from a tombstone row and is never
-- written into issue_activity. It is nonetheless added to the issue_activity CHECK constraint to
-- keep the documented event-type chain whole (AGENT_EVENT_TYPES in agent-events.ts is validated
-- against this set), mirroring 1723400000_widen_event_types_created_occurrence.sql's pg_constraint
-- guard for the drop+re-add (Postgres has no `ADD CONSTRAINT IF NOT EXISTS`).
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
      'created', 'occurrence_burst', 'issue_deleted'
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
      'question_answered', 'moved', 'attachment_added', 'report_edited', 'report_created',
      'created', 'occurrence_burst'
    )
  );
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS issue_tombstones;
-- +goose StatementEnd
