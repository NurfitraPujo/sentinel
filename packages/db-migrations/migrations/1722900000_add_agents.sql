-- +goose Up
-- +goose StatementBegin
-- Manual Issues M5 stage 1 (docs/plans/MANUAL_ISSUES_DESIGN.md §7, Q5): agent identity + the
-- 'agent' key scope. IDEMPOTENCY: same rationale as every migration in this feature (A1) -- one
-- flat directory serves several goose ledgers against the SAME physical database, so this file is
-- replayed per target. IF NOT EXISTS on the table/columns/indexes; a pg_constraint catalog guard
-- for the project_api_keys.scope CHECK swap (Postgres has no ADD CONSTRAINT IF NOT EXISTS),
-- mirroring 1722600000's issue_activity_event_type_check pattern.

-- §7 Identity: agents(id, org_id, name, kind, status, created_by, created_at). Org-scoped (not
-- project-scoped) because an agent key works across every project in the org (project_id NULL on
-- project_api_keys for agent-scoped keys, see below).
CREATE TABLE IF NOT EXISTS agents (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    kind VARCHAR(20) NOT NULL CHECK (kind IN ('ai', 'bot')),
    status VARCHAR(20) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_by VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agents_org ON agents (org_id);

-- §7 Credentials: project_api_keys gains scope value 'agent' and a nullable agent_id FK. All
-- existing prefix/hash/status/revocation/rate-limit machinery in
-- src/lib/db/queries/apikeys.ts is reused unchanged; agent keys are always org-scoped
-- (project_id NULL), never bound to a single project.
ALTER TABLE project_api_keys
  ADD COLUMN IF NOT EXISTS agent_id UUID REFERENCES agents(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS idx_api_keys_agent ON project_api_keys (agent_id);

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'project_api_keys_scope_check' AND conrelid = 'project_api_keys'::regclass
  ) THEN
    ALTER TABLE project_api_keys DROP CONSTRAINT project_api_keys_scope_check;
  END IF;

  ALTER TABLE project_api_keys ADD CONSTRAINT project_api_keys_scope_check CHECK (
    scope IN ('ingest', 'read', 'admin', 'agent')
  );
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'project_api_keys_scope_check' AND conrelid = 'project_api_keys'::regclass
  ) THEN
    ALTER TABLE project_api_keys DROP CONSTRAINT project_api_keys_scope_check;
  END IF;

  ALTER TABLE project_api_keys ADD CONSTRAINT project_api_keys_scope_check CHECK (
    scope IN ('ingest', 'read', 'admin')
  );
END $$;

DROP INDEX IF EXISTS idx_api_keys_agent;
ALTER TABLE project_api_keys DROP COLUMN IF EXISTS agent_id;

DROP TABLE IF EXISTS agents;

-- +goose StatementEnd
