-- +goose Up
-- +goose StatementBegin

-- S12 fix: the inline constraint from 1716508800_init.sql (auto-named
-- issues_status_check, allowing 'open','resolved','ignored') was never dropped
-- when this migration introduced 'unresolved' and its own check_status
-- constraint (allowing 'unresolved','resolved','ignored'). Both constraints
-- were simultaneously live; their intersection, {'resolved','ignored'}, made
-- every INSERT/UPDATE writing 'unresolved' (the only status the application
-- ever writes, see apps/processor-go/service/processor_service.go) fail with
-- SQLSTATE 23514. Drop the stale inline constraint FIRST, before the data
-- backfill below, so the backfill itself does not trip on the old rule either.
--
-- U30 / re-runnability: every statement below is written to be safe to run
-- against a schema that has already had this migration applied (e.g. a
-- second `up` from an independent goose version-tracking table pointed at
-- the same physical database — this is exactly what
-- tests/integration/db_migrations_test.go does across its several
-- MigrationOptions, and what happens when the docker-compose `migrate`
-- one-shot service has already run against the same Postgres a test later
-- connects to). IF EXISTS / IF NOT EXISTS guards make that safe; the two DO
-- blocks below cover the two constraint types (CHECK, and the DROP target)
-- that Postgres has no direct "IF NOT EXISTS" DDL for.
ALTER TABLE issues DROP CONSTRAINT IF EXISTS issues_status_check;

-- Change existing 'open' to 'unresolved'
UPDATE issues SET status = 'unresolved' WHERE status = 'open';

-- Modify issues table. ADD COLUMN IF NOT EXISTS makes each column (and its
-- inline CHECK, added together the first time) a no-op on replay.
ALTER TABLE issues
  ALTER COLUMN status SET DEFAULT 'unresolved',
  ADD COLUMN IF NOT EXISTS regression_status VARCHAR(20) NOT NULL DEFAULT 'none' CHECK (regression_status IN ('none', 'regressed')),
  ADD COLUMN IF NOT EXISTS issue_type VARCHAR(50) NOT NULL DEFAULT 'system_error' CHECK (issue_type IN ('system_error', 'user_report')),
  ADD COLUMN IF NOT EXISTS source_channel VARCHAR(50) NOT NULL DEFAULT 'ingestion_sdk' CHECK (source_channel IN ('ingestion_sdk', 'manual_support', 'api')),
  ADD COLUMN IF NOT EXISTS assignee_type VARCHAR(20) DEFAULT NULL CHECK (assignee_type IN ('user', 'agent')),
  ADD COLUMN IF NOT EXISTS assigned_to VARCHAR(255) DEFAULT NULL,
  ADD COLUMN IF NOT EXISTS resolved_in_version VARCHAR(100) DEFAULT NULL,
  ADD COLUMN IF NOT EXISTS resolved_at TIMESTAMP WITH TIME ZONE DEFAULT NULL,
  ADD COLUMN IF NOT EXISTS resolved_by_type VARCHAR(20) DEFAULT NULL CHECK (resolved_by_type IN ('user', 'agent')),
  ADD COLUMN IF NOT EXISTS resolved_by VARCHAR(255) DEFAULT NULL,
  ADD COLUMN IF NOT EXISTS regression_count INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS last_regressed_at TIMESTAMP WITH TIME ZONE DEFAULT NULL;

-- Add check constraint for status (now the only status constraint on the
-- table). Postgres has no ADD CONSTRAINT IF NOT EXISTS, so guard via catalog
-- lookup.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'check_status' AND conrelid = 'issues'::regclass
  ) THEN
    ALTER TABLE issues ADD CONSTRAINT check_status CHECK (status IN ('unresolved', 'resolved', 'ignored'));
  END IF;
END $$;

-- Add indexes on issues
CREATE INDEX IF NOT EXISTS idx_issues_project_status_regression ON issues (project_id, status, regression_status);
CREATE INDEX IF NOT EXISTS idx_issues_project_assignee ON issues (project_id, assignee_type, assigned_to);

-- Modify error_occurrences table
ALTER TABLE error_occurrences
  ADD COLUMN IF NOT EXISTS release_version VARCHAR(100) DEFAULT NULL;

CREATE INDEX IF NOT EXISTS idx_occurrences_issue_release ON error_occurrences (issue_id, release_version);

-- Create issue_relations table
CREATE TABLE IF NOT EXISTS issue_relations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_issue_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    target_issue_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    relation_type VARCHAR(50) NOT NULL CHECK (relation_type IN ('linked_to', 'caused_by', 'duplicate_of')),
    created_by_type VARCHAR(20) NOT NULL CHECK (created_by_type IN ('user', 'agent', 'system')),
    created_by VARCHAR(255) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    CONSTRAINT issue_relations_unique UNIQUE (source_issue_id, target_issue_id, relation_type)
);

CREATE INDEX IF NOT EXISTS idx_issue_relations_source ON issue_relations (source_issue_id);
CREATE INDEX IF NOT EXISTS idx_issue_relations_target ON issue_relations (target_issue_id);

-- Create issue_activity table
CREATE TABLE IF NOT EXISTS issue_activity (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    issue_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    actor_type VARCHAR(20) NOT NULL CHECK (actor_type IN ('user', 'agent', 'system')),
    actor_id VARCHAR(255) NOT NULL,
    event_type VARCHAR(50) NOT NULL CHECK (event_type IN ('status_changed', 'assigned', 'unassigned', 'regressed', 'ai_analysis', 'linked')),
    old_value JSONB DEFAULT NULL,
    new_value JSONB DEFAULT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_issue_activity_issue_created ON issue_activity (issue_id, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS issue_activity;
DROP TABLE IF EXISTS issue_relations;

DROP INDEX IF EXISTS idx_occurrences_issue_release;
ALTER TABLE error_occurrences DROP COLUMN IF EXISTS release_version;

DROP INDEX IF EXISTS idx_issues_project_assignee;
DROP INDEX IF EXISTS idx_issues_project_status_regression;

ALTER TABLE issues DROP CONSTRAINT IF EXISTS check_status;
ALTER TABLE issues
  ALTER COLUMN status SET DEFAULT 'open',
  DROP COLUMN IF EXISTS regression_status,
  DROP COLUMN IF EXISTS issue_type,
  DROP COLUMN IF EXISTS source_channel,
  DROP COLUMN IF EXISTS assignee_type,
  DROP COLUMN IF EXISTS assigned_to,
  DROP COLUMN IF EXISTS resolved_in_version,
  DROP COLUMN IF EXISTS resolved_at,
  DROP COLUMN IF EXISTS resolved_by_type,
  DROP COLUMN IF EXISTS resolved_by,
  DROP COLUMN IF EXISTS regression_count,
  DROP COLUMN IF EXISTS last_regressed_at;

-- Revert status 'unresolved' back to 'open'
UPDATE issues SET status = 'open' WHERE status = 'unresolved';

-- Restore the original inline constraint dropped in Up (S12), so that a
-- subsequent Up on this same connection/schema recreates the exact state
-- 1716508800_init.sql originally established, and the DROP CONSTRAINT at the
-- top of Up has something to act on again. Guarded the same way as Up's
-- check_status, for the same replay-safety reason.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'issues_status_check' AND conrelid = 'issues'::regclass
  ) THEN
    ALTER TABLE issues ADD CONSTRAINT issues_status_check CHECK (status IN ('open', 'resolved', 'ignored'));
  END IF;
END $$;

-- +goose StatementEnd
