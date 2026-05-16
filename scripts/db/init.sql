-- Enable UUID extension
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- Enable pgcrypto for digest() function
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Projects table
CREATE TABLE IF NOT EXISTS projects (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name VARCHAR(255) NOT NULL,
    api_key VARCHAR(64) NOT NULL UNIQUE,
    api_key_hash VARCHAR(128) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_projects_api_key ON projects(api_key);
CREATE INDEX idx_projects_api_key_hash ON projects(api_key_hash);

-- Issues table
CREATE TABLE IF NOT EXISTS issues (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    fingerprint VARCHAR(64) NOT NULL,
    message TEXT NOT NULL,
    error_class VARCHAR(255) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved', 'ignored')),
    first_seen TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    last_seen TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    count BIGINT NOT NULL DEFAULT 1,
    UNIQUE (project_id, fingerprint)
);

CREATE INDEX idx_issues_project_id ON issues(project_id);
CREATE INDEX idx_issues_fingerprint ON issues(fingerprint);
CREATE INDEX idx_issues_status ON issues(status);
CREATE INDEX idx_issues_last_seen ON issues(last_seen DESC);

-- Error occurrences table
CREATE TABLE IF NOT EXISTS error_occurrences (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    issue_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    environment VARCHAR(50) NOT NULL,
    platform VARCHAR(50) NOT NULL,
    stacktrace JSONB NOT NULL DEFAULT '[]',
    metadata JSONB NOT NULL DEFAULT '{}',
    trace_id VARCHAR(64),
    span_id VARCHAR(64),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_error_occurrences_issue_id ON error_occurrences(issue_id);
CREATE INDEX idx_error_occurrences_created_at ON error_occurrences(created_at DESC);
CREATE INDEX idx_error_occurrences_trace_id ON error_occurrences(trace_id);

-- Error search index table
CREATE TABLE IF NOT EXISTS error_search_index (
    occurrence_id UUID PRIMARY KEY REFERENCES error_occurrences(id) ON DELETE CASCADE,
    user_id VARCHAR(255),
    tenant_id VARCHAR(255),
    trace_id VARCHAR(64),
    span_id VARCHAR(64),
    request_id VARCHAR(255)
);

CREATE INDEX idx_error_search_user_id ON error_search_index(user_id);
CREATE INDEX idx_error_search_tenant_id ON error_search_index(tenant_id);
CREATE INDEX idx_error_search_trace_id ON error_search_index(trace_id);
CREATE INDEX idx_error_search_request_id ON error_search_index(request_id);

-- Full-text search index on issues
CREATE INDEX idx_issues_message_fts ON issues USING gin(to_tsvector('english', message));

-- Alert configurations table
CREATE TABLE IF NOT EXISTS alert_configs (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    channel VARCHAR(20) NOT NULL CHECK (channel IN ('email', 'telegram')),
    channel_config JSONB NOT NULL DEFAULT '{}',
    frequency_threshold INT NOT NULL DEFAULT 50,
    frequency_window_seconds INT NOT NULL DEFAULT 60,
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_alert_configs_project_id ON alert_configs(project_id);

-- Audit logs table
CREATE TABLE IF NOT EXISTS audit_logs (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    action VARCHAR(100) NOT NULL,
    resource_type VARCHAR(50),
    resource_id UUID,
    actor_id VARCHAR(255),
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_audit_logs_action ON audit_logs(action);
CREATE INDEX idx_audit_logs_created_at ON audit_logs(created_at DESC);

-- Settings table
CREATE TABLE IF NOT EXISTS settings (
    key VARCHAR(255) PRIMARY KEY,
    value TEXT NOT NULL, -- Encrypted value
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);
