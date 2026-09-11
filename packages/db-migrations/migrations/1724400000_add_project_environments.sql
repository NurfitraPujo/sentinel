-- sentinel-project-environments §4.9 — the identity migration.
--
-- Read the identity guard below before anything else. Decision 7 (split issue identity by
-- environment) is correct ONLY while `issues` is empty: an existing row has collapsed several
-- environments into one, and no query can say which of its occurrences constituted which issue —
-- `status`, `count`, `first_seen`, `last_seen`, `resolved_in_version` and `regression_count` are
-- single shared values with no per-environment provenance. The guard makes that precondition a
-- database fact instead of a sentence in a document. There is deliberately NO environment variable
-- and NO flag to bypass it: overriding it means editing this file, which means a diff, which means
-- a human.
--
-- IF NOT EXISTS / catalog guards throughout: same re-runnability rationale as
-- 1722000000_add_api_key_management.sql — one flat migrations directory serves several goose
-- ledgers against the SAME physical database (A1), so every statement here is replayed per target.
-- `ADD CONSTRAINT` has no IF NOT EXISTS form, hence the `pg_constraint` catalog guards, exactly as
-- 1722900000_add_agents.sql does.

-- +goose Up
-- +goose StatementBegin
-- Decision 19. Neither string occurs anywhere else in this repository, so there is no house value
-- to inherit; §4.9 states these and the reasoning. The identity change below takes an
-- ACCESS EXCLUSIVE lock on `issues`: on the empty table the guard guarantees, acquiring it is
-- instantaneous, and 5s is short enough that on a database where it is NOT instantaneous this
-- migration fails fast and visibly instead of stalling ingest behind a lock wait. 5min on the
-- statement is a ceiling, not a target.
SET lock_timeout = '5s';
SET statement_timeout = '5min';
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM issues LIMIT 1) THEN
    RAISE EXCEPTION
      'sentinel-project-environments decision 7 (split issue identity) was taken on the verified '
      'basis that sentinel has never been deployed and `issues` is empty. This database has issue '
      'rows, so that basis does not hold here and the environment for each existing occurrence '
      'cannot be recovered. A human must re-take decision 7 before this migration may run.';
  END IF;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The DEFAULT exists only so `ADD COLUMN … NOT NULL` is legal against an existing table, and is
-- dropped on the very next statement so no future insert can omit the value. The literal is
-- `_unspecified` with a LEADING UNDERSCORE, and the underscore is the whole enforcement
-- (decision 51): it fails §3's `^[a-z0-9][a-z0-9._-]{0,49}$`, so no writer in the system can
-- produce it and no caller can declare a registry row that collides with it. There is no
-- reserved-word check anywhere in this feature and none is to be added. The guard above guarantees
-- the DEFAULT applies to no row.
ALTER TABLE issues ADD COLUMN IF NOT EXISTS environment VARCHAR(50) NOT NULL DEFAULT '_unspecified';
ALTER TABLE issues ALTER COLUMN environment DROP DEFAULT;

-- 1716508800_init.sql declares `UNIQUE (project_id, fingerprint)` inline (its line 26), so
-- PostgreSQL derives the name `issues_project_id_fingerprint_key`. Verified against `\d issues`
-- before this was written, not taken from the contract: a `DROP … IF EXISTS` that silently matches
-- nothing would leave the old constraint in place and the split half-built with no error.
ALTER TABLE issues DROP CONSTRAINT IF EXISTS issues_project_id_fingerprint_key;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'issues_project_env_fingerprint_key' AND conrelid = 'issues'::regclass
  ) THEN
    ALTER TABLE issues ADD CONSTRAINT issues_project_env_fingerprint_key
      UNIQUE (project_id, environment, fingerprint);
  END IF;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
-- §4.9's registry. There is NO backfill and none is to be restored: under decision 8 a registry row
-- exists because a person created it, and under decision 15 there are no occurrences to derive rows
-- from. Every project's registry is empty on day one, by design, and §4.1's empty-collection case is
-- the normal state rather than an edge one.
CREATE TABLE IF NOT EXISTS project_environments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- Matches issues.environment and error_occurrences.environment — this is the join key.
    name VARCHAR(50) NOT NULL,
    display_name VARCHAR(64) NOT NULL,
    is_default BOOLEAN NOT NULL DEFAULT false,
    -- Display only, never access. §4.10 is access.
    hidden BOOLEAN NOT NULL DEFAULT false,
    -- NULL until §4.8's UPDATE first touches the row.
    first_seen_at TIMESTAMPTZ,
    last_seen_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, name)
);

