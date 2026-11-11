-- +goose Up
-- +goose StatementBegin
-- Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8): subscriptions + notifications.
--
-- IDEMPOTENCY: one flat migration directory serves several goose ledgers against the same
-- physical database (A1), so this file is replayed per target -- every statement below is
-- written to be a no-op on a schema that already has it applied (IF NOT EXISTS on
-- tables/indexes; a pg_constraint catalog guard for CHECKs, since Postgres has no
-- ADD CONSTRAINT IF NOT EXISTS).

-- §8: who gets notified about an issue. subscriber_id is NOT an FK to "user"/agents -- it can
-- name either a user or an agent depending on subscriber_type, exactly like issue_comments'
-- author_type/author_id pair, so it stays a plain VARCHAR the same way that column does.
CREATE TABLE IF NOT EXISTS issue_subscriptions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    issue_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    subscriber_type VARCHAR(20) NOT NULL,
    subscriber_id VARCHAR(255) NOT NULL,
    reason VARCHAR(20) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'issue_subscriptions_subscriber_type_check' AND conrelid = 'issue_subscriptions'::regclass
  ) THEN
    ALTER TABLE issue_subscriptions ADD CONSTRAINT issue_subscriptions_subscriber_type_check
      CHECK (subscriber_type IN ('user', 'agent'));
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'issue_subscriptions_reason_check' AND conrelid = 'issue_subscriptions'::regclass
  ) THEN
    ALTER TABLE issue_subscriptions ADD CONSTRAINT issue_subscriptions_reason_check
      CHECK (reason IN ('reporter', 'claimant', 'participant', 'manual'));
  END IF;
END $$;

-- Idempotent auto-subscribe (createComment/claimIssue/etc. upsert on this) and the manual
-- toggle's own dedup, both in one place: one row per (issue, subscriber).
CREATE UNIQUE INDEX IF NOT EXISTS idx_issue_subscriptions_unique
  ON issue_subscriptions (issue_id, subscriber_type, subscriber_id);

-- §8: the fan-out target. user_id is a real FK ("user".id, better-auth's TEXT id) -- unlike
-- issue_subscriptions above, a notifications row is ALWAYS a user's inbox entry in M4 (agent
-- subscribers get no row yet -- they poll, per design; see notify.ts).
CREATE TABLE IF NOT EXISTS notifications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    issue_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    kind VARCHAR(30) NOT NULL,
    actor_type VARCHAR(20) NOT NULL,
    actor_id VARCHAR(255) NOT NULL,
    payload JSONB,
    read_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'notifications_kind_check' AND conrelid = 'notifications'::regclass
  ) THEN
    ALTER TABLE notifications ADD CONSTRAINT notifications_kind_check CHECK (
      kind IN (
        'commented', 'claimed', 'status_changed', 'resolved', 'linked', 'progress_update',
        'question_asked'
      )
    );
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'notifications_actor_type_check' AND conrelid = 'notifications'::regclass
  ) THEN
    ALTER TABLE notifications ADD CONSTRAINT notifications_actor_type_check
      CHECK (actor_type IN ('user', 'agent', 'system'));
  END IF;
END $$;

-- Bell/unread-count query: WHERE user_id = $1 AND read_at IS NULL.
CREATE INDEX IF NOT EXISTS idx_notifications_user_unread ON notifications (user_id, read_at);
-- Notification list, newest first: WHERE user_id = $1 ORDER BY created_at DESC.
CREATE INDEX IF NOT EXISTS idx_notifications_user_created ON notifications (user_id, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS issue_subscriptions;

-- +goose StatementEnd
