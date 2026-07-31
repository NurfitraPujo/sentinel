# Event Idempotency Plan (P9-3) — `event_id` on the processor's write path

*Status: **v2, after adversarial review**. v1 was reviewed by three independent adversarial passes —
transactional/concurrency (F-TX, probes against a real Postgres 15 replica schema), wire-contract/deploy
(F-CT), and test-forcing (F-TP) — before any implementation. §0 records what the review changed and why;
the body below is the amended plan. Executes P9-3 of [E2E_RECOVERY_PLAN.md](E2E_RECOVERY_PLAN.md).*

## 0. Review of v1 — what was wrong with it

Kept because the *reasons* are what stop the next plan repeating them. Findings are cited as F-TX-n /
F-CT-n / F-TP-n throughout the body.

1. **v1's central non-regression promise was false (F-TX-1 + F-CT-1, found independently by both
   reviewers).** v1 claimed pre-W0 in-flight messages are "NULL-safe by the partial index". Proto3 has
   no null: an absent `event_id` deserializes to `""`, and `''` **is** `NOT NULL`, so it enters the
   partial index. Proven with pgx against a replica schema: three distinct pre-W0 events on one issue →
   first stores, the other two return `INSERT 0 0`, get reported `stored=false`, **ACK as successful
   no-ops, and are discarded**. The issue freezes at `count=1` and the loss reports itself as healthy
   dedup (`outcome="duplicate"`). The population v1 promised to protect — the 72h JetStream window, the
   30-day DLQ, any rolled-back ingestor — was exactly the population it destroyed. Fixed by D-b/D-c:
   `NULLIF($n,'')` at the insert site, a CHECK constraint as defence in depth, and a dedicated test.
2. **v1's concurrency mechanism was factually wrong, though its outcome was right (F-TX-2).**
   The claimed "second insert waits on the first's speculative insertion" is unreachable for same-issue
   duplicates: the second tx blocks one statement earlier, on the `issues` row lock taken by
   `ON CONFLICT DO UPDATE`. `pg_locks` shows it. The unique index's real job is the *sequential*
   redelivery-after-commit case. D-c's reasoning is rewritten; W3's tests now say which mechanism each
   one proves.
3. **v1 mis-cited the defect's own name (F-CT-11).** S16 is the ProjectKey secret/name split —
   already resolved. The count-inflation defect has NO S-number; it lives as a "residual, knowingly
   accepted" paragraph under S9, and `CLAUDE.md` + `E2E_RECOVERY_PLAN.md` inherited the mislabel. W4
   assigns it a fresh number (S18) and fixes all three documents; v1's W4 would have corrupted memory.
4. **v1's proof package could go green while asserting nothing (F-TP-1/2/3).** The duplicate outcome
   had no path to the metrics recorder (it derives outcome solely from the returned error — a duplicate
   returns nil → records `stored`); the e2e metrics helper cannot read label values at all; and U36's
   DB assertions ran before any signal that the duplicate had been *processed*, so they'd pass with the
   duplicate still in flight. All three are now specified precisely (D-e, §4).
5. **v1 justified D-a with a retry behavior the SDK does not have (F-CT-6).** `handlePartialFailure`
   reads `result.Failed`, fires `OnError`, and returns — it never re-sends and cannot identify the
   failed subset. The real duplication window is the whole-body re-POST after a lost/timed-out response.
   D-a's table is corrected; the S15 citation is deleted.
6. **v1's B4 warning aimed at the wrong package (F-TP-5).** `tests/unit` contains no store doubles at
   all; the actual interface ripple is ~13 call sites in `tests/integration/processor_store_test.go`
   plus a behavioral-assertion change in `processor_service_test.go`. W2 now carries the real file
   list.
7. **v1 was silent on five things that had to be spoken**: explicit READ COMMITTED (F-TX-3), the
   Normalize hazard that would have mangled every UUID id into `<UUID>` (F-CT-2, a B6 instance), the
   processor-side length guard for non-ingestor producers (F-CT-3), deploy ordering (F-CT-5), and the
   second hand-maintained schema in Drizzle (F-CT-12).
8. **v1's performance risk was backwards (F-TX, measured).** The single-tx shape was benchmarked
   *faster* than today's two-tx shape at every concurrency level on a hot fingerprint (1 client:
   7.1ms vs 13.3ms; 16 clients: 112ms vs 174ms) — it removes a commit round trip and adds no
   serialization (the issues row already serializes both shapes). The speculative "pre-check
   mitigation" is deleted.

