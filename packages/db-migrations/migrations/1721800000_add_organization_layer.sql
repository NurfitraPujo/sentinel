-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS organizations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(255) NOT NULL,
    slug VARCHAR(255) UNIQUE NOT NULL,
    avatar_url TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS organization_members (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id VARCHAR(255) NOT NULL,
    role VARCHAR(20) DEFAULT 'viewer' CHECK (role IN ('owner', 'admin', 'engineer', 'support', 'viewer')),
    joined_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_organization_members_user_org ON organization_members(user_id, organization_id);

CREATE TABLE IF NOT EXISTS organization_invitations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email VARCHAR(255) NOT NULL,
    role VARCHAR(20) DEFAULT 'viewer' CHECK (role IN ('owner', 'admin', 'engineer', 'support', 'viewer')),
    token VARCHAR(128) UNIQUE NOT NULL,
    status VARCHAR(20) DEFAULT 'pending' CHECK (status IN ('pending', 'accepted', 'revoked', 'expired')),
    expires_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS user_session_preferences (
    user_id VARCHAR(255) PRIMARY KEY,
    last_active_organization_id UUID REFERENCES organizations(id) ON DELETE SET NULL,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE projects ADD COLUMN IF NOT EXISTS organization_id UUID REFERENCES organizations(id) ON DELETE CASCADE;

-- Data migration step: Automatically create a default personal organization for existing projects and populate projects.organization_id.
DO $$
DECLARE
    rec RECORD;
    new_org_id UUID;
BEGIN
    FOR rec IN SELECT id, name FROM projects WHERE organization_id IS NULL LOOP
        -- Create a personal organization for this project
        INSERT INTO organizations (name, slug)
        VALUES (rec.name || ' Organization', 'org-' || gen_random_uuid())
        RETURNING id INTO new_org_id;
        
        -- Update the project
        UPDATE projects SET organization_id = new_org_id WHERE id = rec.id;
    END LOOP;
END $$;
-- projects.name is the tenant routing key used by the org-wide-key path
-- (X-Project-Key header) and by store.GetProjectByKey's `WHERE name = $1`.
-- Without this, two organizations can own same-named projects and that lookup
-- resolves arbitrarily across tenants — see VERIFIED_STATE.md S6.
CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_org_name ON projects (organization_id, name);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_projects_org_name;
ALTER TABLE projects DROP COLUMN IF EXISTS organization_id;
DROP TABLE IF EXISTS user_session_preferences;
DROP TABLE IF EXISTS organization_invitations;
DROP TABLE IF EXISTS organization_members;
DROP TABLE IF EXISTS organizations;
-- +goose StatementEnd
