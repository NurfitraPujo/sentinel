-- +goose Up
-- +goose StatementBegin
-- N9 (docs/plans/AGENT_WORKER_PLAN.md contract corrections C4/C5, server-side prerequisite for the
-- N8 sentinel-worker plan): client-supplied idempotency keys for agent write endpoints. N7d's
-- natural-key dedupe (agent-dedupe.ts) is an exact-body match within a 2-minute window and, by
-- design, does NOT cover blocking questions -- so a crashed agent that retries a question >2min
-- later, or with reworded text, both duplicates the comment AND double-emails the reporter (the
-- question_asked kind bypasses the 15-min email throttle). This table lets a client stamp each
-- logical write with a per-job UUID; a repeat key replays the ORIGINAL result with no second side
-- effect and no second email.
--
-- Scope is (agent_id, idempotency_key): the key space is the CALLING AGENT's (B7 -- agent_id here
-- always comes from the credential, never a request field), so two different agents may reuse the
-- same UUID without colliding. `op` records which write the key belongs to so a key accidentally
-- reused across two different operations is a detectable client error rather than a mis-shaped
-- replay. `comment_id` is the only original-result reference we need to replay: comment/question
-- ops re-fetch that row (preserving the waiting_on-predictability rationale -- a question hit
-- returns the ORIGINAL question's comment id, D21), and progress ops store NULL (their result is a
-- bare success). No FK on comment_id on purpose: if the original comment is later deleted the
-- dedupe guard must still stand (a retry must not resurrect a deleted comment via a fresh write);
-- the row is instead aged out by the 7-day reaper (retention.ts reapExpiredIdempotencyKeys).
--
-- IDEMPOTENCY (A1): one flat migration directory serves several goose ledgers against the SAME
-- physical database, so this file is replayed per target. `CREATE TABLE IF NOT EXISTS` (with the
-- UNIQUE + PK declared inline) and `CREATE INDEX IF NOT EXISTS` are all replay-safe on their own --
-- no bare `ADD CONSTRAINT` (which has no IF NOT EXISTS form and would need a pg_constraint guard).
CREATE TABLE IF NOT EXISTS agent_idempotency_keys (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	agent_id VARCHAR(255) NOT NULL,
	idempotency_key VARCHAR(255) NOT NULL,
	op VARCHAR(50) NOT NULL,
	comment_id UUID,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	CONSTRAINT agent_idempotency_keys_agent_key_unique UNIQUE (agent_id, idempotency_key)
);
-- +goose StatementEnd

-- +goose StatementBegin
-- Reaper predicate is `created_at < cutoff`; a btree on created_at keeps the 7-day sweep from a
-- full scan as the table grows with agent traffic.
CREATE INDEX IF NOT EXISTS idx_agent_idempotency_keys_created_at ON agent_idempotency_keys (created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS agent_idempotency_keys;
-- +goose StatementEnd
