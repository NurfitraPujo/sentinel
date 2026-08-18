-- +goose Up
-- +goose StatementBegin
-- N10 part 2 (docs/plans/AGENT_WORKER_PLAN.md §4.5 "Git credentials are server-side too"):
-- encrypted git-credentials store. IDEMPOTENCY: same rationale as every migration in this repo
-- (A1) -- one flat directory serves several goose ledgers against the SAME physical database, so
-- this file is replayed per target. `CREATE TABLE IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`,
-- `IF NOT EXISTS` indexes; pg_constraint catalog guards for CHECKs.
--
-- `encrypted_secret` is AES-256-GCM ciphertext (base64, auth tag appended) under the
-- SENTINEL_ENCRYPTION_KEY master key -- NEVER plaintext. This is a deliberate divergence from
-- agent_webhooks.secret (plaintext, signing-only): these credentials authorize repository WRITES,
-- so at-rest exposure is a direct push-access breach. `nonce` is the per-row random 96-bit GCM
-- nonce (base64); `key_version` identifies which master key encrypted the row so the master key
-- can be rotated without re-encrypting everything at once. The plaintext inside the envelope is a
-- JSON object: {"token": "..."} (github, or bitbucket access token) or
-- {"username": "...", "appPassword": "..."} (bitbucket app password pair).
-- `secret_prefix` is a short display fragment (token prefix or bitbucket username) for the
-- write-only UI -- the full secret is never shown again after set, not even once.
-- On revoke the ciphertext and nonce are overwritten with '' (crypto-shredding lite); the row is
-- kept for the audit trail.
CREATE TABLE IF NOT EXISTS repo_credentials (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    provider VARCHAR(20) NOT NULL,
    label VARCHAR(255) NOT NULL,
    secret_prefix VARCHAR(16) NOT NULL,
    encrypted_secret TEXT NOT NULL,
    nonce TEXT NOT NULL,
    key_version SMALLINT NOT NULL DEFAULT 1,
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    created_by VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ,
    last_fetched_at TIMESTAMPTZ
);

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'repo_credentials_provider_check' AND conrelid = 'repo_credentials'::regclass
  ) THEN
    ALTER TABLE repo_credentials ADD CONSTRAINT repo_credentials_provider_check CHECK (
      provider IN ('github', 'bitbucket')
    );
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'repo_credentials_status_check' AND conrelid = 'repo_credentials'::regclass
  ) THEN
    ALTER TABLE repo_credentials ADD CONSTRAINT repo_credentials_status_check CHECK (
      status IN ('active', 'revoked')
    );
  END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_repo_credentials_org ON repo_credentials (organization_id);
CREATE INDEX IF NOT EXISTS idx_repo_credentials_org_active
    ON repo_credentials (organization_id) WHERE status = 'active';

-- Admin-set delivery gate (N10): a plain agent key must NOT receive repo credentials. Default
-- false -- access is an explicit per-agent grant toggled in the agent management UI.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS can_access_repo_credentials BOOLEAN NOT NULL DEFAULT false;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE agents DROP COLUMN IF EXISTS can_access_repo_credentials;
DROP TABLE IF EXISTS repo_credentials;

-- +goose StatementEnd
