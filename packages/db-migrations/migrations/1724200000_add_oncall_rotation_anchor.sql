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
-- 1. handoff_weekday is ISO 8601 -- 1 = Monday .. 7 = Sunday. 1724100000 wrote it 0-6 (0 =
--    Sunday), which cannot store Sunday under the encoding SR-4 settles. Any pre-existing row is
--    migrated 0 -> 7 WHILE NO CHECK IS IN FORCE -- see ORDERING below. 1724100000's own CHECK is
--    the union of both ranges (0-7) so that replaying it after this file cannot re-narrow what
--    this file has superseded; the ISO 1-7 range that judges every application write is the one
--    installed HERE, and this file always runs after it in the same directory (BUG-03).
-- 2. rotation_anchor_at is the explicit anchor SR-4 requires: the rotation index is the count of
--    wall-clock handoff boundaries at or before T since the FIRST boundary at or after this
--    column. It is deliberately NOT created_at, because §4.4a's PUT is create-or-replace and
--    saving the editor to fix a typo must not re-phase every future handoff.
--
-- ORDERING (BUG-01). The re-encode must sit BETWEEN the DROP and the ADD, not before the DO block.
-- The constraint that judges `SET handoff_weekday = 7` is the one in force AT THE MOMENT OF THE
-- WRITE, and that is the OLD one. An earlier revision of this file ran the UPDATE first and
-- asserted the opposite in a comment; on postgres:15-alpine it aborts with `new row for relation
-- "oncall_schedules" violates check constraint "oncall_schedules_handoff_weekday_check"`. The Down
-- direction had the mirror-image bug (writing 0 under this file's own 1-7 CHECK). Both now
-- re-encode inside the DO block, in the window where the table carries no weekday CHECK at all.
--
-- BUG-03 widened 1724100000's CHECK to 0-7, which does NOT make this ordering optional: any
-- database that already applied 1724100000 in its pre-BUG-03 shape still carries the physical
-- `BETWEEN 0 AND 6` constraint -- goose will not re-run an applied version to relax it -- and that
-- is precisely the database this UPDATE exists to migrate. Down is unconditionally dependent on
-- the ordering in every case.
--
-- IDEMPOTENCY (A1): one flat migration directory serves several goose ledgers against the SAME
-- physical database, so this file is replayed per target. ADD COLUMN IF NOT EXISTS; a
-- pg_constraint catalog guard around the CHECK swap, in the same shape 1724100000 copied from
-- 1722900000_add_agents.sql:35-47 (unconditional DROP inside the guard, then ADD). The UPDATE is
-- idempotent by its own WHERE clause -- after the first run no row matches.

-- SR-4's explicit anchor column. NOT NULL DEFAULT NOW() so an existing schedule keeps resolving:
-- backfilled rows anchor at the moment of the migration, which is the only defensible answer for
-- a rotation that had no recorded phase.
ALTER TABLE oncall_schedules
  ADD COLUMN IF NOT EXISTS rotation_anchor_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'oncall_schedules_handoff_weekday_check' AND conrelid = 'oncall_schedules'::regclass
  ) THEN
    ALTER TABLE oncall_schedules DROP CONSTRAINT oncall_schedules_handoff_weekday_check;
  END IF;

  -- Re-encode any row written while the CHECK was 0-6, now that no weekday CHECK is in force.
  -- Under that encoding 0 meant Sunday; under ISO Sunday is 7. Every other value (1-6,
  -- Monday-Saturday) means the same day in both, so only 0 moves.
  UPDATE oncall_schedules SET handoff_weekday = 7 WHERE handoff_weekday = 0;

  -- ISO 8601 weekday: 1 = Monday .. 7 = Sunday. Null unless kind = 'weekly' (§4.6).
  ALTER TABLE oncall_schedules ADD CONSTRAINT oncall_schedules_handoff_weekday_check CHECK (
    (kind = 'weekly' AND handoff_weekday IS NOT NULL AND handoff_weekday BETWEEN 1 AND 7)
    OR (kind = 'daily' AND handoff_weekday IS NULL)
  );
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Reverse of the above: restore the CHECK 1724100000 installs, with Sunday re-encoded back to 0
-- in the same no-constraint window (BUG-01, see ORDERING above), then drop the anchor column.
--
-- The range mirrors 1724100000's, which is `BETWEEN 0 AND 7` (BUG-03: it accepts the union of
-- both encodings so that replaying it after this file cannot re-narrow a constraint this file has
-- superseded). Down therefore lands on exactly the state 1724100000's Up leaves, whichever of the
-- two runs last -- the rows themselves are what carry the old encoding back, and after the UPDATE
-- below none of them is 7.
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'oncall_schedules_handoff_weekday_check' AND conrelid = 'oncall_schedules'::regclass
  ) THEN
    ALTER TABLE oncall_schedules DROP CONSTRAINT oncall_schedules_handoff_weekday_check;
  END IF;

  UPDATE oncall_schedules SET handoff_weekday = 0 WHERE handoff_weekday = 7;

  ALTER TABLE oncall_schedules ADD CONSTRAINT oncall_schedules_handoff_weekday_check CHECK (
    (kind = 'weekly' AND handoff_weekday IS NOT NULL AND handoff_weekday BETWEEN 0 AND 7)
    OR (kind = 'daily' AND handoff_weekday IS NULL)
  );
END $$;

ALTER TABLE oncall_schedules DROP COLUMN IF EXISTS rotation_anchor_at;
-- +goose StatementEnd
