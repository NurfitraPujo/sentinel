-- +goose Up
-- +goose StatementBegin
-- sentinel-user-report-issue SR-4 (contract 05b5a109), the two schema facts SR-4's anchoring
-- correction needs and 1724100000_add_user_report_intake_and_oncall.sql does not yet carry.
--
-- WHY A SECOND FILE rather than an edit to 1724100000: that migration is delivered evidence for
-- another work item (an up/down/up replay diffed byte-identical against it). Amending it under
-- its reviewer would invalidate that evidence; a separate, idempotent migration supersedes the
-- shape without rewriting the record of it.
--
-- 1. handoff_weekday is ISO 8601 -- 1 = Monday .. 7 = Sunday. 1724100000 declared the CHECK as
--    BETWEEN 0 AND 6, which cannot store Sunday under the encoding SR-4 settles. Any pre-existing
--    row is migrated 0 -> 7 BEFORE the CHECK is swapped, so the new constraint can never fail on
--    data written under the old one.
-- 2. rotation_anchor_at is the explicit anchor SR-4 requires: the rotation index is the count of
--    wall-clock handoff boundaries at or before T since the FIRST boundary at or after this
--    column. It is deliberately NOT created_at, because §4.4a's PUT is create-or-replace and
--    saving the editor to fix a typo must not re-phase every future handoff.
--
-- IDEMPOTENCY (A1): one flat migration directory serves several goose ledgers against the SAME
-- physical database, so this file is replayed per target. ADD COLUMN IF NOT EXISTS; a
-- pg_constraint catalog guard around the CHECK swap, in the same shape 1724100000 copied from
-- 1722900000_add_agents.sql:35-47 (unconditional DROP inside the guard, then ADD).

-- SR-4's explicit anchor column. NOT NULL DEFAULT NOW() so an existing schedule keeps resolving:
-- backfilled rows anchor at the moment of the migration, which is the only defensible answer for
-- a rotation that had no recorded phase.
ALTER TABLE oncall_schedules
  ADD COLUMN IF NOT EXISTS rotation_anchor_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- Re-encode any row written while the CHECK was 0-6. Under that encoding 0 meant Sunday; under
-- ISO Sunday is 7. Every other value (1-6, Monday-Saturday) means the same day in both, so only
-- 0 moves. Runs before the swap deliberately -- see the note above.
UPDATE oncall_schedules SET handoff_weekday = 7 WHERE handoff_weekday = 0;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'oncall_schedules_handoff_weekday_check' AND conrelid = 'oncall_schedules'::regclass
  ) THEN
    ALTER TABLE oncall_schedules DROP CONSTRAINT oncall_schedules_handoff_weekday_check;
  END IF;

  -- ISO 8601 weekday: 1 = Monday .. 7 = Sunday. Null unless kind = 'weekly' (§4.6).
  ALTER TABLE oncall_schedules ADD CONSTRAINT oncall_schedules_handoff_weekday_check CHECK (
    (kind = 'weekly' AND handoff_weekday IS NOT NULL AND handoff_weekday BETWEEN 1 AND 7)
    OR (kind = 'daily' AND handoff_weekday IS NULL)
  );
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Reverse of the above, in reverse order: restore 1724100000's 0-6 CHECK, re-encode Sunday back
-- to 0 first so the restored constraint does not fail on ISO data, then drop the anchor column.
UPDATE oncall_schedules SET handoff_weekday = 0 WHERE handoff_weekday = 7;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'oncall_schedules_handoff_weekday_check' AND conrelid = 'oncall_schedules'::regclass
  ) THEN
    ALTER TABLE oncall_schedules DROP CONSTRAINT oncall_schedules_handoff_weekday_check;
  END IF;

  ALTER TABLE oncall_schedules ADD CONSTRAINT oncall_schedules_handoff_weekday_check CHECK (
    (kind = 'weekly' AND handoff_weekday IS NOT NULL AND handoff_weekday BETWEEN 0 AND 6)
    OR (kind = 'daily' AND handoff_weekday IS NULL)
  );
END $$;

ALTER TABLE oncall_schedules DROP COLUMN IF EXISTS rotation_anchor_at;
-- +goose StatementEnd