Review verdicts: TX — "sound with amendments; three of my four planned attacks failed" (the abort
interleaving, deadlock construction, and throughput attacks all failed against D-c; the type-mismatch
attack landed). CT — "design sound; contains a fifth B5 and a B9, both fixed by amendment". TP — "the
mutation matrix belongs at integration level; one e2e rebuild cycle total".

## 1. The defect, precisely

`issues.count` (and `regression_count`, `last_regressed_at`, and `issue_activity`) can inflate on
partial-failure redelivery. The window is exact:

```
1. ResolveProjectID
2. UpsertIssueWithOutcome      ← its OWN tx; ON CONFLICT ... count = issues.count + 1; COMMITS
3. GetIssueIDByFingerprint
4. dispatchAlert               ← feeds the alert frequency counter
5. InsertOccurrence            ← bare exec on the pool, NO transaction with step 2
6. IndexOccurrence             ← best-effort
```

A failure at step 5 NAKs; NATS redelivers the same bytes; step 2 commits another `count + 1`. Result:
`occurrences = 1`, `count = 2`. Nothing structural prevents it: `error_occurrences` has no unique
constraint of any kind. Step 4 sits *inside* the window, so a redelivery also double-feeds the alert
frequency counter — and feeds it for events that are then never stored.

The idempotency key already exists client-side and is thrown away: `packages/sdk-go/event.go:31` sends
`event_id`; the ingestor never reads it (`tests/contract/sdk_ingestor_test.go` documents the drop);
the proto has no field for it. `apps/processor-go/degradation/buffer.go`'s header records this exact
gap as why the buffer couldn't dedup.

This defect is currently mislabeled "S16" in `CLAUDE.md` and `E2E_RECOVERY_PLAN.md`; it is actually
the S9 residual (`VERIFIED_STATE.md:1016`) and gets its own number in W4 (F-CT-11).

## 2. Decisions

### D-a · The key is `event_id`: client-supplied when valid, minted by the ingestor otherwise, stamped into the message before publish

The key must be identical across every delivery of the same event, different across different events.
Where it is minted decides coverage (table corrected per F-CT-6):

| Minted at | NATS redelivery (the P9-3 defect) | Whole-body re-POST after a lost/timed-out response (`transport.go:169-189`, 5s `doSend` deadline — the ingestor may have published every item before the response was lost) |
|---|---|---|
| Processor | ✗ (new id per delivery) | ✗ |
| Ingestor | ✓ (id rides in the message bytes) | ✗ (each POST gets a fresh id) |
| **SDK, guaranteed by ingestor** | ✓ | ✓ |

The ingestor reads the client's `event_id`; if present and ≤64 chars it is used; otherwise the
ingestor mints a UUIDv4 **at accept time, before the NATS publish**, and stamps it into the proto.
Every consumer — first delivery, redelivery, DLQ replay — sees the same id.

Per-item resend does not exist in the SDK today (`handlePartialFailure` never re-sends and cannot
identify the failed subset — F-CT-6), so per-item ids buy nothing beyond the whole-body case; recorded
here so nobody cites batch-subset retry as a benefit again.

**Invalid-id policy (F-CT-10, decided):** re-mint, but never silently.
- 400-reject is strictly worse than today: the SDK treats 4xx as non-retryable and drops the whole
  batch (`transport.go:191-198`) — a client with a 65-char id would lose 100% of its events instead of
  losing only dedup. Truncation silently merges distinct events. Hashing oversized ids adds a second
  derivation contract — a B5 generator.
- So: mint, plus (a) `sentinel_ingest_event_id_replaced_total{reason}` with `reason` ∈
  {`too_long`, `empty`, `invalid_chars`} — never the id itself (D15 cardinality rule); (b) one log
  line with the offending *length* and project — WARN for the anomalous reasons, Debug for `empty`
  (the designed-for case; WARN there would equal ingest volume — F-VW0-4); (c) the 202 body echoes
  the **effective** id —
  `{"status":"accepted","event_id":"…"}` and per-item in the batch response — so a client can diff
  what it sent against what was used. Additive response keys are backwards-compatible; D-f is amended
  accordingly.
- JSON-type hazard, decided deliberately: `ErrorPayload.EventID string` means `"event_id": 12345`
  (a number) fails the whole-body decode → 400. That is consistent with every other field's type
  mismatch behavior, and only our SDK sends the field today (as a string). Accepted; W0 documents it
  in the payload struct comment.
