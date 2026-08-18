-- +goose Up
-- +goose StatementBegin
-- N10 part 1 (docs/plans/AGENT_WORKER_PLAN.md rev 4 SS4.5): server-side per-project agent settings
-- and repository connections, the prerequisite for the N8 sentinel-worker. `project_agent_settings`
-- holds the on/off switch (`fix_enabled`, default false) plus the daily PR cap the worker enforces.
-- `project_repo_connections` holds the one-repo-per-project-v1 target the worker clones and tests
-- against; the PK is `project_id` itself (not a surrogate id) so "one connection per project" is
-- enforced by the primary key rather than an app-level check. `test_cmd` is a server-stored command
-- the worker executes verbatim -- accepted deliberately (running a cloned repo's own tests already
-- executes repo-controlled code; the fix container sandbox is the real boundary), gated by
-- manage_agents RBAC + the existing audit_logs trail, not by anything in this migration.
--
-- No credentials table/field here on purpose -- a SIBLING task owns the encrypted git-credentials
-- store; this migration only prefixes its own tables project_agent_* / project_repo_* to keep the
-- namespaces from colliding.
--
-- IDEMPOTENCY (A1): one flat migration directory serves several goose ledgers against the SAME
-- physical database, so this file is replayed per target. `CREATE TABLE IF NOT EXISTS` covers the
-- PK/FK/NOT NULL/DEFAULT declared inline; the CHECK constraints are added separately below via a
-- pg_constraint guard since `ADD CONSTRAINT` has no `IF NOT EXISTS` form.
CREATE TABLE IF NOT EXISTS project_agent_settings (
	project_id UUID PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
	fix_enabled BOOLEAN NOT NULL DEFAULT false,
	max_prs_per_day INTEGER,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
BEGIN
	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conname = 'project_agent_settings_max_prs_per_day_check'
		AND conrelid = 'project_agent_settings'::regclass
	) THEN
		ALTER TABLE project_agent_settings
			ADD CONSTRAINT project_agent_settings_max_prs_per_day_check CHECK (max_prs_per_day > 0);
	END IF;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS project_repo_connections (
	project_id UUID PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
	provider TEXT NOT NULL,
	owner TEXT NOT NULL,
	repo TEXT NOT NULL,
	default_branch TEXT NOT NULL,
	test_cmd TEXT NOT NULL,
	agent_cmd TEXT,
	clone_depth INTEGER,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
BEGIN
	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conname = 'project_repo_connections_provider_check'
		AND conrelid = 'project_repo_connections'::regclass
	) THEN
		ALTER TABLE project_repo_connections
			ADD CONSTRAINT project_repo_connections_provider_check CHECK (provider IN ('github', 'bitbucket'));
	END IF;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
BEGIN
	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conname = 'project_repo_connections_clone_depth_check'
		AND conrelid = 'project_repo_connections'::regclass
	) THEN
		ALTER TABLE project_repo_connections
			ADD CONSTRAINT project_repo_connections_clone_depth_check CHECK (clone_depth > 0);
	END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS project_repo_connections;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS project_agent_settings;
-- +goose StatementEnd
