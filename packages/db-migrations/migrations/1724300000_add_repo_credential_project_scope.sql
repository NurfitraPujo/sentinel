-- +goose Up
-- +goose StatementBegin
-- Project-scoping for repo credentials. Before this, `agents.can_access_repo_credentials` was the
-- ONLY gate and `fetchDecryptedCredentialsForAgent(orgId)` returned every active credential in the
-- organization -- an agent working on one project received the git tokens for all of them.
--
-- Two additions:
--
--   `repo_credentials.project_id` -- NULL means org-wide, which is what every existing row is and
--   stays. A non-NULL value binds the credential to one project.
--
--   `agent_repo_credential_grants` -- which projects an agent may receive credentials for. The
--   grant is the scope; `agents.can_access_repo_credentials` is KEPT as an outer master switch, so
--   an agent needs the flag AND (for project-scoped credentials) a grant. Revoking everything for
--   one agent stays a single toggle.
--
-- Delivery, after this migration: org-wide credentials (project_id IS NULL) go to any agent whose
-- flag is set, exactly as today -- no existing worker breaks on deploy. Project-scoped credentials
-- go only to agents holding a grant for that project. Narrowing is therefore opt-in per credential:
-- set its project_id, and only granted agents see it.
--
-- IDEMPOTENCY (A1): one flat migration directory serves several goose ledgers against the SAME
-- physical database, so this file is replayed per target -- hence IF NOT EXISTS throughout and a
-- pg_constraint guard for the CHECK, which has no IF NOT EXISTS form.
ALTER TABLE repo_credentials ADD COLUMN IF NOT EXISTS project_id UUID REFERENCES projects(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS idx_repo_credentials_project ON repo_credentials(project_id);

CREATE TABLE IF NOT EXISTS agent_repo_credential_grants (
	agent_id UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
	project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	created_by VARCHAR(255) NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (agent_id, project_id)
);

CREATE INDEX IF NOT EXISTS idx_agent_repo_credential_grants_project ON agent_repo_credential_grants(project_id);

-- A credential's project, when it has one, must belong to the credential's own organization.
-- Without this a project id from another tenant could be written into the column and would then
-- match that tenant's agent grants. Enforced as a trigger rather than a CHECK because a CHECK
-- cannot read another table.
CREATE OR REPLACE FUNCTION repo_credential_project_in_org() RETURNS trigger AS $$
BEGIN
	IF NEW.project_id IS NOT NULL THEN
		IF NOT EXISTS (
			SELECT 1 FROM projects
			WHERE id = NEW.project_id AND organization_id = NEW.organization_id
		) THEN
			RAISE EXCEPTION 'repo_credentials.project_id must belong to organization_id';
		END IF;
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_repo_credential_project_in_org ON repo_credentials;
CREATE TRIGGER trg_repo_credential_project_in_org
	BEFORE INSERT OR UPDATE ON repo_credentials
	FOR EACH ROW EXECUTE FUNCTION repo_credential_project_in_org();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS trg_repo_credential_project_in_org ON repo_credentials;
DROP FUNCTION IF EXISTS repo_credential_project_in_org();
DROP TABLE IF EXISTS agent_repo_credential_grants;
DROP INDEX IF EXISTS idx_repo_credentials_project;
ALTER TABLE repo_credentials DROP COLUMN IF EXISTS project_id;
-- +goose StatementEnd