- Two boundary rules found by W0's validation, both executed rather than assumed (F-VW0-1/2): the
  ingestor's length guard counts **runes**, because the other two enforcement points both count
  characters — CEL `.size()` counts code points (proven: 64×'ä' = 128 bytes passes, 65 runes fails)
  and `VARCHAR(64)` counts characters — so a byte count would strip multibyte clients of dedup with a
  false `too_long`. And ids carrying **control characters** are minted over (`invalid_chars`): NUL
  passes JSON decoding and protovalidate but cannot be stored in a Postgres varchar, so passing it
  through would let a client dead-letter its own events once W2 writes the column.

Trust boundary: the key is scoped per issue (D-b) and issues are tenant-scoped, so a hostile client
can only suppress its own events (B7 posture: the client value never crosses tenants).

### D-b · The key lives ON `error_occurrences`, scoped `(issue_id, event_id)`, partial unique index — with the empty string mapped to NULL at the boundary

```sql
ALTER TABLE error_occurrences ADD COLUMN IF NOT EXISTS event_id VARCHAR(64);

-- '' is what proto3 hands us for "absent" (F-TX-1/F-CT-1); it must NEVER reach this column.
-- NULLIF at the insert site is the mapping; this CHECK is the tripwire for any future statement
-- that bypasses it: 23514 is class 23 → Permanent via classifyStoreError → loud, not silent.
ALTER TABLE error_occurrences ADD CONSTRAINT ck_error_occurrences_event_id_nonempty
    CHECK (event_id IS NULL OR length(event_id) > 0);  -- guarded DO block for re-runnability

CREATE UNIQUE INDEX IF NOT EXISTS uq_error_occurrences_issue_event
    ON error_occurrences (issue_id, event_id) WHERE event_id IS NOT NULL;
```

- **The `"" → NULL` mapping is load-bearing, not cosmetic.** Absent ids must behave exactly as today
  (every event stores; duplicates possible) — that is the pre-W0 in-flight window, the 30-day DLQ
  reservoir, and any rolled-back ingestor. Proven: with NULLIF, three empty-id events store 3/3; without
  it, 1/3 and two silent losses (F-TX-1).
- **No second table.** A `processed_events` ledger is a second moving part with its own retention and a
  new way for two sources of truth to disagree. The occurrence row IS the fact "this event was stored".
- **Scoped per issue, not global.** Global unique lets project A's `evt_123` suppress project B's.
  `issue_id` is the tenant-scoped column that already exists; identical bytes → same deterministically
  computed fingerprint (`event/event.go:133`) → same issue.
- **Index build is plain, NOT `CONCURRENTLY` — a deliberate trade recorded here (F-CT-8 vs F-TX-6,
  reviewers disagreed).** `CONCURRENTLY` cannot run in a transaction: goose wraps migrations in one
  (this repo has no `-- +goose NO TRANSACTION` precedent), and **both** test migration runners execute
  the whole Up section as a single multi-statement `pool.Exec` — an implicit transaction
  (`tests/integration/setup_test.go:311`, `tests/integration/testcontainers/setup.go:318`), so a
  CONCURRENTLY migration cannot even be tested. The cost is a brief ACCESS-EXCLUSIVE-grade stall of
  inserts during the build — accepted at this table's current size. Escape hatch if this ever ships
  against a production-sized table: `CONCURRENTLY` + `-- +goose NO TRANSACTION` + test-runner support
  + an `indisvalid` check (a failed concurrent build leaves an INVALID index) — all four together.
- **Dedup horizon, stated honestly (F-CT-7):** guaranteed within `min(DATA_RETENTION_DAYS, DLQ
  MaxAge=30d)`. They are equal at defaults; an operator lowering retention below 30d accepts that a
  DLQ replay older than retention re-stores and re-increments. Retention's SQL needs no change (filters
  on `created_at`, cascades; the new column is inert to it) — and retention already never decrements
  `count`, so `count > occurrences` post-retention is pre-existing product semantics, not this plan's.
- **Known limit, accepted:** same `event_id` with *different* payloads (a client bug) can land in two
  issues and will not dedup.
- `VARCHAR(64)` errors rather than truncates on overflow (22001, class 22 → already Permanent) — the
  S3/S14 family stays closed *provided the bounds guard also exists processor-side* (D-g).

### D-c · One transaction for the whole write; a duplicate aborts it

```
StoreEvent(ctx, issue, occurrence, releaseVersion) → (outcome IssueOutcome, stored bool, err error)
```

Inside one transaction, begun explicitly with `pgx.TxOptions{IsoLevel: pgx.ReadCommitted}`:
1. Read existing issue state; upsert/update the issue exactly as today (count, regression bookkeeping,
   `issue_activity`), folding `RETURNING id` into the upsert.
