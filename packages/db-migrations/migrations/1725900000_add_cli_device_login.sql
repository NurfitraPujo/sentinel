-- +goose Up
-- +goose StatementBegin
-- sentinel-cli-browser-login TASK-01 (docs/plans/CLI_BROWSER_LOGIN.md): `sentinel login` through an RFC 8628 device
-- authorization flow, minting a PERSONAL agent key bound to the approving user.
--
-- IDEMPOTENCY: one flat directory serves several goose ledgers against the SAME physical database (A1), so this file is
-- replayed per target. Every CREATE/ADD is IF NOT EXISTS; the two constraint changes (Postgres has no ADD CONSTRAINT IF NOT
-- EXISTS) are pg_constraint-guarded, mirroring 1722900000_add_agents.sql.

-- 1. Personal agents. A personal agent is an `agents` row of kind 'personal' owned by one user. Org agents keep
--    owner_user_id NULL, so nothing about them changes. ON DELETE CASCADE: deleting the user deletes their agent, and
--    project_api_keys.agent_id's own ON DELETE CASCADE takes the keys with it, so no key outlives its owner.
--    `organization_members.user_id` has no FK to "user" (it is varchar), which is why membership is re-checked LIVE on
--    every request rather than relied on through a cascade.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS owner_user_id TEXT REFERENCES "user"(id) ON DELETE CASCADE;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'agents_kind_check' AND conrelid = 'agents'::regclass
  ) THEN
    ALTER TABLE agents DROP CONSTRAINT agents_kind_check;
  END IF;
  ALTER TABLE agents ADD CONSTRAINT agents_kind_check CHECK (kind IN ('ai', 'bot', 'personal'));

  -- A personal agent always has an owner; an org agent never has one. Without this, an admin-created 'ai' agent could
  -- carry an owner (and be authorized as that person) or a 'personal' agent could lose its owner and fall back to
  -- org-wide scope.
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'agents_personal_owner_check' AND conrelid = 'agents'::regclass
  ) THEN
    ALTER TABLE agents ADD CONSTRAINT agents_personal_owner_check
      CHECK ((kind = 'personal') = (owner_user_id IS NOT NULL));
  END IF;
END $$;

-- One personal agent per (org, user): every device of that user shares it, so the person's claims and comments read as
-- ONE agent ("<name> (CLI)"). The per-device key is a project_api_keys row whose `name` is the device name.
CREATE UNIQUE INDEX IF NOT EXISTS uq_agents_personal_owner ON agents (org_id, owner_user_id) WHERE owner_user_id IS NOT NULL;

-- 2. Device authorizations. Never holds a key or a raw device_code: device_code is stored as sha256 hex, and the key is
--    minted in the same transaction that flips the row to 'delivered' (the token endpoint), so the plaintext key exists
--    only in that one HTTP response. Expiry is derived from expires_at (no 'expired' status to keep in sync); the
--    retention sweep deletes rows a day past it.
CREATE TABLE IF NOT EXISTS cli_device_authorizations (
    id                    UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    device_code_hash      VARCHAR(64)  NOT NULL UNIQUE,
    -- 8 characters of the RFC 8628 consonant alphabet, stored without the display dash.
    user_code             VARCHAR(8)   NOT NULL CHECK (user_code ~ '^[BCDFGHJKLMNPQRSTVWXZ]{8}$'),
    status                VARCHAR(20)  NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending', 'approved', 'denied', 'delivered')),
    client                VARCHAR(32)  NOT NULL DEFAULT '',
    client_version        VARCHAR(64)  NOT NULL DEFAULT '',
    device_name           VARCHAR(64)  NOT NULL,
    requested_profile     VARCHAR(32)  NOT NULL DEFAULT '',
    requested_ip          VARCHAR(64)  NOT NULL DEFAULT '',
    requested_user_agent  VARCHAR(255) NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at            TIMESTAMPTZ  NOT NULL,
    poll_interval_seconds INTEGER      NOT NULL DEFAULT 5,
    last_polled_at        TIMESTAMPTZ,
    approved_by_user_id   TEXT         REFERENCES "user"(id) ON DELETE CASCADE,
    organization_id       UUID         REFERENCES organizations(id) ON DELETE CASCADE,
    decided_at            TIMESTAMPTZ,
    api_key_id            UUID         REFERENCES project_api_keys(id) ON DELETE SET NULL,
    delivered_at          TIMESTAMPTZ
);

-- A user_code identifies at most one PENDING request; used/denied/delivered rows may repeat a code later.
CREATE UNIQUE INDEX IF NOT EXISTS uq_cli_device_authorizations_pending_user_code
    ON cli_device_authorizations (user_code) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_cli_device_authorizations_expires_at ON cli_device_authorizations (expires_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS cli_device_authorizations;

-- Personal agents cannot survive the narrower CHECK; their keys go with them (project_api_keys.agent_id cascades).
DELETE FROM agents WHERE kind = 'personal';
DROP INDEX IF EXISTS uq_agents_personal_owner;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'agents_personal_owner_check' AND conrelid = 'agents'::regclass
  ) THEN
    ALTER TABLE agents DROP CONSTRAINT agents_personal_owner_check;
  END IF;
  IF EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'agents_kind_check' AND conrelid = 'agents'::regclass
  ) THEN
    ALTER TABLE agents DROP CONSTRAINT agents_kind_check;
  END IF;
  ALTER TABLE agents ADD CONSTRAINT agents_kind_check CHECK (kind IN ('ai', 'bot'));
END $$;

ALTER TABLE agents DROP COLUMN IF EXISTS owner_user_id;
-- +goose StatementEnd
