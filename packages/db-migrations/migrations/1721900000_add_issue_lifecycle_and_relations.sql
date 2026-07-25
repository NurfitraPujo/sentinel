-- +goose Up
-- +goose StatementBegin

-- Change existing 'open' to 'unresolved'
UPDATE issues SET status = 'unresolved' WHERE status = 'open';

-- Modify issues table
ALTER TABLE issues 
  ALTER COLUMN status SET DEFAULT 'unresolved',
  ADD COLUMN regression_status VARCHAR(20) NOT NULL DEFAULT 'none' CHECK (regression_status IN ('none', 'regressed')),
  ADD COLUMN issue_type VARCHAR(50) NOT NULL DEFAULT 'system_error' CHECK (issue_type IN ('system_error', 'user_report')),
  ADD COLUMN source_channel VARCHAR(50) NOT NULL DEFAULT 'ingestion_sdk' CHECK (source_channel IN ('ingestion_sdk', 'manual_support', 'api')),
  ADD COLUMN assignee_type VARCHAR(20) DEFAULT NULL CHECK (assignee_type IN ('user', 'agent')),
  ADD COLUMN assigned_to VARCHAR(255) DEFAULT NULL,
  ADD COLUMN resolved_in_version VARCHAR(100) DEFAULT NULL,
  ADD COLUMN resolved_at TIMESTAMP WITH TIME ZONE DEFAULT NULL,
  ADD COLUMN resolved_by_type VARCHAR(20) DEFAULT NULL CHECK (resolved_by_type IN ('user', 'agent')),
  ADD COLUMN resolved_by VARCHAR(255) DEFAULT NULL,
  ADD COLUMN regression_count INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN last_regressed_at TIMESTAMP WITH TIME ZONE DEFAULT NULL;

-- Add check constraint for status
ALTER TABLE issues ADD CONSTRAINT check_status CHECK (status IN ('unresolved', 'resolved', 'ignored'));

-- Add indexes on issues
CREATE INDEX idx_issues_project_status_regression ON issues (project_id, status, regression_status);
CREATE INDEX idx_issues_project_assignee ON issues (project_id, assignee_type, assigned_to);

-- Modify error_occurrences table
ALTER TABLE error_occurrences
  ADD COLUMN release_version VARCHAR(100) DEFAULT NULL;

CREATE INDEX idx_occurrences_issue_release ON error_occurrences (issue_id, release_version);

-- Create issue_relations table
CREATE TABLE issue_relations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_issue_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    target_issue_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    relation_type VARCHAR(50) NOT NULL CHECK (relation_type IN ('linked_to', 'caused_by', 'duplicate_of')),
    created_by_type VARCHAR(20) NOT NULL CHECK (created_by_type IN ('user', 'agent', 'system')),
    created_by VARCHAR(255) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    CONSTRAINT issue_relations_unique UNIQUE (source_issue_id, target_issue_id, relation_type)
);

CREATE INDEX idx_issue_relations_source ON issue_relations (source_issue_id);
CREATE INDEX idx_issue_relations_target ON issue_relations (target_issue_id);

-- Create issue_activity table
CREATE TABLE issue_activity (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    issue_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    actor_type VARCHAR(20) NOT NULL CHECK (actor_type IN ('user', 'agent', 'system')),
    actor_id VARCHAR(255) NOT NULL,
    event_type VARCHAR(50) NOT NULL CHECK (event_type IN ('status_changed', 'assigned', 'unassigned', 'regressed', 'ai_analysis', 'linked')),
    old_value JSONB DEFAULT NULL,
    new_value JSONB DEFAULT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_issue_activity_issue_created ON issue_activity (issue_id, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS issue_activity;
DROP TABLE IF EXISTS issue_relations;

DROP INDEX IF EXISTS idx_occurrences_issue_release;
ALTER TABLE error_occurrences DROP COLUMN release_version;

DROP INDEX IF EXISTS idx_issues_project_assignee;
DROP INDEX IF EXISTS idx_issues_project_status_regression;

ALTER TABLE issues DROP CONSTRAINT check_status;
ALTER TABLE issues 
  ALTER COLUMN status SET DEFAULT 'open',
  DROP COLUMN regression_status,
  DROP COLUMN issue_type,
  DROP COLUMN source_channel,
  DROP COLUMN assignee_type,
  DROP COLUMN assigned_to,
  DROP COLUMN resolved_in_version,
  DROP COLUMN resolved_at,
  DROP COLUMN resolved_by_type,
  DROP COLUMN resolved_by,
  DROP COLUMN regression_count,
  DROP COLUMN last_regressed_at;

-- Revert status 'unresolved' back to 'open'
UPDATE issues SET status = 'open' WHERE status = 'unresolved';

-- +goose StatementEnd
