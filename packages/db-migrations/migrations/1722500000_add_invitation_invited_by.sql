-- +goose Up
-- +goose StatementBegin
-- D31 (residual): the role on an invitation is validated against the ORG_ROLES allowlist at
-- redemption (1722300000), but nothing re-validates that the ORIGINAL INVITER still has authority
-- to grant that role. A pending 'owner' invitation issued by an owner who is later demoted or
-- removed still redeemed as 'owner' for up to 7 days -- the grant outlived the granter's authority.
--
-- Closing this needs to know who issued the invitation, which nothing on this table recorded.
-- invited_by is nullable and ON DELETE SET NULL: an inviter's user row can be deleted (or the
-- column can simply be unset for pre-existing rows created before this migration) without
-- corrupting the invitation row itself. claimInvitation treats invited_by IS NULL as "cannot verify
-- the inviter's current authority" and refuses the redemption -- fail closed, not fail open.
--
-- IDEMPOTENCY: guarded with IF NOT EXISTS. One flat migration directory serves several goose
-- ledgers against the same physical database (A1), so this file is replayed per target.
-- "user".id is TEXT (better-auth's default, not this codebase's usual UUID), and every existing
-- *_user_id FK column in this schema (organization_members.user_id, project_members.user_id, ...)
-- matches it with VARCHAR(255) rather than UUID/TEXT -- kept consistent with that convention.
ALTER TABLE organization_invitations
    ADD COLUMN IF NOT EXISTS invited_by VARCHAR(255) REFERENCES "user"(id) ON DELETE SET NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE organization_invitations DROP COLUMN IF EXISTS invited_by;
-- +goose StatementEnd
