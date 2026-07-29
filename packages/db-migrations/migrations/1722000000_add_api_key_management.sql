-- +goose Up
-- +goose StatementBegin
-- IF NOT EXISTS / IF NOT EXISTS throughout: same re-runnability rationale as
-- 1721900000_add_issue_lifecycle_and_relations.sql (U30) — this table and
-- its indexes were previously created unconditionally, which would fail a
-- second `up` run against an already-migrated schema (a different goose
-- version-tracking table pointed at the same physical database).
CREATE TABLE IF NOT EXISTS project_api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    project_id UUID REFERENCES projects(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    key_prefix VARCHAR(16) NOT NULL,
    key_hash VARCHAR(128) NOT NULL UNIQUE,
    scope VARCHAR(20) NOT NULL DEFAULT 'ingest' CHECK (scope IN ('ingest', 'read', 'admin')),
    status VARCHAR(20) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked', 'expired')),
    rate_limit_rpm INTEGER NOT NULL DEFAULT 5000,
    expires_at TIMESTAMPTZ DEFAULT NULL,
    revoked_at TIMESTAMPTZ DEFAULT NULL,
    created_by VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_api_keys_hash_status ON project_api_keys (key_hash, status);
CREATE INDEX IF NOT EXISTS idx_api_keys_org_project ON project_api_keys (organization_id, project_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS project_api_keys;
-- +goose StatementEnd
