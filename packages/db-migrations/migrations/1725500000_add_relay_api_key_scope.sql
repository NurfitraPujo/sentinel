-- +goose Up
-- +goose StatementBegin
-- sentinel-relay-key-trust TASK-01 (contract §4.1): widen project_api_keys_scope_check with 'relay', the scope that
-- alone makes the ingestor honour core's mobile-relay identity headers and relayed client IP on the
-- Sentry-compatible routes. It touches no existing row.
--
-- IDEMPOTENCY: one flat directory serves several goose ledgers against the SAME physical database, so this file is
-- replayed. The constraint is dropped by a pg_constraint catalog guard and re-added, as in
-- 1724100000_add_user_report_intake_and_oncall.sql, and the new set is a superset of every earlier one.
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'project_api_keys_scope_check' AND conrelid = 'project_api_keys'::regclass
  ) THEN
    ALTER TABLE project_api_keys DROP CONSTRAINT project_api_keys_scope_check;
  END IF;

  ALTER TABLE project_api_keys ADD CONSTRAINT project_api_keys_scope_check CHECK (
    scope IN ('ingest', 'read', 'admin', 'agent', 'report', 'relay')
  );

  -- A relay key asserts identity for ONE project's traffic: it is never org-wide (the dashboard API already refuses
  -- one; this makes it impossible in the database too, including for a hand-run UPDATE).
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'project_api_keys_relay_project_check' AND conrelid = 'project_api_keys'::regclass
  ) THEN
    ALTER TABLE project_api_keys ADD CONSTRAINT project_api_keys_relay_project_check CHECK (
      scope <> 'relay' OR project_id IS NOT NULL
    );
  END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Restores the 1724100000 set. A relay key would violate it, so any relay row is first demoted to 'ingest' (a
-- strictly smaller grant: it loses the right to assert an identity and keeps the right to ingest).
UPDATE project_api_keys SET scope = 'ingest' WHERE scope = 'relay';

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'project_api_keys_relay_project_check' AND conrelid = 'project_api_keys'::regclass
  ) THEN
    ALTER TABLE project_api_keys DROP CONSTRAINT project_api_keys_relay_project_check;
  END IF;

  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'project_api_keys_scope_check' AND conrelid = 'project_api_keys'::regclass
  ) THEN
    ALTER TABLE project_api_keys DROP CONSTRAINT project_api_keys_scope_check;
  END IF;

  ALTER TABLE project_api_keys ADD CONSTRAINT project_api_keys_scope_check CHECK (
    scope IN ('ingest', 'read', 'admin', 'agent', 'report')
  );
END $$;
-- +goose StatementEnd