2. `INSERT INTO error_occurrences (..., event_id) VALUES (..., NULLIF($n,'')) ON CONFLICT
   (issue_id, event_id) WHERE event_id IS NOT NULL DO NOTHING`, executed via `tx.Exec`, duplicate
   detected by `CommandTag.RowsAffected() == 0`.
3. 0 rows → ROLLBACK → return `stored=false, nil`. The rollback undoes this delivery's count and
   regression writes; the message ACKs as a successful no-op.
4. Otherwise COMMIT.

Correctness properties, each with its evidence:
- **Atomicity kills the original window**: any failure rolls back everything; redelivery reprocesses
  from zero. The unique index handles the other direction — committed-but-ACK-lost redelivery.
- **Concurrency (corrected, F-TX-2):** the serialization point is the **`issues` row**, not the
  occurrence index. Two deliveries of the same event target the same `issue_id`, so the second blocks
  on the first's `ON CONFLICT DO UPDATE` row lock and reaches the occurrence insert only after the
  first commits/aborts. Probed: commit-first → second re-reads count, inserts 0 rows, rolls back; final
  `count=1+1, occs=1` per event. Abort-first → second stores normally, **no phantom duplicate**
  (probed). The index is the sole mechanism only in the sequential redelivery-after-commit case.
- **READ COMMITTED is required, not assumed (F-TX-3):** under REPEATABLE READ/SERIALIZABLE the same
  interleaving throws 40001 (probed), which `classifyStoreError` leaves retryable → a hot fingerprint
  converts contention into a NAK/retry storm. Hence the explicit `TxOptions`; W3 asserts the level.
- **The conflict target is mandatory and load-bearing twice (F-TX-5):** it is the only form that
  matches a partial unique index (probed: three wrong forms all error loudly), AND it keeps a
  primary-key collision an error instead of a silent 0-rows-read-as-dedup — probed: bare
  `ON CONFLICT DO NOTHING` swallows an `error_occurrences_pkey` collision as `INSERT 0 0`, which step
  3 would report as a healthy duplicate. Bare `DO NOTHING` is forbidden in this statement.
