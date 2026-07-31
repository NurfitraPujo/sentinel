-- +goose Up
-- +goose StatementBegin
-- Event idempotency key (P9-3 / IDEMPOTENCY_PLAN.md D-b). `error_occurrences` has no unique constraint
-- of any kind today, so a partial-failure redelivery (NAK after the issue upsert commits but before the
-- occurrence insert) reprocesses the same bytes from zero and inflates `issues.count` — the defect this
-- column exists to close (assigned S18 in VERIFIED_STATE.md).
--
-- IF NOT EXISTS / guarded DO blocks throughout: same re-runnability rationale as every migration before
-- this one (1722100000 in particular). Production (cmd/migrate) uses a single schema_migrations ledger —
-- -target selects only the DSN env var — but tests/integration/db_migrations_test.go replays every Up
-- under multiple independent ledger names (processor_migrations, dashboard_migrations, ...) against ONE
-- physical database, so an `up` must tolerate an already-migrated schema. That test-suite reality, not
-- a production topology, is what the guards serve.
ALTER TABLE error_occurrences ADD COLUMN IF NOT EXISTS event_id VARCHAR(64);

-- '' is what proto3 hands us for "absent" — there is no wire-level NULL. Absent-id events must keep
-- today's semantics exactly (every event stores; duplicates possible, because that IS the pre-W0
-- in-flight population: the 72h JetStream window, the 30-day DLQ reservoir, any rolled-back ingestor).
-- The mapping that protects that population is NULLIF($n, '') at the INSERT site (store.go, W2) — this
-- CHECK is NOT that mapping, it is the tripwire for any future statement that bypasses it. Without
-- NULLIF, three empty-id events silently collide onto (issue_id, '') via the partial unique index below:
-- the first stores, the other two return `INSERT 0 0`, get reported `stored=false`, and ACK as
-- successful no-ops — a proven, silent loss (F-TX-1/F-CT-1 in IDEMPOTENCY_PLAN.md). This CHECK turns any
-- regression that reintroduces that bug into 23514 (class 23 → Permanent via classifyStoreError) — loud
-- and dead-lettered, never silent.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'error_occurrences'::regclass
           AND conname = 'ck_error_occurrences_event_id_nonempty'
    ) THEN
        ALTER TABLE error_occurrences
            ADD CONSTRAINT ck_error_occurrences_event_id_nonempty
            CHECK (event_id IS NULL OR length(event_id) > 0);
    END IF;
END $$;

-- Deliberately plain, NOT CONCURRENTLY (F-CT-8 vs F-TX-6 in IDEMPOTENCY_PLAN.md — reviewers disagreed,
-- this is the accepted trade). CONCURRENTLY cannot run inside a transaction: goose wraps every migration
-- in one (no migration in this repo uses goose's no-transaction directive — which, NOTE, must never be
-- written out literally in a comment here: goose scans EVERY comment line for its annotation marker and
-- fails the whole file with "invalid annotation" if it appears mid-sentence; this file originally
-- shipped with the directive quoted in this very paragraph and could not be parsed by cmd/migrate at
-- all). BOTH raw test migration runners additionally execute the entire Up section as a single
-- multi-statement `pool.Exec` — an implicit transaction (tests/integration/setup_test.go:311,
-- tests/integration/testcontainers/setup.go:340) — so a CONCURRENTLY build could not even be exercised
-- by the test suite that gates this repo. The accepted cost is a brief ACCESS-EXCLUSIVE-grade stall of
-- inserts against error_occurrences while the index builds, judged acceptable at this table's current
-- size.
--
-- Escape hatch, if this ever ships against a production-sized table — all FOUR parts together, none
-- alone: (1) CONCURRENTLY, (2) goose's no-transaction directive on this file, (3) test-runner
-- support for running such a migration outside an implicit transaction, (4) a post-build check of
-- `pg_index.indisvalid` (a failed CONCURRENTLY build leaves an INVALID index that silently stops
-- enforcing uniqueness while still existing).
--
-- If this Up ever fails with SQLSTATE 23505 ("could not create unique index"): rows with duplicate
-- (issue_id, event_id) pairs exist — possible only if the index was dropped while writes continued.
-- Deduplicate them first, then re-run; IF NOT EXISTS does not make an index BUILD tolerant of existing
-- duplicates. The failure is atomic (column and CHECK roll back with it; the ledger does not advance),
-- so a failed attempt leaves nothing half-applied.
--
-- Deploy-ordering contract (F-CT-5): this migration MUST be applied before any processor build that
-- writes `event_id` is deployed. A processor writing the column before it exists gets 42703, which
-- errors.go/classifyStoreError leaves retryable — every event then burns its full NATS delivery budget
-- before dead-lettering. This exact failure mode shipped once already for a different column; see the
-- comment at apps/processor-go/store/store.go:204. The reverse order (migration first, processor still
-- old) is safe: the old processor never reads or writes the `event_id` field the wire hasn't started
-- sending yet.
-- Compose enforces migrate-then-processor via `depends_on: migrate: service_completed_successfully`.
-- Rollback is one-way safe: an old processor never writes this column, so a migrated schema running
-- under old code is simply an unread nullable column; no down-migration is needed purely to roll the
-- processor back.
CREATE UNIQUE INDEX IF NOT EXISTS uq_error_occurrences_issue_event
    ON error_occurrences (issue_id, event_id) WHERE event_id IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS uq_error_occurrences_issue_event;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'error_occurrences'::regclass
           AND conname = 'ck_error_occurrences_event_id_nonempty'
    ) THEN
        ALTER TABLE error_occurrences DROP CONSTRAINT ck_error_occurrences_event_id_nonempty;
    END IF;
END $$;

ALTER TABLE error_occurrences DROP COLUMN IF EXISTS event_id;
-- +goose StatementEnd
