-- +goose Up
-- +goose StatementBegin
-- N7c (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md, A03): agent claims on an issue
-- (issues.assigned_to/assignee_type) have no timestamp, so nothing can tell a fresh claim from
-- one an agent abandoned mid-loop -- an unattended agent that crashes or hangs leaves the issue
-- claimed forever. This column records when the current claim was made; the reaper
-- (retention.ts reapStaleClaims) force-releases agent claims whose claimed_at is older than
-- CLAIM_STALE_HOURS with no recent activity from the claimant.
--
-- No backfill: pre-existing claims get claimed_at = NULL, which the reaper treats as
-- stale-eligible (see reapStaleClaims) rather than backdating them to an arbitrary value.
--
-- IDEMPOTENCY (A1): one flat migration directory serves several goose ledgers against the SAME
-- physical database, so this file is replayed per target -- `ADD COLUMN IF NOT EXISTS` is
-- idempotent on its own, unlike `ADD CONSTRAINT`.
ALTER TABLE issues ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE issues DROP COLUMN IF EXISTS claimed_at;
-- +goose StatementEnd
