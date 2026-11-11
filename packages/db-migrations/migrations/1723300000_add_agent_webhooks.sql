-- +goose Up
-- +goose StatementBegin
-- N1a (AI-agent-native Sentinel): outbound webhook subscriptions that let an agent get pushed
-- events instead of polling the issue_activity.seq cursor. IDEMPOTENCY: same rationale as every
-- migration in this repo (A1) -- one flat directory serves several goose ledgers against the SAME
-- physical database, so this file is replayed per target. `CREATE TABLE IF NOT EXISTS` and
-- `IF NOT EXISTS` indexes; a pg_constraint catalog guard for the status CHECK (Postgres has no
-- `ADD CONSTRAINT IF NOT EXISTS`).
--
-- `secret` is stored in PLAINTEXT, unlike project_api_keys/agent keys which only ever store a
-- hash. This is a deliberate divergence, not an oversight: an API key is a credential the SERVER
-- verifies (compare an incoming hash), so only the hash need ever be stored. A webhook secret is
-- a credential the SERVER SIGNS with (HMAC the outbound payload so the receiver can verify it came
-- from Sentinel) -- signing requires the raw secret at delivery time, so hashing it would make
-- delivery impossible. `secret_prefix` is kept alongside for display/rotation UX (matches the
-- existing project_api_keys pattern) without ever re-exposing the full secret in a listing.
CREATE TABLE IF NOT EXISTS agent_webhooks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    agent_id UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    url TEXT NOT NULL,
    secret TEXT NOT NULL,
    secret_prefix VARCHAR(16) NOT NULL,
    event_types TEXT[] NOT NULL DEFAULT '{}',
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    last_delivered_seq BIGINT NOT NULL DEFAULT 0,
    consecutive_failures INT NOT NULL DEFAULT 0,
    last_attempt_at TIMESTAMPTZ,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'agent_webhooks_status_check' AND conrelid = 'agent_webhooks'::regclass
  ) THEN
    ALTER TABLE agent_webhooks ADD CONSTRAINT agent_webhooks_status_check CHECK (
      status IN ('active', 'disabled', 'failed')
    );
  END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_agent_webhooks_agent ON agent_webhooks (agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_webhooks_status_active ON agent_webhooks (status) WHERE status = 'active';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS agent_webhooks;

-- +goose StatementEnd