- **Both arms check rows affected (F-TX-7):** the folded `RETURNING id` yielding `pgx.ErrNoRows` is a
  bug, never a duplicate (it can only happen if the `DO UPDATE` predicate is narrowed — probed) →
  error. The regression `UPDATE ... WHERE id = $2` must check `RowsAffected() == 1` — today it does
  not, and a concurrently-deleted issue proceeds with a stale id into an FK failure. (D-c's row lock
  actually closes that race against retention's DELETE — but only if the 0-row case errors.)
- **Do not use `QueryRow` + `RETURNING` for the duplicate signal (F-TX-8):** `pgx.ErrNoRows` is not a
  `*pgconn.PgError`; `classifyStoreError` would leave it retryable and a duplicate would burn
  MaxDeliver and dead-letter an already-stored message.
- **Deadlock-free by construction, and keep it that way (F-TX, probed with a positive control):**
  `StoreEvent` acquires exactly one contended lock (the issues row) and never waits again. Moving
  `IndexOccurrence` or audit into the tx, or batching multiple events per tx, reintroduces deadlock
  potential against `batchUpdateIssues` and the retention cron. (A pre-existing deadlock between two
  concurrent `batchUpdateIssues` calls was found and reproduced during review — unsorted uncapped
  `inArray` — noted for a separate ticket, NOT this plan's scope.)
- **Performance is a gain, not a risk (F-TX, measured):** hot-fingerprint benchmark, plan-shape vs
  today-shape: 1 client 7.1ms vs 13.3ms; 4 clients 27.1 vs 40.2; 16 clients 112.2 vs 173.7. Throughput
  flat in concurrency for both (the issues row already serializes); the single tx removes a commit
  round trip.

**Events with absent ids** (NULL after the boundary mapping) skip the conflict arm entirely — probed:
repeated NULL inserts all store. Today's semantics exactly.

### D-d · Alert dispatch and indexing move after commit, gated on `stored`

`dispatchAlert` and `IndexOccurrence` run only when `stored == true`, after commit. Fixes the §1
double-feed. Audit stays best-effort (P8's design) but moves after commit and is skipped for
duplicates, so audit stops recording rolled-back writes. This gate is forced by a dedicated
integration test (§4, F-TP-4) — in v1 nothing would have failed if D-d were simply not implemented.

### D-e · Duplicates are loudly visible — and the mechanism is specified, because the obvious implementation double-counts (F-TP-1/F-CT-13)

`ProcessEvent`'s deferred metrics classifier derives the outcome solely from the returned error; a
duplicate returns nil and would record `stored`. Recording `duplicate` separately inside
`processEventInternal` would record BOTH for one message — the metric stops summing to the message
count, and a delta test on `duplicate` alone still passes. Therefore:

- `processEventInternal` changes signature to return `(stored bool, err error)`.
- The deferred classifier in `ProcessEvent` records **exactly one** outcome per message:
  `err == nil && !stored` → `obs.OutcomeDuplicate` (new constant in the fixed set,
  `packages/shared-go/obs/obs.go`); `err == nil && stored` → `OutcomeStored`; error paths unchanged.
- Invariant, mutation-tested at integration level: `stored + duplicate + retried + deadlettered`
  deltas sum to the number of deliveries.
- One structured log line per duplicate with `event_id`, `issue_id`, and the NATS delivery count
  (distinguishes redelivery-after-lost-ACK from client re-send). `event_id` never becomes a metric
  label (D15).

### D-f · The HTTP contract changes only additively

Duplicate POSTs still get 202. The 202 body gains `event_id` (the effective id, echoed — D-a); the
batch response gains it per item. Additive keys only; no status-code changes. SDK hardening:
`evt_<UnixNano>` → UUIDv4 (`crypto/rand`) — UnixNano collides across goroutines and restarts within a
project, which under this design would silently merge real events.

### D-g · The bounds guard exists on BOTH sides of the NATS hop (F-CT-3, new decision)

The proto CEL bound runs only at the ingestor (`s.validator.Validate`); the processor never runs
protovalidate — its `validateEvent` checks four required fields. Any direct publisher (U36-2 itself,
a replayed pre-validation DLQ message, a future producer) can deliver an oversized id straight to the
`VARCHAR(64)` insert → 22001 → class 22 → Permanent → **the event dead-letters and is lost**.

Decision: the processor **preserves the event and drops the id**. In `Deserialize`, an `event_id`
longer than 64 chars is replaced with `""` (→ NULL at the insert) with one WARN log. Losing dedup for
one malformed producer beats losing the event. The ingestor-side CEL rule remains the contract's
front door; the processor-side clamp is the storage-width guarantee.

### D-h · `EventID` is copied before Normalize and never normalized or masked (F-CT-2 — this would have been the whole feature silently off)

`Normalize` runs `normalizer.NormalizeString` over TraceID/SpanID, whose regexes rewrite UUIDs to the
literal `<UUID>` and ≥6-digit runs to `<NUMERIC_ID>`. An implementor following the local pattern would
turn every UUIDv4 id into `<UUID>` — all events in an issue collide on it — and every legacy
`evt_<UnixNano>` id into `evt_<NUMERIC_ID>`. Identical failure to F-TX-1, on the fully-upgraded path.
This is B6 verbatim. `EventID` is copied in `Deserialize`'s struct literal (with `ProjectID`/
`ReleaseVersion`, before `Normalize`), carries a field comment in the S5 style saying it must never
pass through `normalizer` or `masker`, and has a unit test asserting a UUIDv4 survives byte-identical.
(B6 interaction otherwise verified absent: `Normalize` touches only Message/ErrorClass/TraceID/SpanID/
Metadata — F-TP-8d.)

## 3. Contracts

| Boundary | Contract | Guarded by |
|---|---|---|
| SDK JSON → ingestor | optional `event_id` string; ≤64 chars used as-is; absent/oversized → ingestor mints UUIDv4, WARN + `sentinel_ingest_event_id_replaced_total{reason}`; effective id echoed in the 202/batch body | W0 tests exercising **`svc.Ingest`** (NOT `validation.ValidatePayload`, which is unreachable from any route — F-CT-9/B3); contract job (F-CT-14) |
| Proto | `string event_id = 17` (verified free; no reserved ranges; CI's proto job already gates gen/ sync — F-CT-15). Bound via a **message-level CEL entry** `id: "error_event.event_id"`, `expression: this.event_id.size() <= 64` — NOT a field-level `max_len`; the proto file itself forbids mixing the two validator styles (F-CT-4). No `min_len`, no `required`. CEL counts code points, `VARCHAR(64)` counts characters — 64↔64 genuinely matches; recorded so nobody "fixes" it | `buf lint` + `buf breaking` + gen-sync CI step |
| Ingestor → NATS | every message published by a W0+ ingestor carries a non-empty `event_id`. **This is an ingestor-side invariant, NOT a processor-side one** — the processor reads a 72h stream window and a 30-day DLQ stocked with pre-W0 messages (the review's "most dangerous unstated assumption") | W0 publish-path test; the D-b NULLIF mapping is what makes the processor safe anyway |
| Processor boundary | `""` → NULL before storage (D-b); >64 chars → treated as absent (D-g); copied before Normalize (D-h) | integration case (d); unit tests |
| Schema | `event_id VARCHAR(64)` + CHECK nonempty + partial unique `(issue_id, event_id)`; **and the Drizzle mirror** `apps/dashboard-web/src/lib/db/schema.ts` gains `eventId` in the same change (F-CT-12 — the hand-maintained-schema family that already shipped three defects) | migration round-trip gate; schema.ts comment naming the migration |
| Store | `StoreEvent` atomic at READ COMMITTED; `stored=false` ⇔ the exact `(issue_id, event_id)` pair already exists; duplicate leaves every counter untouched | §4 mutation matrix |
| Deploy ordering (F-CT-5) | **migration → (ingestor \| processor, either order); processor-before-migration prohibited** — the new INSERT names a column whose absence is 42703, which `errors.go` leaves retryable → every event burns MaxDeliver and dead-letters (this exact failure shipped once: store.go:204's comment). Compose already enforces it (`depends_on: migrate: service_completed_successfully`). Rollback is one-way safe: old processor ignores field 17 (unknown field); a migrated schema under old code is an unread nullable column; no down-migration needed on rollback | stated in the migration header + W2 brief |

## 4. Work packages

Standing mode: sonnet implements, opus validates by re-running gates and attacking; provisioning stays
with the orchestrator. Sequencing: W0 ∥ W1 → W2 → W3 → W4. One full rebuild before the e2e leg.

### W0 · The key exists end-to-end on the wire

- Proto field 17 + message-level CEL (§3); `buf lint && buf generate`; commit `gen/`.
- SDK: `EventID` → UUIDv4; JSON name unchanged.
- Ingestor: read `event_id` in the **live** path — `handleIngest`/`handleBatchIngest` → `svc.Ingest` →
  `mapping.MapPayloadToEvent` — explicitly NOT `validation.ValidatePayload` (unreachable dead code,
  F-CT-9). Mint/replace per D-a; stamp before publish (batch: per item); echo effective id in
  responses; `sentinel_ingest_event_id_replaced_total{reason}` metric.
- **Contract tests (F-CT-14):** delete `stripEventIDField`/`stripEventIDFieldBatch` from
  `tests/contract/sdk_ingestor_test.go`, stop stripping, add `event_id` to
  `assertPayloadMatchesEvent`/`assertSemanticFieldsSurvived`.
- Unit: `TestIngest_ClientEventIDTooLongIsReplacedNotRejected` (65 chars → 202 + published proto
  carries a fresh UUID), `..._EmptyIsMinted`, `..._ValidIsUsedVerbatim`;
  `tests/unit/ingestor_mapping_test.go`'s `AllFieldsPopulated` gains `event_id` (F-TP-5's B5 hole).
- Gates: root build/vet, `GOWORK=off` vet, unit, sdk-go tests, `buf lint`,
  **`go test -tags=contract ./tests/contract/...`** (missing from v1's list).

### W1 · The schema can express the key

- New migration: column + CHECK + plain (non-CONCURRENT, D-b) partial unique index, all guarded/
  re-runnable (three goose ledgers replay Up against the same database — the 1722100000 precedent);
  `Down` drops index, CHECK, column. Header comment records the deploy-ordering rule and the
  CONCURRENTLY trade.
- Drizzle `schema.ts`: `eventId: varchar('event_id', { length: 64 })` with a comment naming the
  migration (F-CT-12).
- Gates: db-migrations tests; up→down→up round-trip; no pseudo-version bump needed (migration content
  is read from disk at runtime, invisible to compilation — F-CT-14's A2 answer).

### W2 · The write path is atomic and duplicate-aware

- `store.StoreEvent` per D-c **verbatim** — the NULLIF, the explicit `TxOptions`, the mandatory
  conflict target, the RowsAffected checks on both arms, `tx.Exec` not `QueryRow`. Delete
  `UpsertIssue` (zero callers) and, once `processEventInternal` migrates, `UpsertIssueWithOutcome`/
  `InsertOccurrence`/`GetIssueIDByFingerprint` if nothing else calls them.
- `service`: `processEventInternal` → `(stored bool, err error)`; deferred classifier per D-e;
  dispatch/index/audit post-commit gated on `stored` (D-d); duplicate log line with delivery count.
- `event/event.go`: `EventID` copied before Normalize with the D-h comment; D-g length clamp.
- `obs`: `OutcomeDuplicate` constant.
- Degradation buffer header: update the "event_id has no server-side destination today" clause to
  point here.
- **The real ripple (F-TP-5), not v1's imaginary one:** `tests/integration/processor_store_test.go`
  (~13 direct calls to the old methods), `processor_service_test.go` — note
  `TestProcessorService_ProcessEvent_InsertOccurrenceFailsWhenOccurrencesMissing` changes meaning
  under D-c (the upsert no longer survives the occurrence failure; rewrite its post-state assertions
  to assert the ROLLBACK), `procgo_alerting_degradation_test.go` call sites. `tests/unit`:
  `processor_event_test.go`'s `Deserialize_HappyPathMapsAllFields` gains `EventID` (the third
  time this exact function has hidden a dropped field — S5, S6); add an `Outcome` permitted-set
  assertion to `shared_obs_test.go` (today a typo'd outcome literal would go unnoticed).
- Gates: root build/vet, `GOWORK=off` vet, unit, **integration under FORCE_TESTCONTAINERS=1**.

### W3 · Proof

**e2e first needs two harness additions:** `scrapeSeries(url) map[string]map[string]float64`
(family → outcome → value, ignoring `otel_scope_*` labels; extend `tracing_test.go`'s own parser —
`expfmt` panics on `model.NameValidationScheme`, documented there) and `EventID` on `occurrenceRow`
(F-TP-2, F-TP-6).

**Integration (FORCE_TESTCONTAINERS=1 — real SQL, real migrations, no image rebuild):**
- (a) same event twice sequentially → 1 occurrence, `count=1`, second returns `stored=false`.
- (b) N goroutines racing the same event → exactly 1 occurrence, `count=1`. *Proves the issues-row
  lock ordering (F-TX-2); (a) and U36-2 are what prove the index.*
- (c) different `event_id`, same fingerprint → `count=2`.
- (d) **two distinct events with EMPTY `event_id`, same fingerprint → 2 occurrences, `count=2`**
  (the F-TX-1 guard — without this test, a dropped NULLIF silently destroys the legacy population).
- (e) resolve at 1.0.0, deliver a 2.0.0 event **twice with the same id** → `regression_count == 1`,
  exactly one `regressed` activity row (the COMMIT-instead-of-ROLLBACK mutation is visible ONLY here —
  F-TP-8c).
- (f) alert gate (F-TP-4): capture sender (`procgo_alerting_degradation_test.go:389` pattern),
  `frequency_threshold=2`; deliver E, deliver E again (same id) → **0 alerts**; third occurrence,
  fresh id → **exactly 1**. Companion: `error_search_index` count stays 1 after the duplicate.
- (g) outcome-sum invariant (D-e) and the tx isolation level assertion (F-TX-3).

**e2e — U36, with the wait condition F-TP-3 mandates (the DB assertions are meaningless without it):**
```go
before := processOutcomeCounts(t)                    // scrapeSeries on :8081/metrics
// leg 1: POST with "event_id": "e2e-u36-"+uniqueSuffix(), twice
// leg 2: proto.Marshal once (ProjectId SET — production stamps it; omitting it exercises a legacy
//        fallback path production no longer takes — F-TP-11), js.Publish twice,
//        assert ack.Stream == "ERROR_EVENTS" (the test states which stream it believes it hit)
waitFor(t, asyncTimeout, "duplicate recorded", func() (bool, string) {
    now := processOutcomeCounts(t)
    return now["duplicate"] >= before["duplicate"]+1, fmt.Sprintf("duplicate %v→%v", ...)
})
// ONLY NOW: occurrenceCount()==1; onlyIssue().Count==1;
// occurrence.EventID == the exact client literal (F-TP-6 — otherwise a deterministic-minting
//   ingestor that broke D-a's retry column would still pass);
// stored delta == 1 (catches the double-count implementation D-e forbids)
```
Duplicate delta is `>=1` (counters are stack-lifetime cumulative); exactness lives in the
fixture-scoped DB assertions; no `t.Parallel` exists in these suites (verified).
Leg 3: fresh id, same fingerprint → count 2.

**Mutation matrix (F-TP-7 — every guard proven at `go test` speed; ONE e2e rebuild cycle total):**

| Guard | Mutation | Proven at |
|---|---|---|
| conflict arm present | delete it | integration (a) |
| arm targets the partial index | drop the `WHERE` from the arm | integration (a) — errors loudly |
| NULLIF mapping | bind raw `""` | integration (d) |
| rollback not commit | COMMIT on duplicate | integration (e) — only `regression_count` sees it |
| single-tx boundary | split the tx | integration (b) |
| `stored` gates alerts / index | dispatch/index unconditionally | integration (f) |
| one outcome per message | record both / neither | integration (g) sum invariant + U36's `stored` delta |
| id survives the deployed wire | ingestor drops field 17 | e2e U36 leg 1 value assertion |

**U28:** unchanged, and now with the *reason* recorded: `f.newEvent()` sends no `event_id`, so the
ingestor mints per POST — 5 distinct ids, `want = 1 + accepted` semantics identical (F-TP-10). Its
final `t.Errorf` message documents the count-inflation defect as open ("this is S16…") — after W2
that text describes the invariant, not a defect; reword it in W3 (it lives in a test file, not docs).

### W4 · Docs and memory

- **Assign the defect its real number (F-CT-11):** promote S9's residual paragraph to a new `S18`
  entry in `VERIFIED_STATE.md` (verify 18 is still the next free number at execution time), marked
  resolved with commands + output. Fix the "S16" mislabels at `CLAUDE.md:118` and
  `E2E_RECOVERY_PLAN.md` P9-3. **Leave the real S16 entry untouched.**
- `DECISIONS.md` D16; `E2E_RECOVERY_PLAN.md` P9-3 → DONE + U36 matrix row; `CLAUDE.md` known-gaps
  table + test-count baselines.
- Record the review's pre-existing side findings where they belong: the `batchUpdateIssues`
  self-deadlock (unsorted uncapped `inArray` — reproduced with a positive control) and the dashboard's
  dormant `detectAndHandleRegression` unguarded read-then-write — as a known-open note, NOT fixed in
  this change.

## 5. Out of scope, stated so nothing is assumed missing by accident

- Cross-issue dedup of same-id-different-payload (client bug; D-b).
- **Concurrent DISTINCT deliveries on a resolved issue can still double-count `regression_count`**
  (F-TX-4, probed): the regression arm's read-then-write has no `FOR UPDATE`. D-c fixes the same-id
  case (proven — the loser's regression rolls back with its occurrence). The distinct-events fix needs
  `SELECT ... FOR UPDATE`, serializing every delivery on the issue at the read — deliberately deferred.
- A dedup horizon beyond `min(DATA_RETENTION_DAYS, DLQ MaxAge)` (D-b).
- Per-item batch resend in the SDK (does not exist; F-CT-6) and dashboard surfacing of `event_id`.
- Ingestor-side dedup (shared state for a guarantee the index already gives).
- The two pre-existing findings recorded in W4 (batchUpdateIssues deadlock; detectAndHandleRegression).

## 6. Acceptance

Against a freshly rebuilt stack:
1. U36 (all three legs, with the metric-delta wait condition) green under `SENTINEL_E2E=1`; every
   mutation-matrix row observed failing at its stated level; one e2e rebuild cycle.
2. Integration (a)–(g) green under `FORCE_TESTCONTAINERS=1`.
3. U28 green with its comment updated.
4. All existing gates green: root build/vet (+`GOWORK=off`), unit, sdk-go, db-migrations, contract,
   dashboard, full e2e, 9/9 CI.
5. W4 done — including the S18 renumbering; memory is part of done.

## 7. Risks

| Risk | Mitigation |
|---|---|
| The `""`/NULL boundary regresses under future edits | CHECK constraint (23514 → Permanent → loud); integration (d) in CI forever |
| Interface change breaks `tests/integration` at ~13 sites (the REAL ripple; `tests/unit` has no store doubles) | W2 carries the exact file list; `GOWORK=off go vet` still guards test-file compile breakage |
| Processor deployed before migration | 42703 is retryable → budget burn → DLQ (shipped once before). Deploy-ordering contract in §3; compose `depends_on` enforces it; rollback direction is safe |
| A non-ingestor producer sends an oversized id | D-g clamp: event preserved, dedup dropped, WARN — instead of 22001 → dead-letter → loss |
| Isolation level drifts from READ COMMITTED | Explicit `TxOptions` + integration (g) assertion; 40001-under-stricter-levels probed and documented |
| Index build stalls inserts during migration | Accepted at current size; CONCURRENTLY escape hatch documented with all four of its prerequisites (D-b) |
| Duplicate-rollback churn under a retry storm | Duplicates are rare by construction; measured baseline shows the tx shape is faster than today even at 16-way contention, so there is headroom, not debt |