-- A partial unique index is what makes "at most one default per project" a database fact rather
-- than a hope in §4.2's handler.
CREATE UNIQUE INDEX IF NOT EXISTS idx_project_environments_one_default
    ON project_environments (project_id) WHERE is_default;

-- MAX_ENVIRONMENTS_PER_PROJECT is deliberately application-level (§4.3 only): a CHECK cannot count
-- sibling rows.
-- +goose StatementEnd

-- +goose StatementBegin
-- §2.2's grant. NO row is created by this migration, which is what makes it narrow nobody.
CREATE TABLE IF NOT EXISTS project_environment_grants (
    user_id TEXT NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    environments TEXT[] NOT NULL,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, project_id),
    CONSTRAINT project_environment_grants_environments_nonempty
        CHECK (array_length(environments, 1) >= 1)
);

CREATE INDEX IF NOT EXISTS idx_project_environment_grants_project
    ON project_environment_grants (project_id);
-- +goose StatementEnd

-- +goose StatementBegin
-- NULL = every environment. DEFAULT NULL, and existing rows are NOT backfilled — decisions 6 and
-- 11. A schema change must not silently change who is alerted.
ALTER TABLE alert_configs ADD COLUMN IF NOT EXISTS environments TEXT[] DEFAULT NULL;

-- NULL = the invitation carries no scope and the accepted member is unrestricted — decision 28,
-- §4.10a. Every existing row is NULL and none is backfilled, the same rule decisions 6 and 11 apply
-- to alert_configs.environments: a schema change must not silently change who sees what.
--
-- CONTRACT DEVIATION, reported in status/be.md and not fixed here: §4.9 and §4.10a name this table
-- `invitations`. There is no such table. The invitation table in this repository is
-- `organization_invitations` (created by 1721800000_add_organization_layer.sql:21, mirrored in
-- Drizzle as `organizationInvitations`, and altered under that name by 1722300000 and 1722500000).
-- be.md task 1's rule — "where a capture disagrees with a table in contract §4, the capture wins" —
-- is what settles it: a migration against `invitations` raises 42P01 and applies nothing.
ALTER TABLE organization_invitations ADD COLUMN IF NOT EXISTS environment_scopes JSONB DEFAULT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
-- §4.4's list: the guard, the environment filter, the project predicate and the
-- ORDER BY last_seen DESC in one index. The existing idx_issues_project_id and idx_issues_last_seen
-- are single-column and serve neither the composite predicate nor the sort together.
CREATE INDEX IF NOT EXISTS idx_issues_project_env_lastseen
    ON issues (project_id, environment, last_seen DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SET lock_timeout = '5s';
SET statement_timeout = '5min';

DROP INDEX IF EXISTS idx_issues_project_env_lastseen;
ALTER TABLE organization_invitations DROP COLUMN IF EXISTS environment_scopes;
ALTER TABLE alert_configs  DROP COLUMN IF EXISTS environments;
DROP TABLE IF EXISTS project_environment_grants;
DROP TABLE IF EXISTS project_environments;

-- `environment` is dropped BEFORE the old constraint is added back, so the three-column constraint
-- goes with the column and the two-column one is added to the shape it originally had.
ALTER TABLE issues DROP CONSTRAINT IF EXISTS issues_project_env_fingerprint_key;
ALTER TABLE issues DROP COLUMN IF EXISTS environment;

-- The name is NOT decoration. The next Up runs `DROP CONSTRAINT IF EXISTS
-- issues_project_id_fingerprint_key`, which targets this exact string; restoring the constraint
-- under any other name leaves that DROP matching nothing, and the split half-built with NO error —
-- and `up`/`down`/`up` still goes green, because on an empty table the stale two-column constraint
-- has nothing to reject. `\d issues` after this Down is the only thing that shows it.
--
-- Re-adding this against a table that HAS rows will fail loudly if two environments already share a
-- (project_id, fingerprint). That is correct: it is the identity split being un-done on data that
-- has come to depend on it, and it must not succeed quietly.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'issues_project_id_fingerprint_key' AND conrelid = 'issues'::regclass
  ) THEN
    ALTER TABLE issues ADD CONSTRAINT issues_project_id_fingerprint_key
      UNIQUE (project_id, fingerprint);
  END IF;
END $$;
-- +goose StatementEnd
