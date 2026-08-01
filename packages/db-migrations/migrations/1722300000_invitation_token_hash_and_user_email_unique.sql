-- +goose Up
-- +goose StatementBegin
-- D06: organization_invitations.token was stored PLAINTEXT, and the token also leaked into the
-- ?token= query string on the redemption round trip (browser history, Referer headers, OAuth
-- redirectTo). Fix: store only sha256(token) and look up invitations by that hash; the emailed URL
-- keeps the raw token in the path (never persisted) and the application layer stops putting it in a
-- query string.
--
-- DECISION (made by the user, not inferred here): plaintext cannot be turned into a hash of the
-- original secret after the fact for existing PENDING invitations -- there is nothing to hash
-- retroactively that recovers the security property, because the whole point is that the plaintext
-- token must never have been the thing sitting in the row. So instead of a backfill, this migration
-- simply DELETES every currently pending invitation. Any outstanding invite link a recipient has not
-- yet used becomes invalid; the inviter must re-send it. Non-pending rows (accepted / revoked /
-- expired) are kept for their audit trail -- they are never looked up by token again, so their
-- token_hash is left NULL rather than fabricated.
--
-- IDEMPOTENCY: every statement below is guarded. This repo keeps ONE flat migration directory for
-- all goose targets (A1 in docs/memory/ARCHITECTURE.md), and each target has its OWN ledger
-- (schema_migrations, processor_migrations, dashboard_migrations, ...) tracking the SAME physical
-- database. So a second target replays this file against a database where it has already been
-- applied. Unguarded DDL fails there with "column already exists" and takes the whole migration
-- run down -- which is exactly what broke the `integration` CI job (TestMigrationStatus,
-- TestSequentialMigrations, TestTargetIsolation, TestBaselineCommand). Every other migration in
-- this directory follows the same IF NOT EXISTS convention.
DELETE FROM organization_invitations WHERE status = 'pending';

ALTER TABLE organization_invitations ADD COLUMN IF NOT EXISTS token_hash VARCHAR(64);
ALTER TABLE organization_invitations DROP COLUMN IF EXISTS token;

-- D07: redemption becomes a single conditional UPDATE ... SET status='accepted', accepted_at=now()
-- WHERE token_hash=$1 AND status='pending' AND expires_at > now() RETURNING *. That needs somewhere
-- to record when acceptance happened.
ALTER TABLE organization_invitations ADD COLUMN IF NOT EXISTS accepted_at TIMESTAMP;

-- Multiple NULLs are permitted under a UNIQUE constraint in Postgres, so the historical rows left
-- with token_hash = NULL above do not collide with each other or with future real hashes.
-- ADD CONSTRAINT has no IF NOT EXISTS in Postgres, so guard it by catalog lookup.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'organization_invitations_token_hash_unique'
    ) THEN
        ALTER TABLE organization_invitations
            ADD CONSTRAINT organization_invitations_token_hash_unique UNIQUE (token_hash);
    END IF;
END $$;

-- D30: "user".email has no unique/citext constraint, so an email->userId resolution done with
-- `LIMIT 1` picks an arbitrary row when duplicates exist, and a case-variant address
-- (Bob@X.com vs bob@x.com) was invisible to any lower(email) comparison. This is what let an
-- already-a-member invitee bypass the "already a member" guard.
--
-- DECISION (made by the user): fail loudly if duplicate rows already exist rather than guessing
-- which one should win a merge -- silently picking one is a data-loss decision for someone else to
-- make deliberately, not this migration.
DO $$
DECLARE
    dup_count INTEGER;
BEGIN
    SELECT COUNT(*) INTO dup_count FROM (
        SELECT lower(email) FROM "user" GROUP BY lower(email) HAVING COUNT(*) > 1
    ) dupes;

    IF dup_count > 0 THEN
        RAISE EXCEPTION 'Cannot add unique index on lower("user".email): % duplicate email group(s) already exist. Resolve the duplicates manually, then re-run this migration.', dup_count;
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_email_lower_unique ON "user" (lower(email));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_user_email_lower_unique;
ALTER TABLE organization_invitations DROP COLUMN IF EXISTS accepted_at;
ALTER TABLE organization_invitations DROP CONSTRAINT IF EXISTS organization_invitations_token_hash_unique;
ALTER TABLE organization_invitations ADD COLUMN IF NOT EXISTS token VARCHAR(128);
ALTER TABLE organization_invitations DROP COLUMN IF EXISTS token_hash;
-- +goose StatementEnd
