-- +goose Up
-- +goose StatementBegin
-- Two-layer alert routing: an alert config is either ORGANIZATION-WIDE (project_id IS NULL, applies to
-- every project in the organization) or PROJECT-SCOPED (project_id set). Before this, alert_configs had
-- project_id NOT NULL and no organization_id at all, so there was no way to say "notify this address for
-- anything in our org" — every project had to repeat the same routing, and a new project silently had no
-- alerting until someone remembered to add it.
--
-- The shape deliberately mirrors project_api_keys (1722000000), which already models exactly this
-- distinction: organization_id NOT NULL, project_id nullable, NULL meaning organization-wide. Matching an
-- existing pattern matters more than inventing a tidier one — the application code, the RBAC checks and
-- the mental model are already built around that convention.
--
-- IF NOT EXISTS / guarded DO blocks throughout: same re-runnability rationale as the migrations before
-- this one — several goose version-tracking tables point at the same physical database, so an `up` can
-- legitimately be replayed against an already-migrated schema.

-- A composite foreign key needs a matching unique key on the parent. projects.id is already the primary
-- key, so this adds no real constraint — it exists purely so alert_configs can reference the PAIR and have
-- Postgres enforce that a project-scoped config's project actually belongs to the organization the config
-- claims. Without it, nothing would stop a row naming org A and a project in org B, which is a
-- cross-tenant routing leak: org A's alert would fire on org B's events.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'projects'::regclass AND conname = 'projects_id_organization_id_key'
    ) THEN
        ALTER TABLE projects ADD CONSTRAINT projects_id_organization_id_key UNIQUE (id, organization_id);
    END IF;
END $$;

ALTER TABLE alert_configs ADD COLUMN IF NOT EXISTS organization_id UUID;

-- Backfill from the project every existing row already points at, so no configuration is lost and the
-- column can be made NOT NULL in the same migration.
UPDATE alert_configs ac
   SET organization_id = p.organization_id
  FROM projects p
 WHERE p.id = ac.project_id
   AND ac.organization_id IS NULL;

-- Any row that could not be backfilled has a project_id pointing at a project that no longer exists; the
-- old single-column FK had ON DELETE CASCADE, so this should be empty, but deleting explicitly is safer
-- than letting SET NOT NULL fail on a row nobody can route anyway.
DELETE FROM alert_configs WHERE organization_id IS NULL;

ALTER TABLE alert_configs ALTER COLUMN organization_id SET NOT NULL;

-- project_id becomes optional: NULL is what makes a config organization-wide.
ALTER TABLE alert_configs ALTER COLUMN project_id DROP NOT NULL;

-- Replace the single-column project FK with the composite one. MATCH SIMPLE (the default) is exactly the
-- behaviour wanted here: when project_id IS NULL the constraint is satisfied trivially, so organization-
-- wide rows are unconstrained by projects, while project-scoped rows must match BOTH columns.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'alert_configs'::regclass AND conname = 'alert_configs_project_id_fkey'
    ) THEN
        ALTER TABLE alert_configs DROP CONSTRAINT alert_configs_project_id_fkey;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'alert_configs'::regclass AND conname = 'alert_configs_project_org_fkey'
    ) THEN
        ALTER TABLE alert_configs
            ADD CONSTRAINT alert_configs_project_org_fkey
            FOREIGN KEY (project_id, organization_id)
            REFERENCES projects (id, organization_id) ON DELETE CASCADE;
    END IF;

    -- organization_id needs its own FK as well: the composite one above enforces nothing when project_id
    -- IS NULL, which is precisely the organization-wide case, so without this an org-wide config could
    -- outlive its organization.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'alert_configs'::regclass AND conname = 'alert_configs_organization_id_fkey'
    ) THEN
        ALTER TABLE alert_configs
            ADD CONSTRAINT alert_configs_organization_id_fkey
            FOREIGN KEY (organization_id) REFERENCES organizations (id) ON DELETE CASCADE;
    END IF;
END $$;

-- The processor resolves a project's alerts as "project-scoped for this project" UNION "organization-wide
-- for its organization", so both lookups need an index.
CREATE INDEX IF NOT EXISTS idx_alert_configs_organization_id ON alert_configs (organization_id);
CREATE INDEX IF NOT EXISTS idx_alert_configs_org_project ON alert_configs (organization_id, project_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Organization-wide configs cannot be represented once project_id is NOT NULL again. They are deleted
-- rather than silently attached to an arbitrary project in the organization, which would start sending
-- that project's alerts to an address chosen by a migration.
DELETE FROM alert_configs WHERE project_id IS NULL;

DROP INDEX IF EXISTS idx_alert_configs_org_project;
DROP INDEX IF EXISTS idx_alert_configs_organization_id;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'alert_configs'::regclass AND conname = 'alert_configs_project_org_fkey'
    ) THEN
        ALTER TABLE alert_configs DROP CONSTRAINT alert_configs_project_org_fkey;
    END IF;

    IF EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'alert_configs'::regclass AND conname = 'alert_configs_organization_id_fkey'
    ) THEN
        ALTER TABLE alert_configs DROP CONSTRAINT alert_configs_organization_id_fkey;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'alert_configs'::regclass AND conname = 'alert_configs_project_id_fkey'
    ) THEN
        ALTER TABLE alert_configs
            ADD CONSTRAINT alert_configs_project_id_fkey
            FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE;
    END IF;
END $$;

ALTER TABLE alert_configs ALTER COLUMN project_id SET NOT NULL;
ALTER TABLE alert_configs DROP COLUMN IF EXISTS organization_id;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'projects'::regclass AND conname = 'projects_id_organization_id_key'
    ) THEN
        ALTER TABLE projects DROP CONSTRAINT projects_id_organization_id_key;
    END IF;
END $$;
-- +goose StatementEnd
