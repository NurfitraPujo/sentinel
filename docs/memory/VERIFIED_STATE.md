# Verified State of the Codebase

Last verified: **2026-08-01**. HEAD at verification time is `b895df1` on `main` — the merge commit for
PR #11 (`fix/ui-parity-remediation`, 8 commits), the first time this repository has had a **green CI run
on `main`**. Every gate below was re-run against that merged tree, not carried over from the branch:

| Gate | Command | Result at `b895df1` |
|---|---|---|
| Dashboard typecheck | `cd apps/dashboard-web && pnpm check` | 1024 files, **0 errors, 0 warnings** |
| Dashboard build | `cd apps/dashboard-web && pnpm build` | pass (adapter-node) |
| Dashboard tests | `cd apps/dashboard-web && pnpm test` | **32 files, 251 passed** |
| Root module | `go build ./... && go vet ./...` | clean |
| Unit suite | `go test ./tests/unit/...` | **308 passed** |
| E2E (needs the stack up) | `SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/ -count=1` | **76 passed, 0 skipped** |
| CI | GitHub Actions, `push` event on `main` | **9/9 check runs green** (8 jobs; `go-sdk` is a 2-leg matrix) |

The previous verification (2026-07-29, `cd84d17` on `chore/p0-p1-green-tree` with P2/P2b staged and
uncommitted, baseline `ad2f967`) remains the provenance for the S1–S18 entries below; re-run before
trusting any entry older than the current HEAD.

> [!NOTE]
> **CI on `main` was RED before this merge and is green after it.** `gh run list --branch main` records
> `b9e2018` (the pre-remediation baseline) as `failure` and `b895df1` as `success`. Any claim in this repo
> that predates 2026-08-01 and says "CI exists but has never been proven green on a real push" is now
> stale — that had been true since CI was introduced under P0-1, and is not any more.

> [!IMPORTANT]
> This file records what the code **actually does when executed**, as distinct from what `specs/`, `docs/todos/`,
> and `WORKLOG.md` claim was shipped. Those files describe *intent and merge events*; this file describes
> *observed behavior*. When the two disagree, this file wins until re-verified.
>
> Re-run the verification commands below before trusting any entry older than the current HEAD.

> [!NOTE]
> **Full re-verification pass, 2026-07-29**: every entry in [Resolved](#resolved) below was re-run today,
> not copied from the P2 evidence brief that prompted this update — see the command + real output under
> each entry. S7–S10 were independently re-read against current code (not assumed unchanged) and are
> confirmed still open; S9 and S10's open status is independently corroborated by a same-day
> `docs/memory/DECISIONS.md` correction citing its own re-verification. The previous "2026-07-28 partial
> re-verification" note (S1/S2 only) is superseded by this pass.

> [!IMPORTANT]
> **2026-08-01**: five independent reviews of the org-member-management, invitation, issue-relations, and
> observability commits (`dc359cb`, `5639e64`, `f8d66ac`, `b3ccde9`, `49c0307`, `b9e2018`) found the same
> pattern documented above one layer up — merges that pass their own tests and do not run. That defect
> register, its phased remediation, and the acceptance command for each fix live in
> **[docs/plans/UI_PARITY_REMEDIATION_PLAN.md](../plans/UI_PARITY_REMEDIATION_PLAN.md)** (D01–D47). This
> file gains a dated entry, with the command that proved it, only as each item there is actually closed —
> not on the strength of the plan's existence.
>
> **That work is now closed and merged** (PR #11 → `b895df1`). See
> [UI parity remediation](#ui-parity-remediation-d01d47--resolved-2026-08-01) below for what was actually
> proven, what was deliberately left open, and the three defects the remediation *introduced* and then
> caught. Note the ID collision: `D01`–`D47` in that plan are **findings**, unrelated to `D1`–`D18` in
> `DECISIONS.md`, which are **architecture decisions**. The overlap is unfortunate and load-bearing in
> both directions — always name the file when citing a `D` number.

---

## How to verify (copy-paste)

```bash
rtk go build ./...                                    # root module: PASSES
rtk go vet ./...                                      # PASSES
rtk go test ./tests/unit/... -count=1                  # PASSES, 308 passed (2026-08-01)
rtk go test -tags=contract ./tests/contract/... -v -count=1   # PASSES, 4/4 (proves S3/S4/S5/S11/S16 against REAL sdk-go + REAL ingestor decode/validate path)
cd packages/sdk-go && rtk go test ./... -count=1       # PASSES, 4 packages (SDK is an independent module — not covered by the root-module commands above)
cd apps/dashboard-web && rtk pnpm build                # PASSES — run this, NOT just check+test: it is the
                                                       # ONLY gate that enforces SvelteKit's route-export
                                                       # allowlist (B12)
cd apps/dashboard-web && rtk pnpm check                # 1024 files, 0 errors, 0 warnings (2026-08-01)
cd apps/dashboard-web && rtk pnpm test                 # PASSES, 32 files / 251 tests (2026-08-01)
cd apps/dashboard-web && rtk pnpm test --sequence.shuffle   # must ALSO pass — order-independence decays
                                                       # silently (B13); verified across 8 seeds
SENTINEL_E2E=1 rtk go test -tags=e2e ./tests/e2e/ -count=1  # PASSES, 81 passed, 0 skipped (2026-08-17) — NEEDS the compose stack up
```

> [!NOTE]
> **2026-08-17 — agent e2e dead-coverage gap closed.** The 5 M5 agent-work-loop proofs
> (`TestM5AgentWorkLoopIntegration`, `TestU37_*`, `TestU38_*`, `TestU39_*`) gated on
> `M5_AGENT_INTEGRATION_REQUIRED=1`, an env **no workflow ever set**, so they `t.Skip`ped on every CI
> run since merge while the `e2e` job stayed green — this repo's characteristic "recorded green, never
> executed" failure (AGENT_WORKER_PLAN.md §8). Fixed by dropping that separate gate: the tests now gate
> only on `requireStack`, which fatals rather than skips under `SENTINEL_E2E=1`, so the CI `e2e` job
> (which already sets `SENTINEL_E2E=1`) runs them unconditionally. Verified locally against the compose
> stack: all 5 report `--- PASS` (not SKIP). Suite count rose **76 → 81 passed, 0 skipped**.

**CI is now green on `main`** (merge commit `b895df1`, 2026-08-01 — the first green run this repo has
had; the prior baseline `b9e2018` was red). A green badge is evidence about `main`, not your working tree.

There was **no CI** when the original findings below were recorded — `.github/` contained only
`copilot-instructions.md`, with no `.github/workflows/` directory. A CI setup was part of P0; if you are
re-verifying after that landed, prefer running whatever `.github/workflows/` now defines over hand-copying
the commands above, and treat this list as a manual fallback.

### Hazards that will silently invalidate a verification attempt

- **`docker compose up -d` does NOT rebuild.** It reuses whatever image already exists locally. If you have
  edited `apps/*/Dockerfile` sources or Go code and just re-run `up -d`, you are testing a stale binary.
  Use `docker compose up -d --build --force-recreate`.
- **Editing a `.proto` file alone changes nothing at runtime.** `protovalidate` (and the Go struct itself)
  reads the descriptor compiled into `gen/`, not the `.proto` source. Any change to
  `packages/proto/sentinel/v1/error_event.proto` needs `buf generate` before it is live — this is exactly
  how the S3 field-rule bug shipped: the correct CEL expression was already sitting right next to the
  broken field rule in the same file, and regenerating was the step that was skipped.
- **Running the integration suite CORRUPTS the shared dev database.** FIXED 2026-07-29: migration tests
  now clone a throwaway database per test from a template (`tests/integration/db_migrations_test.go`), so
  a `down` can no longer drop tables another goose ledger still believes exist. The account below is kept
  because the *hazard* is structural — every ledger still points at one physical database — and because it
  is what the fix has to keep preventing. Original report: `schema_migrations` and `processor_migrations` both
  report version `1722000000` (`add_api_key_management`, which `CREATE TABLE`s `project_api_keys`) as
  applied:
  ```
  podman exec sentinel-postgres psql -U sentinel -d sentinel -c "SELECT * FROM schema_migrations ORDER BY version_id;"
  →  6 | 1722000000 | t | 2026-07-28 16:23:25...
  ```
  but the table does not exist:
  ```
  podman exec sentinel-postgres psql -U sentinel -d sentinel -c \
    "SELECT table_schema, table_name FROM information_schema.tables WHERE table_name ILIKE '%api_key%';"
  →  (0 rows)
  ```
  Cause (per the hazard originally logged below): `schema_migrations`, `processor_migrations`,
  `seq_migrations`, `baseline_test_migrations`, `dashboard_migrations`, and (now also observed)
  `status_test_migrations` are independent goose ledgers pointed at the **same physical Postgres
  container**; a test's `down` run drops tables that other ledgers still believe are applied. **Practical
  consequence for this file**: any check that requires a real, authenticated API key
  (S6's cross-tenant probe, S7, S10's live rate-limit behavior) could not be re-run against this
  shared instance during this pass — those entries below are verified by code inspection and by the
  automated test suite instead, and say so explicitly. Do not re-run `task test-integration` /
  `go test ./tests/integration/...` against a database you need to keep in a working state; use a
  throwaway one.

---

## Module layout (three separate Go modules, unified by `go.work` for local dev)

| Module | Path | Notes |
|---|---|---|
| root | `github.com/NurfitraPujo/sentinel` | apps + shared-go + tests |
| SDK | `github.com/NurfitraPujo/sentinel/packages/sdk-go` | independent, `go 1.21` |
| migrations | `github.com/NurfitraPujo/sentinel/packages/db-migrations` | independent |

There are **no `replace` directives**; a committed `go.work` (verified present, see below) puts the three
modules into workspace mode for local dev:

```
go 1.25.8

use (
	.
	./packages/db-migrations
	./packages/sdk-go
)
```

This is what makes `tests/contract/` possible at all — it imports the real `packages/sdk-go` from the root
module's test tree, which was structurally impossible before `go.work` existed (through `ad2f967`, the root
module could not import `packages/sdk-go`, and no test could exercise the SDK↔ingestor contract — that is
why S3/S4 shipped undetected for a full release). A CI job running with `GOWORK=off` would still exercise
the root module against `db-migrations`' published pseudo-version rather than local edits; whether such a
job exists was not re-verified in this pass. See A2 in [ARCHITECTURE.md](ARCHITECTURE.md) for the full,
current picture of module boundaries.

---

## Resolved

### N10 part 2 — encrypted git-credentials store (VERIFIED 2026-08-18, branch `feat/n10-repo-credentials`)

Design in D22 (`DECISIONS.md`); plan §4.5 of `docs/plans/AGENT_WORKER_PLAN.md` (rev 4, on
`feat/agent-key-expiry`'s worktree — not yet merged to main). What was verified, with the command:

```
cd apps/dashboard-web && pnpm check                              1871 files, 0 errors, 0 warnings
cd apps/dashboard-web && pnpm build                              pass
cd apps/dashboard-web && pnpm test                               101 files: 97+2 passed | skips, 837 passed
migration 1723900000 replay                                      goose up ×2 per target + raw double-apply
                                                                 with ON_ERROR_STOP: clean (disposable PG16)
DATABASE_URL=<disposable> SCHEMA_DRIFT_REQUIRED=1 vitest tests/schema-drift.test.ts   30 passed
DATABASE_URL=<disposable> N10_CREDENTIALS_INTEGRATION_REQUIRED=1 \
  vitest src/lib/db/queries/repo-credentials.integration.test.ts                      4 passed
```

Guard mutations proven red then reverted: (a) flag check replaced with `if (false)` → the
"plain agent key gets 403" tests fail; (b) audit loop emptied → the audit tests fail; (c)
encryption bypassed with plaintext-as-ciphertext → the no-plaintext-at-rest tests fail.

Caveat found during this work, NOT caused by it: `pnpm test --sequence.shuffle` fails on a clean
tree (order-dependent `issues.test.ts` assertions + 5s timeouts in two integration flows). Plain
`pnpm test` is green. B13's decay warning is now a live fact, filed as a follow-up task.

### UI parity remediation (D01–D47) — RESOLVED 2026-08-01

**What was wrong.** Five features closed the dashboard/backend parity gaps in `dc359cb`, `5639e64`,
`f8d66ac`, `b3ccde9`, `49c0307`, `b9e2018`. Each merged with its own tests passing. **Three of the five
did not execute at runtime**, and the tree was already red at `b9e2018` (`pnpm check` 2 errors,
`pnpm test` 2 failures) before any remediation started. This is B3 reproducing one layer up, in the
dashboard rather than the pipeline.

The three dead features, each proven dead rather than assumed:

| Finding | Symptom | Root cause |
|---|---|---|
| D01 | Emailed invite link 403'd for **signed-in** invitees only | `invitations` missing from `hooks.server.ts`'s `reservedRoutes`, so the path was parsed as an org slug. Anonymous users escaped via an early return — it failed for the *common* case. |
| D02 | `/api/issues/search` 500'd on **every** query | `issues.id ILIKE` against a `uuid` column. Reproduced against real Postgres: `operator does not exist: uuid ~~* unknown`. Search is the only way to pick a link target, so the whole relations flow was unusable. |
| D04 | Only one alert rule per org/project ever fired | `Dispatcher.configs`/`orgConfigs` were `map[string]*AlertConfig` — one entry per key — with no unique index behind them. Extra rules were overwritten silently at load. |

**Security defects closed** (all live on any deployed instance at the time): `/settings/observability`
served DLQ depth, publish-failure counts and stream names to **anonymous** visitors (D05 — `settings` is
in `reservedRoutes`, so `orgHandle` never guarded it); invitation tokens stored **plaintext** and moved
into a `?token=` query string (D06); redemption was check-then-act across five statements with no
transaction and no revocation path (D07); accepting a `viewer` invite **demoted an existing owner**
(D08); `keyHash` — the SHA-256 the ingestor's Redis cache is keyed on — was returned to the browser on
key create and rotate (D09); org `viewer` could mutate issues one at a time though bulk denied them
(D10).

**Verified by** (re-run at `b895df1`, see the gate table at the top of this file): `pnpm check`
1024 files / 0 errors / 0 warnings, `pnpm build`, `pnpm test` 251 passed, `go build`/`go vet`,
`go test ./tests/unit/...` 308 passed, `SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/` 76 passed, and
9/9 CI check runs green (8 jobs; `go-sdk` is a 2-leg matrix) on the `push` event for `main`.

**Every fix was verified to FAIL with the fix reverted.** That was applied literally — reverting the
`.for('update')` calls, the `::text` cast, the `requireIssueAccess` calls, the `redirectTo` option, and
the inviter-authority check, then observing the specific tests fail and pass again on restore.

#### Three defects the remediation itself introduced — and what caught each

Recorded because *how* they were caught matters more than that they existed:

1. **A 500 on `/[orgSlug]/settings/observability`.** The org route forwarded only `{ fetch }` to a
   shared loader behind an `as Parameters<typeof baseLoad>[0]` cast; when that loader gained
   `locals.auth()` for D05, `locals` was `undefined` and every request threw. **The cast hid it from
   `svelte-check`.** Two agents each made a locally-reasonable change; the composition was broken. Fixed
   by replacing cast-and-delegate with a shared `loadObservability(event)` both routes call.
2. **`pnpm build` was broken and nothing noticed.** SvelteKit route modules may only export a fixed
   allowlist of names; two files exported constants (`INVITE_TOKEN_COOKIE`, and `loadObservability` from
   the remediation's own P0 fix). `pnpm check` and `pnpm test` passed the entire time — **only `pnpm build`
   fails on this**, and it had not been run. Both now live in `$lib/server/`. See B12.
3. **The access model was too strict.** `requireIssueAccess` demanded an org role **and** a
   `project_members` row; e2e U13 disproved it — an org admin could not link two issues, because
   `project_members` holds only per-project grants, not org-level staff. Corrected per D17 below. A unit
   test asserting the stricter model had to be rewritten: **e2e was right and the unit test was wrong.**

#### Two CI failures after the first push, both "passes locally, fails in CI"

The branch was green locally and still failed CI twice. Both causes were environmental state the local
machine had and CI did not — worth internalising before trusting any local-only green:

- **`integration`**: migration `1722300000` used unguarded DDL, so replaying it failed with
  `column "token_hash" ... already exists`. One flat migration directory serves several goose ledgers
  against the **same** physical database (A1), so every migration is replayed per target. Every other
  migration in the directory already followed an `IF NOT EXISTS` convention; the two new ones did not.
  Both are now guarded, including the `ADD CONSTRAINT` statements, which have no `IF NOT EXISTS` in
  Postgres and use a `pg_constraint` catalog check.
- **`dashboard`**: the D02 uuid-cast test queried for *any issue row that happens to exist* and asserted
  one was found. That passed against a dev database full of e2e leftovers and failed in CI's
  freshly-migrated, empty Postgres. It now seeds and deletes its own org/project/issue. See B13.

#### Deliberately open, with reasons

- **A product decision, not a defect**: the raw invitation token is no longer returned to any client and
  `InviteMemberModal`'s copy-paste link was removed (D06). **Invitations therefore depend entirely on
  working email delivery.** The create response reports `delivered` so the caller can tell "created and
  emailed" from "created only", but in an environment with no `EMAIL_SERVER` an admin can create an
  invitation with no way to convey it. Defensible; should be affirmed deliberately rather than inherited.
- **9 `TestProcessorService_*` failures in `tests/integration` on the authoring machine**, all
  `postgres ping unavailable ... connection refused` from a testcontainers/podman lifecycle problem.
  These **pass in CI**, so they are environmental. Do not "fix" them without first reproducing in CI.
- `S10` (rate limiting non-atomic / fails open) is untouched by this work and remains open below.

### S18 — `issues.count` inflated on partial-failure redelivery; the write path had no idempotency key (RESOLVED 2026-07-31)

*Long mislabeled "S16" in CLAUDE.md and E2E_RECOVERY_PLAN.md — S16 is the ProjectKey secret/name
split, resolved 2026-07-28/29. This defect never had its own number; it lived as S9's "residual,
knowingly accepted" paragraph above. Renumbered during IDEMPOTENCY_PLAN.md's adversarial review
(F-CT-11), which caught that updating "S16 references" would have corrupted the real S16's record.*

**Original defect.** The processor's write path was two disconnected transactions: the issue upsert
(`count = issues.count + 1`, committed) and the occurrence insert (bare pool exec). A failure between
them NAKed the message; NATS redelivered the same bytes; the upsert committed another increment.
`regression_count`, `last_regressed_at`, and `issue_activity` sat in the same blast radius, and alert
dispatch ran *between* the two writes — so a redelivery double-fed the alert frequency counter, and an
event that then failed storage still counted toward an alert. `error_occurrences` had no unique
constraint of any kind, and the idempotency key the SDK already sent (`event_id`) was silently dropped
by the ingestor — the proto had no field for it (documented at the time in
`degradation/buffer.go`'s header as why the buffer couldn't dedup).

**The fix** (`docs/plans/IDEMPOTENCY_PLAN.md`, plan reviewed adversarially by three lenses BEFORE
implementation; every work package implemented by one agent and validated by another that re-ran
everything):

- `event_id` travels SDK → ingestor (proto field 17) → NATS → processor. The ingestor uses a valid
  client id (≤64 runes, no control chars) or mints a UUIDv4 — **before publish**, so every delivery
  of one event carries the same key — and echoes the effective id in the 202/batch response.
- `error_occurrences.event_id VARCHAR(64)` + partial unique `(issue_id, event_id) WHERE event_id IS
  NOT NULL` + a CHECK rejecting `''` (migration 1722200000).
- `store.StoreEvent`: ONE transaction (explicit READ COMMITTED) — issue upsert + occurrence insert
  with `NULLIF($11,'')` and a targeted `ON CONFLICT ... DO NOTHING`; 0 rows affected → ROLLBACK →
  the delivery ACKs as a no-op with `outcome="duplicate"`. Audit/alert/index run post-commit, gated
  on `stored`.
- Proto3-has-no-NULL was the load-bearing subtlety: an absent `event_id` deserializes to `""`, which
  IS NOT NULL — without the `NULLIF` mapping, every pre-W0 in-flight message after the first per
  issue would have been silently discarded as a "duplicate". Found by two independent reviewers
  probing a real Postgres before any code existed (F-TX-1/F-CT-1); guarded forever by the CHECK
  constraint (23514 = loud) and integration test (d).

**Verified** (each test observed failing under targeted mutation before being trusted — 8-row
mutation matrix: 7 rows at `go test` speed, and an 8th — "the id survives the deployed wire" — proven
by mutating the ingestor's mapper, rebuilding AND force-recreating its container, and watching U36
time out naming the duplicate metric; the first attempt at that row also demonstrated that
`compose up --build <svc>` without `--force-recreate` rebuilds the image while the old container keeps
running, which would have made the row silently vacuous):

```
tests/integration/event_idempotency_test.go — 7 tests, FORCE_TESTCONTAINERS=1, real migrations:
  (a) same id twice sequentially     → 1 occurrence, count=1, second returns stored=false
  (b) 8 goroutines racing one event  → exactly 1 stored, ZERO errors (no 40001, no deadlock)
  (c) fresh id, same fingerprint     → count=2
  (d) two EMPTY-id events            → BOTH store, rows are NULL not ''   (the legacy population)
  (e) regression under duplicate     → regression_count=1, one activity row — interleaving forced
                                       deterministically via a held-open connection
  (f) duplicate feeds NO alert/index → capture-sender: 0 alerts on dup, 1 at threshold after
  (g) stored+duplicate == deliveries → OTel ManualReader; isolation asserted read committed

tests/e2e/idempotency_test.go — U36, against the deployed stack:
  leg 1: same POST twice → both 202 echo the client literal; wait on duplicate-metric delta;
         1 occurrence whose stored event_id == the literal; stored delta == 1
  leg 2: same proto bytes js.Publish'd twice (ack.Stream == "ERROR_EVENTS") → 1 occurrence
  leg 3: fresh id, same fingerprint → count 2
  + batch echo: 3 items, 1 invalid → ingested=2, event_ids at indices {0,2} only

SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/ -count=1   → ok, 0 failures, 0 skips (~132s)
```

**Also fixed in passing**: the alert frequency counter no longer counts unstored or redelivered
events; U28's failure message no longer describes this defect as open; and two more unqualified
container-image names (`natsio/nats-box` plus all five Dockerfile bases) were qualified after the
nats-box cache eviction reproduced the B9 jaeger failure — a stack that LOOKS healthy while running
four-hour-old images.

**Knowingly still open** (recorded in IDEMPOTENCY_PLAN.md §5, not regressions): concurrent DISTINCT
deliveries on a resolved issue can still double-count `regression_count` (the regression arm's
read-then-write has no FOR UPDATE; the same-id case IS fixed); dedup horizon is
`min(DATA_RETENTION_DAYS, DLQ MaxAge=30d)`; same-id-different-payload lands per-issue and does not
dedup across issues.

**Side findings from this work's review, REPRODUCED but deliberately not fixed here** (recorded so a
true finding does not live only inside a work-package brief):

- ~~**`batchUpdateIssues` can deadlock against itself.**~~ **SUPERSEDED — this attribution was wrong;
  see the dedicated entry further down (`batchUpdateIssues — the "deadlock" does NOT reproduce`).**
  The deadlock trace quoted here was real, but it was produced against per-row UPDATEs in a loop, not
  against the single `inArray` statement this function issues. Re-probed against a real Postgres:
  that UPDATE plans as a Bitmap Heap Scan, so rows lock in PHYSICAL order and the id list's order
  cannot affect lock acquisition — reversed lists, identical lists and an FK/UPDATE crossing all
  failed to deadlock. The proposed "sort the ids" fix was implemented, found inert, and removed
  rather than shipped. What the follow-up DID find in this function was a cross-tenant activity
  write, now fixed. `StoreEvent` remains proven deadlock-free (it holds exactly one contended lock).
- **`detectAndHandleRegression` (dashboard) has the same unguarded read-then-write** the processor's
  regression arm has (`issues.ts` ~246): SELECT status, then UPDATE, no FOR UPDATE. It currently has
  NO callers — if it ever gains one, it inherits the concurrent-distinct-deliveries double-count
  documented above, on the dashboard side.

### Observability: structured logs, metrics, and a distributed trace that actually joins (2026-07-31)

Closes P9-2. `docs/plans/OBSERVABILITY_PLAN.md` holds the decisions; `E2E_RECOVERY_PLAN.md` P9-5 holds
what was deliberately left open.

**Verified** (all run against the tree at the time of writing, not inferred from the diff):
```
go build ./... && go vet ./...                                   clean
GOWORK=off go vet ./...                                          clean
go test ./tests/unit/... -count=1                                292 passed (was 282)
SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/ -count=1           74 passed, 0 failed, 0 skipped
```

**What is actually proven, and by what.** The load-bearing claim is *not* "spans exist" — it is that the
two Go services' spans land in ONE trace:

```
$ curl -s localhost:16686/api/traces/06c073ea88a4cc3e2f87a8e3ed3cb1e9
spans: 8   services: ['ingestor-go', 'processor-go']
$ curl -s localhost:16686/api/traces/ffffffffffffffffffffffffffffffff
{"data":null,"total":0,"errors":[{"code":404,"msg":"trace not found"}]}
```

The second command is the point: the query is by the *caller-supplied* trace id, so it can only succeed
if propagation genuinely worked. U35 (`tests/e2e/tracing_test.go`) asserts exactly this against the
deployed containers.

**Why this needed an e2e row at all.** OpenTelemetry's failure mode is silence. If nobody calls
`otel.SetTextMapPropagator`, the global propagator is a **no-op**: `Inject` writes nothing, `Extract`
returns the context unchanged, no error, no log line. Both services still start, still create spans,
still serve `/metrics` — and emit two disconnected traces. Every unit test stays green. This was
confirmed empirically, not reasoned about: with the registration mutated out, a probe reported
`propagator Fields=[]`, and with it restored, `parent-is-producer=true remote=true`.

Two guards were added and each was **observed failing** before being accepted:
- `TestObsBootstrapRegistersTheGlobalPropagator` — deleting the registration yields
  `[]string{} does not contain "traceparent"`.
- `TestObsBootstrapDropsUnboundedMetricAttributes` — deleting the metrics View lets an
  attacker-controlled `server.address` reach `/metrics`.

**Defects this work found and fixed** (each reproduced at runtime by adversarial review, not read off
the diff):

| | Defect | Why it mattered |
|---|---|---|
| 1 | `otelhttp` recorded `server.address` — the client-supplied `Host` header — as a **metric** label on the ingestor, which is the one publicly exposed port, and it records *outside* the authenticator | Any unauthenticated caller could mint a new time series per distinct header value across several histograms, growing memory inside the ingest process. Fixed with an SDK View in `obs.Bootstrap` that denies request-derived keys for **all** instruments, so future instrumentation inherits it. |
| 2 | `target_info` on the unauthenticated `/metrics` published hostname, OS user, pid, **full argv** and Go toolchain version | A free fingerprint for CVE targeting, on the public port. Fixed by dropping the `WithProcess()`/`WithHost()` resource detectors — in a container the "host" is disposable and the pid is always 1, so nothing of value was lost. |
| 3 | The DLQ **destroyed the trace in both directions** — `deadLetter` built a fresh header map, and `tools/dlq` replayed with no headers at all | The single most valuable trace in the system ("this event failed N times and got parked") was severed exactly at the failure. Fixed; replay now carries `traceparent`/`tracestate`/`baggage` forward and deliberately drops the DLQ bookkeeping headers. |
| 4 | `sentinel_alert_dispatch_total` was recorded **only after a successful enqueue**, so all seven drop paths — including the literal pre-S8 "no sender wired" no-op — were invisible | It re-created the S8 blind spot in metric form: the counter read flat 0 both when healthy-and-quiet and when totally broken. Fixed with an explicit `dropped` outcome at every drop site. |
| 5 | The `jaeger` compose service used an unqualified image name | Podman resolves no unqualified short names, so `up` continued **without a trace backend** while every service started fine. Nothing looked broken until a trace assertion had nowhere to read from — a textbook B9. Fixed by fully qualifying to `docker.io/...`. |

**Dashboard, treated separately (D-f, best effort).** Review found the SvelteKit side exporting
`url.query` verbatim as a span attribute — which on the Auth.js magic-link callback carries the login
token *and* the user's email — and minting a bespoke random correlation id rather than using the OTel
trace id, in violation of D-d. The correlation bug **hides itself**: when a caller supplies a
`traceparent` both sides independently honour the W3C header and coincidentally agree, so it diverges
only for real browser traffic, and the obvious test therefore passes.

Both fixed, and the leak was verified against the **deployed** dashboard rather than in a unit test —
a request with a real secret in the query string, then the span read back out of Jaeger:

```
$ curl -H "traceparent: 00-534eef7c…-01" \
    "localhost:3000/auth/callback/email?token=MAGICLINKSECRET123&email=victim%40example.com"
$ curl -s localhost:16686/api/traces/534eef7c…
  url.path  = /auth/callback/email
  url.query = token=REDACTED&email=REDACTED
$ ... | grep -c "MAGICLINKSECRET123\|victim@example.com"
0
```

A sixth leak of the same family was found while fixing it: NodeSDK's **default** `resourceDetectors` is
`[envDetector, processDetector, hostDetector]`, so hostname, pid and argv were riding on every span's
resource — exactly what the Go side had just stopped doing. Now pinned to `[envDetector]`.

Shutdown was measured rather than assumed: SIGTERM→exit was **10.03s** against a black-holed collector,
which is Docker's default grace period to the millisecond. The real gate was the OTLP exporter's own
10s per-export timeout, not the await; setting `timeoutMillis: 5000` (matching the Go side's
`defaultOTLPTimeout`) brings it to **5.02s**.

### Ops hardening: bounded streams, DLQ alerting and draining, two-layer alert configs (2026-07-30)

**Verified**:
```
GOWORK=off go vet ./...                                          clean
go test ./tests/unit/... -count=1                                282 passed (was 251)
go test ./tests/integration/... -count=1                         ok 49.854s
SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/ -count=1           61 passed, 0 failed, 0 skipped, 125.052s
cd apps/dashboard-web && pnpm check && pnpm test                 691 files/0 errors; 79 tests (was 63)
```
CI green on all 9 jobs; `e2e ok 111.145s` and `integration ok 40.925s` read from the job logs, not inferred
from the green tick.

**The outage that prompted it.** The DLQ silently reached 6,148 messages, exhausted JetStream storage, and
stream creation began failing with `nats: insufficient storage resources available` — which surfaced as
eight *unrelated* integration tests failing, and cost real time to trace because the errors looked like
sentinel bugs (they came from another project's NATS on the default port).

**What was actually wrong was worse than the DLQ.** `ERROR_EVENTS` had `retention=Limits` with no limits and
`discard=DiscardNew`: 18,654 fully-acked messages retained forever, and a full store REJECTS NEW PUBLISHES —
ingestion stops at the front door. Both streams are now bounded (D13), and U33 asserts it. That guard is
real, not fitted: the same probe measured `maxAge=0 maxBytes=-1` before the fix, exactly what it rejects.

**Now true, each with the thing that proves it:**

| Claim | Proof |
|---|---|
| Both streams bounded, discard policy per role | U33; read back from the live server after a full rebuild |
| `nats-init.sh` is idempotent | ran twice against a live server — second run reported "No difference in configuration" for all four streams |
| DLQ depth/age/class reported actionably | U34; `/health` carries `dlq_threshold`, `dlq_stale_after_seconds`, `dlq_oldest_age_seconds`, `dlq_oldest_class` |
| Reported depth is live, not a constant | U34 parks a real malformed event and watches the number move |
| Dead letters carry a machine-readable class | `X-Sentinel-Dlq-Class`, derived from the existing `PermanentError` check (D14) |
| Permanent failures are never auto-replayed | `tools/dlq -drain` forces `class=transient`; verified live that unclassified messages are refused without an explicit override |
| Replay caps survive a re-park | content-hash state file, because `deadLetter` rebuilds headers from scratch — proven by observing a replayed message return with none |
| Alert configs are two-layer | D12; migration `1722100000`; cross-tenant insert rejected by the composite FK on a throwaway database |
| Org-wide alerts need `manage_keys` | dashboard unit tests; `PUT`/`DELETE` authorize from the stored row, not the request body |

**Deferred, deliberately** — see **P9** in `docs/plans/E2E_RECOVERY_PLAN.md` for the reason and acceptance
bar of each: org-wide alert UI (P9-1), observability (P9-2, recommended first), S16 `event_id` idempotency
(P9-3), invitation acceptance route (P9-4).

**Two test seeds were broken by the migration and fixed here**, both found by an agent reporting across its
own scope boundary rather than left to fail in CI: three raw `alert_configs` INSERTs omitted the new NOT NULL
`organization_id`, and `tests/integration`'s `seedProject` created an **orphan project with no organization
at all** — legal, since `projects.organization_id` is nullable, but not a state anything else in the system
supports.

**One of my own tests was wrong again** (B10): U34 accepted only `"permanent"` or `"transient"`, written
before `"unclassified"` was added to the same contract an hour later. It now accepts the whole vocabulary and
nothing outside it, which is what the assertion is for.


### P7 — the E2E proof harness now exists, and closed six defects (2026-07-30)

`tests/e2e/` did not exist until 2026-07-30. All 32 rows of the use-case matrix now have named tests that
run against the full compose stack.

**Verified**:
```
docker compose up -d --build && ./scripts/wait-healthy.sh
SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/ -count=1
→ 56 passed, 0 failed, 0 skipped, 124.816s
```

Runs in CI on every push (the `e2e` job). `-tags=e2e` is mandatory: `sdk_test.go` carries `//go:build e2e`
because it imports `packages/sdk-go`, a separate module reachable only in workspace mode, and an excluded
file leaves no trace in `go test` output — so `sdk_tag_guard_test.go` fails under `SENTINEL_E2E=1` rather
than let U8-U10 vanish silently.

Six defects were found or measured by these rows and then fixed. **Two were previously unknown**, and both
of those are the same shape as B3/B8: complete, correct-looking code that never ran in the deployment.

| # | Defect | Found by | Fix |
|---|---|---|---|
| 1 | **Nobody could sign in to the dashboard.** `GET /auth/signin` 302'd to itself indefinitely for every visitor. `pages.signIn` was set to the exact path Auth.js reserves for its own signin action, so `@auth/core` redirected back to it forever. Every build, typecheck and unit-test gate was green. **Previously unknown.** | U24 | custom page moved to `/signin`; `hooks.server.ts` reserved-route list updated so it is not mistaken for an org slug |
| 2 | **Retention deleted brand-new issues.** The orphaned-issue delete had no age check at all — an issue whose first occurrence had just been removed was deleted immediately. **Previously unknown.** | U32 | gated on `first_seen` (never updated after INSERT, unlike `last_seen`) |
| 3 | **API-key revocation took up to 60s instead of <100ms.** The dashboard service had no `NATS_URL`, so `createNatsPublisher` fell back to `nats://localhost:4222` — nothing, inside a container. Every `api_key.invalidated` publish failed with ECONNREFUSED and revocation silently degraded to cache-TTL latency. Both sides of the feature were correct; only the deployment never connected them, and the failure path logs instead of failing. | U15, U16 | `NATS_URL` + `depends_on: nats` on the dashboard service. **37.45s → 553µs**, and U16 39.3s → 1.89ms |
| 4 | **S10 — rate limiting overshot and failed open.** 200 concurrent requests against a limit of 100 were accepted **111** times (also 101, 109): four unpipelined Redis round-trips let every request read the same count before any wrote. Separately, a nil Redis client accepted 20/20 against a limit of 1, because the nil-client branch returned before `RATELIMIT_STRICT_MODE` was consulted. | U19, U20 | one atomic Lua script; fail-open is now an explicit logged opt-out (`RATELIMIT_ALLOW_NO_REDIS`) and the default refuses to start; `PORT` override added |
| 5 | **Alert configs took up to 5 minutes to take effect.** `loadConfigs` ran at construction and then on a hardcoded ticker with no invalidation path. | U27 | subscribe to `alert_config.changed`; backstop ticker cut to 30s and made env-tunable. Also halved the suite: **246s → 124.8s** |
| 6 | **A relation created through the API could never be removed.** `relations/+server.ts` exported only `POST`; no delete query existed in `src/lib` either. | U13 | `DELETE` handler + `deleteIssueRelation` |

Plus one cross-boundary defect found by an agent reporting across its own scope boundary, not by a row:
**an alert config created in the UI could never deliver.** The dashboard wrote `channel_config` as
`{target: …}` while the processor reads `["to"]` for email and `["chat_id"]` for telegram — the row looked
perfectly well-formed in the database and no sender ever found a destination. This is B5 exactly: a
cross-boundary payload with no compiler and no shared type. Fixed by writing the per-channel key, with a
read-side fallback so pre-fix rows still render.

**Two defects were in the tests, not the product, and both are worth remembering** because each would have
outlived the bug it was written for:

- An **inverted assertion** in U20 read `if accepted != requests`, so it *passed because* all 20 requests
  were being silently accepted. It reported success while S10 was wide open, never printed its own
  diagnosis, and would have turned red the moment somebody fixed the limiter. U24 and U25 had the same
  disease in a different form — they `t.Errorf`'d when the defect reproduced and `t.Fatalf`/`t.Skip`'d when
  it did not, so they could never pass. All were rewritten to assert the REQUIRED behaviour.
- A **deadlock in `t.Cleanup`** receiving a second time from a size-1 channel written once. A hang in
  Cleanup stalls the whole binary, so ten rows (U28-U30, U8-U10, U18, U21) never ran — and `go test`
  prints nothing at all in that state. It stayed hidden because a verification run was piped through
  `head`, which closed the pipe and killed `go test` before the hang surfaced. **Never observe a run
  through a pipeline that can kill it.**

### Dropped: the "stale issue" concept (2026-07-30)

`retention.ts` set `status: 'stale'`, which `issues.check_status` does not permit, while filtering on
`status = 'open'`, which is also not permitted — so the WHERE never matched and the illegal SET was never
reached. Two mutually-masking bugs: the unreachable filter is exactly what stopped the invalid write from
throwing, and the endpoint reported `markedStaleIssues: 0` forever while appearing to work.

Both values came from `scripts/db/init.sql`, a **third and stale schema** that still declares
`CHECK (status IN ('open','resolved','ignored'))`. The concept was dropped rather than repaired: nothing
consumed the count and no spec asks for a 'stale' state, so keeping it would have meant a migration
widening a CHECK constraint for a feature that has never once run.

This is the third defect of this family. The other two: `issue_activity.event_type` written as
`'status_change'` when the constraint requires `'status_changed'`, and the P7 harness's own readers
selecting `issues.title` / `first_release` / `last_release` / `error_occurrences.message` / `timestamp` /
`issue_activity.action` — none of which exist. **As long as `scripts/db/init.sql` exists as a third
schema, this family keeps reproducing.** Deleting it is the actual fix and has not been done.


### S1 — The entire `tests/unit` package did not compile (RESOLVED 2026-07-28)

**Verified**: `go test ./tests/unit/... -count=1` → `ok`, 241 assertions run. New baseline: **241** (253 before the degradation-buffer tests were deleted with the buffer itself; see D1), up from
the 0 that ran while the package failed to build.

**Re-verified 2026-07-29**: `go test ./tests/unit/... -v -count=1 2>&1 | grep -c "PASS:"` → **253**
(up from 251 — `tests/unit/ingestor_middleware_test.go` and `tests/unit/masker_test.go` gained assertions
as part of the R1 and PII fixes below). `go vet ./...` → no issues.

**Original defect** (kept for history — this is what future changes to `middleware.NewRateLimiter` or the
test file must not reintroduce):

`go vet ./...` and `go test ./tests/unit/...` both failed at build:

```
tests/unit/ingestor_middleware_test.go:17:48: too many arguments in call to middleware.NewRateLimiter
        have (nil, number, time.Duration)   want (*redis.Client)
tests/unit/ingestor_middleware_test.go:21:30: ratelimiter.Allow undefined
```

`middleware.NewRateLimiter` was changed to a Redis-backed sliding window (feature 008) and the old
token-bucket `Allow()` API was deleted, but `tests/unit/ingestor_middleware_test.go` was never updated.

**Blast radius that made this dangerous**: Go is per-package, so *all 11 files* in `tests/unit/` were dead —
roughly 2,800 lines of assertions covering fingerprinting, masking, normalization, validation, degradation,
indexer fields and notifiers had not run since the 008 merge. `task test-unit` reported failure but the
failure looked like one broken file, not a disabled suite (B4).

**Fix**: `tests/unit/ingestor_middleware_test.go` rewritten against the current Redis-backed
`NewRateLimiter(*redis.Client)`, using `miniredis` for a real in-process server.

**Where to look**: `tests/unit/ingestor_middleware_test.go`, `apps/ingestor-go/middleware/ratelimit.go`.

---

### S2 — `apps/dashboard-web` did not build (RESOLVED 2026-07-28)

**Verified**: `pnpm check` → `707 FILES 0 ERRORS`.

**Re-verified 2026-07-29**: unchanged this pass — no `apps/dashboard-web/` files are in the P2/P2b staged
diff (`git diff --cached HEAD --stat` touches only `apps/{ingestor,processor}-go`, `packages/`, `gen/`,
`docs/memory/`, and `tests/`). Not re-run live in this pass; treat the 2026-07-28 result as current pending
a fresh `pnpm build && pnpm check && pnpm test`.

**Original defect** (kept for history): root cause was a single line with literal backslash-escaped
backticks written into the source (a heredoc/escaping accident):

```ts
// apps/dashboard-web/src/lib/db/queries/issues.ts:202
regressionCount: sql\`\${issues.regressionCount} + 1\`,
```

It was the only such line in the tree (`grep -rn '\\`' src/`). One-line fix, total build outage.

The stale `apps/dashboard-web/build/` and `.svelte-kit/` directories on disk were from an older successful
build and **masked this failure** — the app appeared runnable while the source was unbuildable. Both are
gitignored, so a clean clone got no dashboard at all.

`svelte-check` additionally reported **39 type errors**, notably:

- `Cannot find module '$lib/db'` in **6 route files** — `src/lib/db/index.ts` does not exist; the working
  alias is `$lib/server/db`. Affected: `api/organizations/switch`, `api/organizations/[orgId]/invitations`,
  `api/issues/[issueId]/relations`, `api/projects/[projectId]/issues`, `.../issues/batch`,
  `lib/db/queries/organizations.ts`.
- `Cannot find module 'vitest'` — vitest was **not** in `package.json` devDependencies and there was **no
  `test` script**. Dashboard tests could not be run at all. (Corrected detail: there were **five**
  `.test.ts` files at the time this was fixed —
  `src/lib/db/queries/apikeys.test.ts`, `src/lib/components/keys/ApiKeyTable.test.ts`,
  `src/routes/api/organizations/keys.test.ts`, `tests/multi-tenant-isolation.test.ts`, and
  `tests/issue-lifecycle-regression.test.ts` — not three as originally recorded here.)
- `api/projects/[projectId]/issues/+server.ts:43` filtered on `issues.releaseVersion`, which does not exist —
  `release_version` lives on `error_occurrences`, not `issues`.
- The same file called `locals.getSession()`; the rest of the app (and `hooks.server.ts`) uses the
  `@auth/sveltekit` v1 API `locals.auth()`. `getSession` was never populated anywhere.

**Fix**: escaped backticks corrected; `$lib/db` imports repointed to `$lib/server/db`; `vitest` +
`@testing-library/svelte` added to devDependencies with a `"test": "vitest run"` script; stale `build/` and
`.svelte-kit/` directories removed.

**Verified**: `pnpm test` → 5 files, 19 tests passed.

**Where to look**: `apps/dashboard-web/src/lib/db/queries/issues.ts`, `apps/dashboard-web/package.json`.

---

### S3 — The `/ingest` endpoint rejects 100% of well-formed events (RESOLVED 2026-07-28/29)

**Resolved by DELETING the redundant field rule, not by switching it to `max_len`.** The fix is
`packages/proto/sentinel/v1/error_event.proto`:

```diff
- string message = 4 [(buf.validate.field).string.len = 10000];
+ string message = 4;
```

with the CEL block above it (already correct before the fix) left as the sole validator:
```
expression: "this.message.size() <= 10000"
```

The proto now carries an explicit comment on why: *"Do not add a `string.len` / `max_len` field rule here
as well: two overlapping validators with different semantics is exactly how that bug hid."* This matters
because `max_len` would have "fixed" the symptom while leaving the original defect class — **a second,
independent validator silently overriding the CEL rule's intent** — in place for the next person to
re-trigger with a different field. Deleting the field rule instead makes the CEL expression the single
source of truth, structurally, not just correctly-configured today.

**Re-verified 2026-07-29**: `go test -tags=contract ./tests/contract/... -v -count=1` → **4/4 PASS**,
including `TestSDKToIngestorContract_SingleEvent` and `TestSDKToIngestorContract_Batch`, which build a real
SDK event, marshal it exactly as `transport.go` does, decode it with the real `validation.ErrorPayload`,
map it with the real `mapping.MapPayloadToEvent`, and run the real `protovalidate` validator — the event's
`message` field is well under 10000 bytes and is accepted (the original bug rejected every such event).
`go build ./...` and `go vet ./...` both pass with the proto's generated code (`gen/sentinel/v1/error_event.pb.go`)
regenerated to match — a reminder that this fix required `buf generate`, not just an edit to the `.proto`
source (see the hazard note under "How to verify" above).

**Original defect** (kept for history):

**Verified by probe** (`protovalidate.Validate` against a mapped payload):

```
message: must be 10000 characters
```

`packages/proto/error_event.proto:40` declares:

```proto
string message = 4 [(buf.validate.field).string.len = 10000];
```

`string.len` in protovalidate means **exactly** 10000 bytes, not a maximum (`string.max_len` is the intended
rule). `IngestService.Ingest` runs `protovalidate` on every event, so every event whose message is not
precisely 10000 characters — i.e. every real event, including the empty one — is rejected with HTTP 400.

The CEL block directly above it already expresses the intended rule correctly
(`this.message.size() <= 10000`), so the field rule is pure redundant breakage.

**Why nothing caught it**: `tests/unit` is dead (S1); the e2e test skips when the ingestor HTTP service is
unreachable (`tests/integration/e2e_test.go:89`), which is the default local state.

**Where to look**: `packages/proto/sentinel/v1/error_event.proto:18-58` (CEL block + field comments),
`gen/sentinel/v1/error_event.pb.go` (regenerated), `tests/contract/sdk_ingestor_test.go`.

---

### S4 — The official Go SDK cannot talk to the ingestor (RESOLVED 2026-07-28/29 — together with S16, see below)

**Resolved together with S16.** The field-name renames documented below made the SDK's JSON *decodable* by
the ingestor, but did **not** by themselves make an SDK event survive to storage: the SDK's single
`ProjectKey` field was still carrying the secret API key into the body's `project_key`, which the server
resolved against `projects.name` — a value that is never an API key — so every renamed-and-decodable SDK
event was still 202'd and then permanently dead-lettered, silently, all the way through this fix. **S16
(below) is the reason S4 is not marked resolved until it is also fixed.**

**Re-verified 2026-07-29**:
- `go test -tags=contract ./tests/contract/... -v -count=1` → **4/4 PASS**. `TestSDKToIngestorContract_SingleEvent`
  and `_Batch` assert `event.GetMessage()`, `event.GetPlatform() == "go"`, and `event.GetMetadata()` are all
  non-empty on the *real* proto `ErrorEvent` built from a *real* SDK-produced, ingestor-decoded payload —
  each assertion is explicitly commented `"- S4 regression"` if it fails.
  `TestSDK_RejectsWithoutPlatform_WouldFailValidation` is a negative control proving the validator step
  actually rejects a missing `platform` (i.e. the positive assertions above are not a rubber stamp).
- `cd packages/sdk-go && go test ./... -count=1` → **PASS**, all 4 packages (`sdk-go`, `sentinellog`,
  `sentinelslog`, `sentinelzerolog`). `TestSendBatchDropsOn4xxWithoutRetry`,
  `TestSendBatchRetriesOn5xxThenSucceeds`, and `TestSendBatchDropsAfterExhaustingRetriesOnNetworkError` in
  `packages/sdk-go/transport_test.go` directly prove `sendBatch` now inspects `resp.StatusCode` (it
  previously discarded it entirely) and applies capped-backoff retry only to 5xx/network failures, not
  4xx.

**Original defect** (kept for history — field-name divergence table below; still useful as the concrete
list of what `tests/contract/sdk_ingestor_test.go` now guards against structurally, per bug pattern B5):

**Verified by probe**: the exact JSON `packages/sdk-go` emits, decoded into `validation.ErrorPayload`:

```
project_key="pk_live_x" platform="" env="production" message="" error_class="*errors.errorString" metadata=map[]
PROTOVALIDATE REJECTS: platform is required and must be lowercase alphanumeric; platform: value is required; message: must be 10000 characters
```

Three independent field-name divergences between `packages/sdk-go/event.go` and
`apps/ingestor-go/validation/validator.go`:

| SDK emits | Ingestor reads | Effect |
|---|---|---|
| `error_message` | `message` | message always empty |
| `context` | `metadata` | all user tags/PII-scrubbed context dropped |
| *(nothing)* | `platform` | **hard 400** — `platform` is required and must match `^[a-z0-9]+$` |
| `release_version` | *(no such field)* | dropped → regression detection can never fire (see S5) |

The SDK also **ignores the HTTP response entirely** (`transport.go` `sendBatch` discards `resp.StatusCode`),
so a 100%-rejection rate is completely silent on the client side. There is no logging of 4xx even with
`Debug: true`.

`docs/sdk-specification.md` is the intended contract; neither side currently matches it. Nothing tests the
contract because the SDK is a separate module (see module layout above).

**Fix**: `error_message`→`message`, `context`→`metadata`, added `platform:"go"`, `release_version`, per-frame
`in_app` (see S11). `sendBatch` now inspects `resp.StatusCode`, logs 4xx under `Debug`, retries 5xx with
capped backoff, and fires `Config.OnError`.

**PII side-effect found and fixed in the same change**: the `context`→`metadata` rename made a *dormant*
PII-scrubbing gap live — previously the SDK emitted its scrubbed map under a JSON key the ingestor never
decoded, so no client metadata reached Postgres at all, masked or not. Once `metadata` started arriving,
both scrubbers turned out to be incomplete: the SDK's `defaultPIIKeys`
(`packages/sdk-go/pii.go`) was missing `pass`/`passwd`/`bearer`/`card_number`/`cvv`/`social_security`, and
the server-side masker (`apps/processor-go/masker/masker.go`) matched keys by **exact equality**, letting
`user_password` and `x-api-key` through untouched. Both were extended/switched to substring matching.

**Where to look**: `packages/sdk-go/event.go`, `packages/sdk-go/transport.go`, `packages/sdk-go/pii.go`,
`apps/processor-go/masker/masker.go`, `apps/ingestor-go/validation/validator.go`,
`apps/ingestor-go/mapping/mapper.go`.

---

### S16 — The Go SDK's single `ProjectKey` field did double duty as the secret AND the routing name (NEW, RESOLVED 2026-07-28/29, together with S4)

Before this fix, `packages/sdk-go/config.go`'s `Config.ProjectKey` held the raw API key
(`sent_live_...`/`sent_org_...`/legacy `pk_live_...`) **and** was sent, unchanged, as the event body's
`project_key` field. The ingestor resolved `project_key` against `projects.name` (a project's unique
display name, never a secret) — so the value the server received there could never match any row, and
every SDK event, after correctly passing the S4 field-name fixes and protovalidate, was `202`'d and then
permanently dropped at the project-resolution step. Nothing surfaced this: the SDK still ignored the
response status until the S4 transport fix landed alongside this one, so the failure looked identical to
S4 from the outside — "SDK events don't show up" — and would have kept looking that way even after S4's
field renames shipped alone.

**Fix**: split `Config.APIKey` (the secret; sent only in the `X-API-Key` header, `json:"-"` so it can never
be marshaled into an event body) from `Config.ProjectKey` (the project's unique name; travels in the body
as `project_key`, used only by organization-wide keys to select a target project — see S6's org-wide
resolution order). `Config.Validate()` additionally checks `ProjectKey` against known API-key prefixes
(`looksLikeSecret`, `packages/sdk-go/config.go`) and refuses to start if it looks like a credential, so a
future instance of this exact swap fails loudly at startup instead of as a silent, unbounded rejection
rate.

**Verified 2026-07-29**: `cd packages/sdk-go && go test ./... -count=1` → PASS, including
`TestNewEventFieldsMatchWireContract`. `go test -tags=contract ./tests/contract/... -v -count=1` → 4/4 PASS
— `applyAuthenticatedScopeForTest` in the contract test mirrors the real `main.go` scoping logic (see S6)
and the resulting proto event's project identity round-trips correctly, which would not be possible if
`ProjectKey` still carried the secret.

**Where to look**: `packages/sdk-go/config.go` (`APIKey`, `ProjectKey`, `Validate`, `looksLikeSecret`),
`packages/sdk-go/CHANGELOG.md` (documents this as a breaking v0.2.0 change for existing integrators).

---

### S5 — Regression detection can never trigger (RESOLVED 2026-07-28/29)

**Re-verified 2026-07-29**: `apps/processor-go/event/event.go` — `Normalize` now reads the metadata
fallback for `ReleaseVersion` **before** calling `masker.MaskMap(normalizer.NormalizeMap(e.Metadata))`:

```go
if e.ReleaseVersion == "" {
    if rv, ok := e.Metadata["release_version"].(string); ok {
        e.ReleaseVersion = rv
    }
}
e.Metadata = masker.MaskMap(normalizer.NormalizeMap(e.Metadata))
```

and `Deserialize` now copies `protoEvent.ReleaseVersion` (proto field 15, added in the same change as S14)
onto `ErrorEvent.ReleaseVersion` — previously it did not copy this field at all, so even a correctly-ordered
`Normalize` would have had nothing to read. `go test -tags=contract ./tests/contract/... -v -count=1` → 4/4
PASS, including the explicit assertion `event.GetReleaseVersion() != wantReleaseVersion → "S5 regression"`
in `assertSemanticFieldsSurvived`. Live DB check this pass:
```
podman exec sentinel-postgres psql -U sentinel -d sentinel -c \
  "SELECT status, error_class FROM issues i JOIN error_occurrences o ON o.issue_id=i.id LIMIT 5;"
```
returned live `unresolved` rows with populated `release_version` from prior probe runs (see S12's live
row count, which is on the same tables).

**Additional defect found and fixed while proving this** (not itself the S5 bug, but blocked U11 —
regression tracking's headline proof — 100% of the time until fixed): `store.go`'s regression-activity
insert wrote a `metadata` column that `issue_activity` does not have (the schema, per
`1721900000_add_issue_lifecycle_and_relations.sql`, defines `old_value`/`new_value`, not `metadata`).
Every regression event failed the transaction with SQLSTATE `42703`, and `errors.go`'s new
`classifyStoreError` classifies `42703` as retryable (it is a class-`42` syntax/access error, not class-`23`
or `22`), so it burned the full delivery budget before dead-lettering instead of failing immediately. Fixed
by writing to `new_value` instead. **`issue_activity.old_value` is still never populated** — carried
forward as an explicit open gap, see "Known-broken, STILL OPEN" below.

**Original defect** (kept for history):

**Verified by probe**:

```
input:  metadata={"release_version": "1.4.2"}
after Normalize(): release_version="<VERSION>"  metadata={release_version:<VERSION>}  message="failed at <VERSION>"
```

`ErrorEvent.Normalize` (`apps/processor-go/event/event.go:41-49`) runs `NormalizeMap` over metadata *before*
reading `release_version` out of it at line 46. `normalizer.versionRegex` (`\bv?\d+\.\d+\.\d+...`) rewrites
every semver to the literal `<VERSION>`, so `ReleaseVersion` is always `"<VERSION>"`.

Downstream, `store.isRegressionVersion` parses `"<VERSION>"` with `strconv.Atoi` → `0` for every component,
so version comparison is meaningless. The regression path (`regression_status`, `regression_count`,
`last_regressed_at`, the `issue_activity` 'regressed' row) — the headline feature of 006 — is unreachable
with real data.

**Where to look**: `apps/processor-go/event/event.go` (`Normalize`, `Deserialize`),
`apps/processor-go/store/store.go` (`ResolveProjectID`, the `issue_activity` insert),
`apps/processor-go/service/errors.go` (`classifyStoreError`).

---

### S6 — Ingest is not tenant-scoped: any valid key can write to any project (RESOLVED 2026-07-28/29)

**Verification note**: the live curl-based cross-tenant probe from the original fix pass (project-scoped
key + body naming another project → 403; org-wide key naming a project in another org → 403, zero rows
written) could **not** be re-run against the shared dev database this pass — `project_api_keys` does not
currently exist in it despite both migration ledgers claiming it is applied (see the corruption hazard
under "How to verify" above, discovered live during this same pass). This entry is therefore verified by
**code inspection** of the merged fix plus the automated test suite below, not by re-executing the original
live probe. Re-run the curl probe once the shared DB is restored to a consistent state before fully
trusting this beyond the code-level guarantee.

**Fix, verified present in the staged diff and by `go build ./...` / `go vet ./...` passing**:
`apps/ingestor-go/main.go`'s new `applyAuthenticatedScope` (called from both `handleIngest` and
`handleBatchIngest`, per-item for batches) now forces a payload's tenancy onto the identity resolved from
the presented API key, not the request body:

- **Project-scoped key**: body may omit `project_key`/`project_id` or name the same project; naming a
  *different* one is `403`.
- **Organization-wide key**: target project comes from the `X-Project-Key` header (resolved by
  `auth.APIKeyAuthenticator.Middleware` via the new `auth.ResolveProjectInOrg`) if present, else the body's
  `project_key`, resolved the same way — **always scoped to the authenticated key's own organization**. A
  name that does not resolve within that organization is `403`, indistinguishable from "does not exist" (so
  it cannot be used to enumerate other tenants' project names).
- `projects.name` — the tenant routing key `GetProjectByKey` uses — now has a UNIQUE constraint scoped to
  the organization: `CREATE UNIQUE INDEX idx_projects_org_name ON projects (organization_id, name)`
  (`1721800000_add_organization_layer.sql`). **Verified live**:
  ```
  podman exec sentinel-postgres psql -U sentinel -d sentinel -c "\d projects" | grep -i unique
  →  "idx_projects_org_name" UNIQUE, btree (organization_id, name)
  ```
  Same-named projects across *different* organizations are still allowed (by design — the index is
  composite); duplicates within one organization are rejected.
- `context.WithValue`'s bare-`string`-key collision risk (the second, lower-severity half of the original
  finding) is also closed: `apps/ingestor-go/auth/context.go` introduces a private `ctxKey` type and typed
  accessors (`auth.WithIdentity`, `auth.ProjectKeyFromContext`, `auth.ProjectIDFromContext`,
  `auth.OrganizationIDFromContext`, `auth.APIKeyHashFromContext`, `auth.RateLimitRPMFromContext`,
  `auth.IsOrgWideKey`). **This same change silently broke rate limiting — see R1 below**, a direct
  illustration of why the collision risk being closed is not free.

**Automated coverage**: `go test -tags=contract ./tests/contract/... -v -count=1` → 4/4 PASS; the tests call
`applyAuthenticatedScopeForTest`, a test-local mirror of the real scoping logic, and assert the resulting
proto event's project identity is the authenticated one. There is no `apps/ingestor-go/main_test.go`
exercising `applyAuthenticatedScope` itself with real HTTP requests and a real Postgres-backed 403 —
that gap is real and is why the live curl probe (above) still matters and should be re-run.

**Original defect** (kept for history):

`apps/ingestor-go/auth/apikey.go:83` resolves the authenticated project and stores it in the request context:

```go
ctx := context.WithValue(r.Context(), "project_key", projectKey)
```

**`handleIngest` and `handleBatchIngest` never read it.** `apps/ingestor-go/main.go:146-165` decodes
`payload.ProjectKey` straight from the request body and publishes it. `processor-go` then resolves that
key with `SELECT id FROM projects WHERE name = $1` (`store.go` `GetProjectByKey`).

Net effect: a holder of *any* active `ingest`-scope key for *any* organization can write error events into
*any other tenant's project* by naming it in the JSON body. The authenticated identity is computed, logged
about in the audit trail, and then discarded.

Related, same file:
- `context.WithValue` uses bare `string` keys (`"project_key"`, `"rate_limit_rpm"`, `"api_key_hash"`) —
  collision-prone across packages; Go convention requires a private key type.
- `projects.name` is used as the tenant routing key and has **no UNIQUE constraint**
  (`1716508800_init.sql`), so two orgs can own projects with the same name and `GetProjectByKey` picks
  arbitrarily.

**Where to look**: `apps/ingestor-go/main.go` (`applyAuthenticatedScope`), `apps/ingestor-go/auth/apikey.go`
(`ResolveProjectInOrg`), `apps/ingestor-go/auth/context.go`,
`packages/db-migrations/migrations/1721800000_add_organization_layer.sql`,
`apps/processor-go/store/store.go` (`ResolveProjectID`).

---

### S11 — Fingerprints collapse when there are no in-app frames (RESOLVED 2026-07-28/29)

**Re-verified 2026-07-29**: `apps/processor-go/fingerprint/fingerprint.go` now falls back to the top
`MaxAppFrames` frames (regardless of `InApp`) when zero frames are marked in-app:

```go
if len(appFrames) == 0 {
    for _, frame := range cfg.Stacktrace {
        appFrames = append(appFrames, fmt.Sprintf("%s:%s", frame.File, frame.Function))
        if len(appFrames) >= MaxAppFrames {
            break
        }
    }
}
```

`go test -tags=contract ./tests/contract/... -v -count=1` → 4/4 PASS, including the explicit assertion
`"proto ErrorEvent.stacktrace has no in_app==true frame - S11 regression"` in `assertSemanticFieldsSurvived`
— the SDK side of this fix (`isInAppFrame`, `packages/sdk-go/event.go`) is proven by
`TestIsInAppFrame` (6 subtests, all PASS) and `TestExtractStacktracePopulatesInApp` in
`packages/sdk-go`'s own test suite.

**Original defect** (kept for history):

`apps/processor-go/fingerprint/fingerprint.go` builds its hash input from `ErrorClass` plus up to 3 frames
where `InApp == true`. When a stacktrace has no `in_app` frames — which is the default for the Go SDK, since
`packages/sdk-go/event.go` `Frame` has no `InApp` field at all and never sets one — the input degenerates to
the error class alone. **Every error of a given class in a project collapses into a single issue**, losing all
grouping fidelity.

**Where to look**: `apps/processor-go/fingerprint/fingerprint.go`, `packages/sdk-go/event.go` (`isInAppFrame`).

---

### S12 — Two contradictory CHECK constraints made `issues` uninsertable — the database had ZERO rows for the life of the project (NEW, RESOLVED 2026-07-28/29)

`1716508800_init.sql:22` defined an inline `CHECK (status IN ('open','resolved','ignored'))` on `issues`
(auto-named `issues_status_check`). `1721900000_add_issue_lifecycle_and_relations.sql:23` later added a
**second**, differently-named constraint, `check_status CHECK (status IN ('unresolved','resolved','ignored'))`,
without ever dropping the first. Both were simultaneously live. Their intersection — the only status values
that satisfy *both* constraints — is `{'resolved', 'ignored'}`. `apps/processor-go/service/processor_service.go`
writes `'unresolved'` on every insert. `'unresolved'` satisfies the second constraint (`check_status`) but
**not** the first (`issues_status_check`, which only allows `'open'`/`'resolved'`/`'ignored'`) — and Postgres
enforces every CHECK constraint on the table, not just one of them — so every `INSERT`/`UPDATE` writing
`'unresolved'` (the only status the application ever writes) failed with SQLSTATE `23514`. Since
issue creation is the first write in the pipeline, **every event, from the beginning of the project, failed
at this step. The `issues` and `error_occurrences` tables had never held a row.**

**Fix**: `1721900000_...sql` now `ALTER TABLE issues DROP CONSTRAINT IF EXISTS issues_status_check;` before
any data backfill, leaving `check_status` as the sole status constraint. The down migration restores the
original inline constraint so a subsequent `up` on the same schema has something to act on again. All
statements in the affected migration were also made replay-safe (`IF EXISTS`/`IF NOT EXISTS`/`DO $$` guards)
as part of the same change — see the note on migration idempotency vs. D4 under "Known-broken, STILL OPEN"
in the durable decisions record.

**Verified live, 2026-07-29**:
```
podman exec sentinel-postgres psql -U sentinel -d sentinel -c \
  "SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='issues'::regclass AND contype='c';"
```
```
             conname             |                          def
 ---------------------------------+-----------------------------------------------------------------
  issues_regression_status_check | CHECK (regression_status::text = ANY (...))
  issues_issue_type_check        | CHECK (issue_type::text = ANY (...))
  issues_source_channel_check    | CHECK (source_channel::text = ANY (...))
  issues_assignee_type_check     | CHECK (assignee_type::text = ANY (...))
  issues_resolved_by_type_check  | CHECK (resolved_by_type::text = ANY (...))
  check_status                   | CHECK (status::text = ANY (ARRAY['unresolved','resolved','ignored']))
 (6 rows)
```
Exactly **one** status constraint remains. And, decisively:
```
podman exec sentinel-postgres psql -U sentinel -d sentinel -c "SELECT count(*) FROM issues;" -c "SELECT count(*) FROM error_occurrences;"
→  issues: 3   error_occurrences: 11
```
Non-zero rows exist in both tables, on this live instance, right now — the first time this has ever been
true.

**Where to look**: `packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql`.

---

### S13 — Poison-message livelock: a permanently-malformed event starved the entire pipeline (NEW, RESOLVED 2026-07-28/29)

`packages/shared-go/nats/subscriber.go` previously Nak'd any handler failure with no `MaxDeliver` cap and no
dead-letter path (`scripts/nats-init.sh` provisions the consumer with `--defaults`, i.e. unlimited
redelivery). A permanently-unprocessable message (any content-caused failure — a constraint violation, a
malformed payload) would be redelivered forever and, being pulled back into every `Fetch` batch ahead of
newer messages, **starved all subsequent events**. Per the evidence brief that prompted this fix: ~510
unique events produced 5,874 processing attempts and no newly-published event was ever observed to be
processed. This measurement was not independently reproduced in this pass (it requires generating sustained
poison-message load); the code fix and its build/test verification below were.

**Fix**: `MaxDeliver` (default 5) and a DLQ are now first-class `SubscriberConfig` fields, enforced both
server-side (JetStream consumer config) and client-side (`handleMessage` checks `msg.Metadata().NumDelivered`
directly, so behavior does not depend on whichever process created the durable consumer first — see the
in-code comment on the post-startup consumer reconciliation goroutine for why that race exists at all). A
new `nats.PermanentError` / `nats.Permanent(err)` / `nats.IsPermanent(err)` lets callers mark a failure as
non-retryable so it dead-letters immediately instead of burning its whole retry budget — used by
`apps/processor-go/service/processor_service.go` for deserialize failures and by the new
`apps/processor-go/service/errors.go` (`classifyStoreError`, `classifyProjectLookupError`) for Postgres
constraint/data-exception errors (SQLSTATE classes `23`/`22`). Retryable failures now use `NakWithDelay`
on a 1s/5s/15s/30s/60s backoff schedule (≈51s total across 5 attempts) instead of a bare `Nak()`, so a
transient DB blip does not exhaust the retry budget before the database has a chance to come back — this
was deliberately traded off against maximum dead-letter speed to stay compatible with D1/D2's "buffer and
recover" guarantee (see S9 below for where that guarantee currently still breaks).

A related, previously undocumented bug fixed in the same change: `sendError` used to be a **blocking** send
on the capacity-1 `Errors()` channel — any caller that did not actively drain `Errors()` (the ingestor's
api-key-invalidation subscriber, `apps/ingestor-go/auth/apikey.go:36`, still does not) would have its fetch
goroutine wedge permanently on the second error. `sendError` is now a non-blocking `select`/`default` that
increments a `droppedErrors` counter (exposed via `Subscriber.DroppedErrors()`) instead. **The
api-key-invalidation subscriber still never calls `Errors()`**, so its errors are silently dropped and
counted rather than surfaced anywhere — no longer fatal, but still invisible; nothing currently reads
`DroppedErrors()`.

**Verified 2026-07-29**: `go build ./...` and `go vet ./...` both pass with the rewritten
`packages/shared-go/nats/subscriber.go` (391 lines changed). `go test ./tests/unit/... -count=1` → 253/253
pass (no unit test exists directly for `subscriber.go` itself — it has no `_test.go` file in
`packages/shared-go/nats/` — so this fix is verified by code inspection plus the fact that everything
downstream that depends on it, including the full contract suite, still passes).

**New durable decision**: **D10 | Bounded-Retry NATS Delivery with Dead-Letter Capture** in
`docs/memory/DECISIONS.md` / `docs/memory/INDEX.md` records this design; do not duplicate its rationale
here.

**Known gap carried forward, not fixed by this change**: the DLQ has no consumer — no drain, no replay, no
alerting, no dashboard surface. D10 records this explicitly as an accepted, out-of-scope gap.

**Where to look**: `packages/shared-go/nats/subscriber.go`, `apps/processor-go/service/errors.go`,
`apps/processor-go/service/processor_service.go`, `docs/memory/DECISIONS.md` (D10).

---

### S14 — Second instance of the S3 bug class: unbounded fields against bounded columns (NEW, RESOLVED 2026-07-28/29)

Several `ErrorEvent` fields had no length rule at all despite writing into fixed-width Postgres columns:
`platform`/`environment` (VARCHAR(50)), `error_class` (VARCHAR(255)), `trace_id`/`span_id` (VARCHAR(64)),
and `release_version` (VARCHAR(100) — a column added by the same P2-1 work that introduced this bug, i.e.
a brand-new field shipped with the same class of gap S3 had just been fixed for). Per the evidence brief,
a probe with a 5000-character `error_class` returned `202 Accepted` and then failed to actually store —
protovalidate accepted it, and the `VARCHAR(255)` column rejected the write later, with no useful error
surfaced back to the 202'd caller.

**Fix**: CEL rules added to `packages/proto/sentinel/v1/error_event.proto` matching each column's real
width (`error_event.error_class` ≤ 255, `error_event.trace_id`/`error_event.span_id` ≤ 64,
`error_event.release_version` ≤ 100, `error_event.fingerprint` ≤ 64, and the existing `platform`/
`environment` rules extended from format-only to format-and-length ≤ 50). Every field now carries an
inline comment naming the exact column and migration it must stay in sync with — an attempt to make the
next schema-width change grep-discoverable instead of silently drifting again.

**Verified 2026-07-29**: `go build ./...` passes with the regenerated `gen/sentinel/v1/error_event.pb.go`.
`go test -tags=contract ./tests/contract/... -v -count=1` → 4/4 PASS (uses a realistic, in-bounds SDK
event, so does not itself re-probe the 5000-char rejection case — that specific probe was not re-run this
pass). `go vet ./...` clean.

**Where to look**: `packages/proto/sentinel/v1/error_event.proto` (CEL block + per-field width comments).

---

### S7 — API key revocation and expiry did not take effect (RESOLVED 2026-07-29, with one bounded caveat)


**Verified unchanged, 2026-07-29.** Confirmed by reading current code, not by assumption:

1. **Subject payload mismatch, still present.** The dashboard still publishes `{ keyId }`
   (`apps/dashboard-web/src/lib/db/queries/apikeys.ts:139`, unchanged in the P0/P1/P2 diffs); the
   ingestor's invalidation handler still reads `data["key_hash"]`
   (`apps/ingestor-go/auth/apikey.go:39`, unchanged). The key it needs is never in the message, so the
   Redis cache entry is never deleted on revocation.
2. **The stream now exists** — this part of the original finding is stale and should not be repeated.
   `scripts/nats-init.sh` was extended (in the already-committed P0/P1 work, `cd84d17`) to also create the
   `API_KEYS` stream and its consumer; `subscriber, _ := nats.NewSubscriber(...)` in `main.go` still
   discards the error, but the stream it is subscribing to is provisioned now.
3. `getAPIKeyData` (`apps/ingestor-go/auth/apikey.go:126-165`) still filters on `status` only and
   **never checks `expires_at`** — the column is not even selected in the query. `rotateApiKey`
   (`apps/dashboard-web/src/lib/db/queries/apikeys.ts:67-109`) still sets `expiresAt` on the old key but
   leaves `status` untouched (stays `'active'`) — so a rotated key remains valid **forever**, not just for
   whatever grace period `expiresAt` implies.

Practical revocation latency today is still the 60-second Redis TTL, not "instant" as `WORKLOG.md` states,
and rotation still grants no real expiry. Not re-verified live this pass (would require a working
`project_api_keys` table — see the DB-corruption hazard above); verified by direct code reading only.

**Where to look**: `apps/ingestor-go/auth/apikey.go`, `apps/dashboard-web/src/lib/db/queries/apikeys.ts`.

---

**Resolved 2026-07-29.** All three breaks are closed:
- `expires_at` is enforced in the SQL itself (`auth/apikey.go` getAPIKeyData), so an expired row is
  indistinguishable from a missing one. Seeded `expires_at = now() - 1 hour` → **401**; control key → 202.
- `rotateApiKey` now sets `status='revoked'` + `revoked_at` on the old key immediately. Old token → **401**,
  new token → **202**, `audit_logs` row written.
- The invalidation payload matches byte for byte on both sides — the dashboard publishes
  `{keyId, keyHash}` and the ingestor reads `data["keyHash"]`. Observed: cached key 202 → publish →
  Redis TTL `-2` → next request **401**, in ~2s rather than the 60s TTL.
- The scope check is now an allowlist matching spec 008 FR-003 (`read` → 403, `ingest`/`admin` → 202).
- `apps/ingestor-go/main.go` no longer discards the subscriber error: a NATS hiccup at startup used to
  leave invalidation permanently dead, silently, for the life of the process.

**Bounded caveat, deliberately not closed.** The cache-hit expiry check reads `ExpiresAt` from the
CACHED payload, so an expiry written or shortened AFTER caching is invisible for the remainder of the
TTL. Application-driven revocation and rotation do not wait for it — they publish the invalidation. An
out-of-band `UPDATE project_api_keys SET expires_at = ...` in psql does, and is bounded by
`APIKEY_CACHE_TTL` (default 60s, env-tunable). That is true of any cache; it is documented rather than
claimed closed.

---

### S8 — Alerting was implemented and completely unwired (RESOLVED 2026-07-29)


**Verified unchanged, 2026-07-29.** `apps/processor-go/service/processor_service.go`'s `NewProcessorService`
(read in full this pass) still constructs only `store`, `indexer`, and `degradation` — no `alerts.Dispatcher`
field exists on `ProcessorService` at all, and `processEventInternal` never references `Dispatch`. None of
the staged P2/P2b diff touches `apps/processor-go/alerts/` or `apps/processor-go/notifiers/`.

- `apps/processor-go/alerts/dispatcher.go` (204 LOC) — still never constructed.
- `apps/processor-go/notifiers/{email,telegram}.go` (221 LOC) — still zero non-test references.
- `apps/ingestor-go/validation/validator.go` `ValidatePayload` — still only referenced by
  `tests/unit/ingestor_validation_test.go`; the real ingest path uses `protovalidate`.

`docs/todos/04-alerting-and-notification-integrations.md` is therefore still entirely open despite the
packages existing and (per S1) their tests now actually running. Also unchanged: `Dispatcher.loadConfigs`
still only refreshes on a 5-minute ticker with no initial load.

**Where to look**: `apps/processor-go/service/processor_service.go`, `apps/processor-go/alerts/dispatcher.go`,
`apps/processor-go/notifiers/`.

---

**Resolved 2026-07-29, in two stages — the second mattered more than the first.**

Stage one wired it: `NewProcessorService` constructs the Dispatcher, `SetSender` connects the real
email/telegram notifiers, and config load happens at construction rather than only on a 5-minute ticker.
Reachability was proven by capturing the production binary making an outbound notifier call.

Stage two fixed two blockers that made the wiring useless — **it was reachable and still could not
deliver for ANY configuration**:
- `channel_config` was scanned into a local and then DISCARDED, never unmarshaled, so `ChannelConfig`
  was nil for every config and both permitted channels dropped every alert at the notifier.
- The gating cancelled out the threshold: dispatch fired only on new/regressed issues, but `Dispatch`
  sends at `count >= frequency_threshold`, which is `NOT NULL DEFAULT 50`. An issue is new exactly once.
  Every occurrence now feeds the counter, which is what the threshold is for.
- Latent: `&cfg.ProjectID` was passed twice in one `Scan`, working only because the second write
  overwrote the first. Any reorder of the SELECT list would have corrupted the tenant routing key.

**Lesson worth keeping**: "reachable from `main()`" is necessary and not sufficient. Alerting passed a
reachability grep, an execution trace, and ~1,100 lines of tests while being incapable of delivering.

---

### S9 — The degradation buffer's control flow was inverted (RESOLVED 2026-07-29 by DELETING the buffer)


**Verified unchanged, 2026-07-29** by reading `apps/processor-go/degradation/buffer.go` (`CheckAndBuffer`)
and `apps/processor-go/service/processor_service.go` (`ProcessEvent`, lines 36-43) directly — the only
staged change to `processor_service.go` is the new error classification
(`classifyStoreError`/`classifyProjectLookupError`, part of S13's D10 work), not this control flow.
`rtk go test ./tests/unit/... -run TestCheckAndBuffer -v -count=1` → 4/4 pass, but the tests **assert the
current (inverted) behavior as the contract** (`TestCheckAndBuffer_DBDown_ReturnsTrueOnPushSuccess`), so a
green run here does not mean this is fixed — it means the tests correctly describe what the code still
does.

```go
if !s.degradation.CheckAndBuffer(ctx, data) {
    log.Printf("Event buffered due to database unavailability")
    return nil          // ← ACKs the NATS message
}
return s.processEventInternal(ctx, data)
```

`CheckAndBuffer` still returns `true` in two situations the caller cannot distinguish — DB healthy, *and*
DB down but the event was successfully buffered — so a successfully-buffered event still falls through to
an immediate live write against the down database right away, rather than the call simply returning
"queued". `CheckAndBuffer` returns `false` only when the DB is down **and** the buffer is full: the event
is silently dropped, the misleading "Event buffered..." log line fires anyway, and `ProcessEvent` returns
`nil` — which the NATS subscriber Acks as success. **This is still real, silent data loss reported as
success.**

**Interaction with S13/D10**: D10's bounded retry/backoff now covers the immediate-write branch above when
it fails with a *retryable* error — redelivery with 1s/5s/15s/30s/60s backoff instead of a hot loop, and an
event that exhausts `MaxDeliver` is dead-lettered (preserved, unprocessed) rather than lost outright. D10
does **not** reach the buffer-full path: that path returns `nil` directly and never produces an error for
NATS to retry or dead-letter in the first place. So S13 measurably softened this defect's worst case
(hot-loop amplification) without fixing its core inversion.

**Corroborated same-day**: `docs/memory/DECISIONS.md`'s "Update (2026-07-29)" under the graceful-degradation
decision independently re-derives this same conclusion, citing
`tests/integration/processor_service_test.go:256-293,491-532` by name (`TestProcessorService_ProcessEvent_DBUnavailableBuffersEvent`,
`TestProcessorService_ProcessEvent_BufferFullReturnsNil`) — not re-run in this pass (needs
testcontainers/a live unreachable-Postgres target); this file's `TestCheckAndBuffer` run above is the
unit-level corroboration.

**Where to look**: `apps/processor-go/degradation/buffer.go`, `apps/processor-go/service/processor_service.go:36-43`,
`docs/memory/DECISIONS.md` (graceful-degradation entry, "Update (2026-07-29)").

---

**Resolved 2026-07-29 — but not by fixing the inversion.** The first repair introduced a worse bug: it
ACKed events held only in process memory, so a crash mid-outage destroyed them (3 events → 0 rows, no
redelivery, no DLQ entry). It traded a duplicate-delivery bug for a permanent-loss bug.

The buffer is now **deleted**. `GracefulDegradation` is a database-health gate returning
`StatusProcessed` or `StatusUnavailable`; `ProcessEvent` returns an error on the latter, so D10's
bounded retry and DLQ own recovery. See DECISIONS.md D1 (superseded) for why buffering could not be
repaired, and BUGS.md B1.

**Proving command** — real stack, real binaries:
```
docker compose up -d --build && ./scripts/wait-healthy.sh
# stop sentinel-postgres, POST 5 events to /ingest (all 202), restart postgres
# => 5 issues (count=1 each), 5 occurrences, 0 loss, 0 duplicates
```
Harder drills: SIGKILL mid-outage → still 5/5 exactly once; outage beyond the retry budget → 3 events
dead-lettered with `dlq_depth=3` on `/health` at ~56s.

**Residual, formerly knowingly accepted — now S18, RESOLVED 2026-07-31**: the issue upsert and
occurrence insert were separate transactions, so a retryable failure between them inflated
`issues.count` by one per redelivery. This paragraph was the defect's only record for months (it was
widely mislabeled "S16", which is actually the ProjectKey secret/name split); it has its own entry as
S18 below, where the fix — `event_id` idempotency and a single-transaction write path — is verified.

---

### S15 — The SDK reported a partially-failed batch as complete success (NEW, RESOLVED 2026-07-29)

**Original defect.** `handleBatchIngest` had no batch-size cap and no request body limit on the only
externally exposed service, and returned `202` even when every item failed — so a caller could not
distinguish total failure from success. The SDK compounded it: `sendBatch` returned on any 2xx without
reading the body, so a batch of 3 valid + 2 invalid events was recorded as fully successful. Nothing was
logged under `Debug`, `OnError` never fired, and the drop counter never incremented. This is S4's failure
mode (silent loss) re-run at per-item granularity.

**Resolved 2026-07-29.** Server side: `maxBatchSize = 500`, `http.MaxBytesReader` on both the single and
batch paths, `413` for an over-cap batch, and a `{ingested, failed, errors:[{index, message}]}` body with
`400` when `ingested == 0`. Client side: `transport.go` parses that body on 2xx and surfaces each per-item
failure through the same path a 4xx takes.

**Verified** by code inspection (`apps/ingestor-go/main.go:41,186,243-245,266-271`;
`packages/sdk-go/transport.go:250-290`) and by the SDK's own tests
(`cd packages/sdk-go && GOWORK=off go test ./... -count=1`).

**Note on provenance**: an earlier evidence brief listed S15 as "still open — its agent died before
reporting". The work had in fact landed; the agent failed on its structured-output return, not on the task.
A missing report is not evidence of missing work — check the tree.

---

### S17 — Ingestor hot-spin: a deadline-less context turned an idle service into a log-flooding CPU spinner (NEW, RESOLVED 2026-07-28/29)

The NATS `Subscriber.Subscribe` pull loop called `sub.Fetch(batchSize, nats.Context(ctx))` using whatever
context the caller passed straight through. `apps/ingestor-go/auth/apikey.go`'s invalidation subscriber
passes `context.Background()` — no deadline — which made every `Fetch` call return **instantly** with
`"nats: context requires a deadline"`, and the loop had no backoff, so it spun as fast as the CPU would
allow. Per the evidence brief: **55.8% CPU on a fully idle ingestor**, and the resulting error volume
flooded the log pipeline hard enough to suppress output for the whole pod (this is also why `sendError`'s
non-blocking-with-rate-limited-log fix under S13 matters here specifically).

**Fix**: every `Fetch` call now gets its own bounded per-call deadline derived from
`SubscriberConfig.BatchWait` (defaulting to 5s), via `context.WithTimeout(ctx, wait)`, independent of
whatever the caller's own context looks like. The error-classification branch was also corrected to treat
`context.DeadlineExceeded` from this new per-fetch timeout the same as `nats.ErrTimeout` from `MaxWait` —
both are "empty fetch window", not errors — and the drop-log path added under S13
(`dropLogInterval = 10 * time.Second`) rate-limits any error that does escape.

**Verified 2026-07-29**: `go build ./...` / `go vet ./...` pass. The specific CPU measurement (brief reports
**0.08%** after the fix, down from 55.8%) was not independently reproduced this pass — it requires running
the ingestor idle against a live NATS deployment with the invalidation subscriber active and sampling CPU,
which was not repeated here; the code change (bounded per-fetch deadline, corrected error classification)
was confirmed present by reading `packages/shared-go/nats/subscriber.go` directly.

**Where to look**: `packages/shared-go/nats/subscriber.go` (`Subscribe`'s fetch loop), `apps/ingestor-go/auth/apikey.go:36`.

---

### R1 — Self-inflicted: typed context keys silently disabled rate limiting on 100% of requests (NEW, introduced and RESOLVED within this same body of work, 2026-07-28/29)

**This is a trap worth reading even if you are not touching rate limiting.** Introducing a private `ctxKey`
type in `apps/ingestor-go/auth/context.go` (to close the bare-string-key collision risk noted under the
original S6 finding — itself a correct, good change) silently disabled rate limiting for every single
request, immediately, with no error and no log line, the moment it was deployed:

`apps/ingestor-go/middleware/ratelimit.go` still read the identity out of context the old way:

```go
apiKeyHash, ok := r.Context().Value("api_key_hash").(string)   // bare string literal key
```

`context.Value` matches on **both the key's dynamic type and its value** — a `ctxKey("api_key_hash")` and a
bare `"api_key_hash"` string are never equal as context keys, even though they print identically. So the
type assertion `.(string)` on a lookup that now always missed had `ok == false` on every request, and
`ratelimit.go`'s own logic for that case is `if !ok || apiKeyHash == "" { next.ServeHTTP(w, r); return }` —
the **fail-open, unauthenticated-request path**, taken for every authenticated request too. Rate limiting
did not fire for any key, at any RPM, with no observable symptom short of noticing quota was never
enforced.

**Why nothing caught it**: three existing test files hand-injected the *old* bare-string context keys
directly (matching the now-broken production read), so the tests exercised exactly the shape that was
broken and passed regardless.

**Fix**: `middleware/ratelimit.go` now reads via `auth.APIKeyHashFromContext(ctx)` /
`auth.RateLimitRPMFromContext(ctx)`, the same typed accessors `context.go` defines and
`apikey.go`'s middleware now writes through `auth.WithIdentity(...)`. Tests were updated to build context
via `auth.WithIdentity` instead of hand-injecting bare keys, so a future re-introduction of this exact
mismatch would fail the test instead of passing it.

**Verified 2026-07-29**:
```
rtk go test ./apps/ingestor-go/middleware/... -v -count=1
```
→ 1/1 PASS (`TestRateLimiterMiddleware`, updated to build its context via `auth.WithIdentity`).
`go build ./...` / `go vet ./...` pass. The specific end-to-end proof from the fix pass — 12 requests
against a limit of 5 → `202 202 202 202 202 429 429 429 429 429 429 429`, with `Retry-After: 60`,
`X-RateLimit-Limit: 5`, `X-RateLimit-Remaining: 0` — is corroborated by a live row in the shared dev
database from that exact probe run (`SELECT message FROM issues` shows `'RL probe'`, `status='unresolved'`,
5 distinct occurrences persisted); it was not independently re-run this pass because the shared DB's
`project_api_keys` table is currently missing (see the corruption hazard above) and a fresh probe needs a
working, authenticated key.

**Never read this context by string literal again** — the code comment in `ratelimit.go` says so directly,
and this entry exists so the next incident report links back to a name.

**Where to look**: `apps/ingestor-go/middleware/ratelimit.go`, `apps/ingestor-go/auth/context.go`,
`apps/ingestor-go/auth/apikey.go`, `apps/ingestor-go/middleware/ratelimit_test.go`,
`docs/memory/DECISIONS.md` ("Correction (2026-07-29) — rate limiting was silently 100% bypassed...").

---

### Test isolation: `tests/integration` was silently driving the COMPOSE ingestor (RESOLVED 2026-07-31)

**The defect.** `TestIngestAndProcess` and `TestSearchIndexing` failed `Expected status 202, got 401`
for an unknown length of time, and passed in CI — so they were written off as "pre-existing, cause not
diagnosed". The cause: `tests/integration/testcontainers/ingestor.go` ran the container with
`NetworkMode: "host"` and a hardcoded port 8080. On any developer machine with the compose stack up,
8080 is taken, the container cannot bind, and **three separate fallback paths returned
`{localhost, 8080}` with a NIL error** — while the "health check" probed `localhost:8080`, which the
*compose* ingestor answered. A container that never started reported `Health check passed - container
is actually running`, and the suite then drove a different ingestor, against the **shared dev
database**, while the test had seeded its project in the **testcontainer database**. Hence the 401:
the key genuinely was not in the database that ingestor read. CI passed because nothing was there to
collide with or to borrow.

This is the same shape as the foreign-NATS-on-4222 trap `tests/e2e/main_test.go` already guards with
`ConnectedServerName()` — a well-known port answered by the wrong process — and a B9: two correct
halves the harness never connected.

**Fixed** by giving the container its own OS-assigned port (`PORT`, which
`apps/ingestor-go/main.go` already supported *precisely* for this — its comment says so), deleting
every silent fallback so provisioning failure is loud, and polling the container's OWN port for
health with container logs dumped on timeout.

**A second bug of the same family, found once the first was gone:** under host networking the
container's `REDIS_ADDR` also defaulted to the compose redis, and the ingestor caches API-key→project
lookups there — so one run's cached project id leaked into the next. `redisAddr` is now an explicit,
non-empty-checked parameter.

**Verified**: `FORCE_TESTCONTAINERS=1 go test ./tests/integration/` → **78 passed, 0 failed, 9
skipped** (was 76/2/9). Load-bearing: re-hardcoding the port to 8080 reproduces the original
`Expected status 202, got 401` exactly; reverting restores green.

### `batchUpdateIssues` — the "deadlock" does NOT reproduce; a cross-tenant write did (2026-07-31)

Recorded because a *correction* to a prior finding is worth more than the finding was. An earlier
review reproduced a deadlock and attributed it to `batchUpdateIssues`; sorting the id list was
proposed as the fix. Probed against a real Postgres:

- The UPDATE plans as a **Bitmap Heap Scan** (`EXPLAIN`), so rows lock in PHYSICAL order — the `IN`
  list's order is irrelevant. Two concurrent calls deadlocked with neither **reversed** nor
  **identical** id lists, nor across an FK/UPDATE crossing interleaving.
- The original trace was produced against **per-row UPDATEs in a loop**, a different statement shape
  from the single statement this function issues.

So the sort was inert and was removed rather than shipped under a confident comment — the function now
carries a note explaining what was probed, and that a real future deadlock should start from a
captured `pg_locks` cycle (`SELECT ... FOR UPDATE ORDER BY id` controls lock order; reordering an `IN`
list does not).

**What the investigation did find is real:** the UPDATE is tenant-scoped
(`WHERE project_id = $1 AND id IN (...)`) but the activity insert mapped over *every id the caller
sent*. An id from another project got **audit history appended to that tenant's issue** while its
status was correctly left alone — a cross-tenant write driven by request-body input, which is exactly
what **B7** exists to prevent. Activity now derives from the UPDATE's `RETURNING` rows, and the
returned count is what actually changed. A batch cap (500, matching the ingestor's `maxBatchSize`)
bounds lock duration and the DoS shape. Both guards observed failing before acceptance.

## Known-broken, STILL OPEN

Each entry below was independently re-read against current code this pass (not assumed unchanged from
2026-07-26). Where a same-day correction also exists in `docs/memory/DECISIONS.md`, it is cited as
corroboration, not as a substitute for this file's own evidence.

## S10 — Rate limiting is non-atomic and fails open

**Verified unchanged, 2026-07-29** by reading `apps/ingestor-go/middleware/ratelimit.go` in full (R1's fix
touched only the identity-lookup lines, not this section):

```go
if rl.client == nil {
    next.ServeHTTP(w, r)   // still: nil Redis client → rate limiting silently skipped
    return
}
...
rl.client.ZRemRangeByScore(ctx, redisKey, "0", ...)   // 1
count, err := rl.client.ZCard(ctx, redisKey).Result()  // 2
...
rl.client.ZAdd(ctx, redisKey, ...)                      // 3
rl.client.Expire(ctx, redisKey, time.Minute)             // 4
```

Still four separate, unpipelined Redis round-trips (`ZRemRangeByScore` → `ZCard` → decide → `ZAdd` →
`Expire` — one more call than originally documented, `Expire` was already there). Concurrent requests still
all read the same `ZCard` count before any writes land, so the effective limit under concurrency is still
unbounded. `RATELIMIT_STRICT_MODE` still only covers the `ZCard`-error path (line ~62), not the nil-client
path (line ~44).

**What changed**: `docker-compose.yml` now defines a real `redis` service (`redis:7-alpine`, with a
healthcheck) and the `ingestor` service now sets `REDIS_ADDR: redis:6379` and depends on
`redis: condition: service_healthy` — the "no redis in compose, so this always runs fail-open locally"
half of the original finding is **stale** and should not be repeated as-is. **What did not change**:
`apps/ingestor-go/main.go`'s `redisClient, _ := redis.NewClient(ctx, redisCfg)` still discards the
connection error — this line is untouched by the staged diff (confirmed by `git diff --cached HEAD --
apps/ingestor-go/main.go`, which shows no hunk near this line). So the fail-open path is now much less
likely to trigger locally (Redis is provisioned and health-gated), but the code still cannot distinguish
"Redis is fine" from "Redis is down, rate limiting is now silently off" if it ever does fail after
startup or the compose dependency is bypassed.

**Corroborated same-day**: `docs/memory/DECISIONS.md`'s 2026-07-29 correction on the same decision entry
independently states: *"S10 (rate limiting is still 4 unpipelined Redis round-trips ... and
`middleware/ratelimit.go:44-47`'s `if rl.client == nil { next.ServeHTTP(...); return }` still fails open ...)
remain[s] open."* The same correction also retracts D9's "hierarchical" (org→project→key) rate-limiting
claim as never having been true: `rtk grep -rn "rate_limit_rpm" apps/ingestor-go
packages/db-migrations/migrations` finds exactly one column, `project_api_keys.rate_limit_rpm` — there is
no project tier, and none was added by this body of work.

**Where to look**: `apps/ingestor-go/middleware/ratelimit.go`, `apps/ingestor-go/main.go` (redis client
construction), `docker-compose.yml`, `docs/memory/DECISIONS.md` ("hierarchical" rate-limiting correction).

---

## Lower-severity notes

Re-checked against the staged diff this pass; items resolved by the same P2/P2b work are marked so rather
than silently dropped — the point of this section is to not re-lose a finding on the way to fixing it.

- **RESOLVED (S13/S17)**: ~~`packages/shared-go/nats/subscriber.go`: `s.errors <- err` on a capacity-1
  channel, never drained by the ingestor, blocks the subscriber goroutine permanently after the second
  error.~~ `sendError` is now a non-blocking `select`/`default` with a `droppedErrors` counter. The
  ingestor's api-key-invalidation subscriber still never calls `Errors()` (see S13), but that no longer
  wedges the goroutine — it silently drops and counts instead.
- **RESOLVED (S13)**: ~~a handler error triggers a bare `msg.Nak()` with no max-delivery or DLQ, so a
  permanently malformed message redelivers forever.~~ See S13 above; this was the same finding.
- **RESOLVED (P2-5, same body of work)**: ~~`handleBatchIngest` has no batch-size cap and no request body
  limit, and returns `202` even when every item fails.~~ `apps/ingestor-go/main.go` now caps batches at
  500 items (`maxBatchSize`), enforces `http.MaxBytesReader` on both single (1 MiB) and batch (5 MiB)
  bodies with a `413` on overflow, and the batch response now reports `{ingested, failed, errors[]}` with
  a `400` (not `202`) when every item failed — a caller can now tell total failure from partial/full
  success by status code alone. Verified: `go build ./...` passes with this code present;
  `apps/ingestor-go/service/batch_test.go` (new) exercises it.
- **RESOLVED, appears fixed incidentally**: ~~`docker-compose.yml` maps NATS twice (`4222:4222` and
  `4223:4222`).~~ Current `docker-compose.yml` maps only `4222:4222` and `8222:8222` for the `nats`
  service — no double-mapping found. Not confirmed which change removed this; noting it as no longer
  reproducible rather than asserting a specific fix commit.
- `packages/db-migrations/cmd/migrate/main.go` `sanitizeDSN` — not re-checked this pass; presumed unchanged
  since `packages/db-migrations/cmd/` is not in the staged diff.
- `apps/processor-go/event/event.go` imports the deprecated `github.com/golang/protobuf/proto`; the ingestor
  uses `google.golang.org/protobuf/proto` — not re-checked this pass, presumed unchanged.
- `apps/dashboard-web/src/lib/server/auth-config.ts:11`: `ALLOWED_EMAIL_DOMAIN = 'company.com'` hardcoded —
  not re-checked this pass (no dashboard files in the staged diff), presumed unchanged.
- `apps/dashboard-web/src/lib/rate-limit.ts` in-process `Map`, unbounded — not re-checked this pass, presumed
  unchanged.
- `apps/dashboard-web/src/lib/rbac.ts` role mismatch (`support`/`owner` both return `false`) — not
  re-checked this pass, presumed unchanged.
- `ProcessorService.VerifyAuditLogTable` writes the all-zero-UUID row on every boot — not re-checked this
  pass; the function is unchanged in the staged diff (only its caller area shifted).

---

## Other open items carried forward unresolved (not owned by any single S-number above)

- **DLQ has no consumer.** No drain, no replay, no alerting, no dashboard surface. D10 in
  `docs/memory/DECISIONS.md` records this as an explicit accepted gap alongside S13's fix.
- **`issue_activity.old_value` is left NULL.** Spec 006 pairs `old_value`/`new_value` to record a
  transition; the regression-activity insert (fixed under S5 above to use the right column at all) only
  ever populates `new_value`.
- **`scripts/db/init.sql`** is a THIRD hand-maintained schema, frozen at `1716508800`, still carrying the
  pre-S12 `CHECK (status IN ('open','resolved','ignored'))`. `tests/integration/setup_test.go` applies it —
  not re-verified this pass whether that still causes drift against the goose-managed schema.
- **Dashboard schema drift (P6-3)**: not re-checked this pass (no `apps/dashboard-web/` files in the staged
  diff) — as of the last check, `schema.ts` declared `issueActivity.metadata` (no such column, see S5) and
  `issueRelations` was missing `created_by_type`/`created_by`; `issues.ts:57` emitted `'status_change'`
  where the DB requires `'status_changed'`.
- **`event_id`** is emitted by the SDK and still has no server-side destination — not re-checked this pass
  whether `mapping.MapPayloadToEvent` or the proto gained a field for it; the contract test
  (`tests/contract/sdk_ingestor_test.go`) explicitly strips it before decode (`stripEventIDField`), which
  is itself evidence it is still not part of the wire contract.
- **Integration suite** — current pass/fail count not re-verified (running it risks the exact DB-corruption
  hazard documented under "How to verify" above; do not run it against a database you need afterward).

---

## Feature-status reality check

`specs/*/spec.md` and `WORKLOG.md` mark 006, 007 and 008 as **Completed**. Verified end-to-end status,
2026-07-29:

| Feature | Claimed | Verified |
|---|---|---|
| 001 error service | shipped | ingest path now accepts well-formed events (S3 resolved) and writes real rows (S12 resolved) — first time ever, confirmed live (`issues: 3`, `error_occurrences: 11` rows) |
| 006 issue lifecycle / regression | Completed | regression detection now reachable and proven firing (S5 resolved, contract-tested); dashboard build status not re-checked this pass (S2, last verified 2026-07-28) |
| 007 Go client SDK | Completed | SDK↔ingestor contract now proven end-to-end by `tests/contract/` (S4/S16 resolved); SDK now surfaces send failures via `OnError` instead of discarding them silently |
| 008 API key management | Completed | key issuance/scoping code path exercised and correct (S6 resolved, tenant isolation code-verified); **revocation and expiry remain broken** (S7, open) — a key that should be dead is not; the `apps/dashboard-web` mock-fixture concern from the prior pass was not re-checked this pass (no dashboard files in the staged diff) |
| alerting (todo 04) | packages exist | still never invoked (S8, open, unchanged) |
| tenant isolation / rate limiting | — | request scoping fixed (S6); rate limiting itself is still non-atomic and can still fail open on a Redis outage after startup (S10, open); it was also completely, silently disabled for the middle portion of this same body of work by a self-inflicted regression (R1) before being caught and fixed |

The real query layer for 008 (`src/lib/db/queries/apikeys.ts`) is written and reasonable for creation;
`revokeApiKey`/`rotateApiKey` are the specific broken paths (S7). Treat "Completed" in `specs/` as "the
tasks file was checked off", not as "verified working" — that gap is exactly what this file exists to
close, one command at a time.

---

## Manual (user-reported) issues — M1 core CRUD VERIFIED 2026-08-11 (branch `claude/fervent-kalam-27b223`, pre-merge)

Phase M1 of `docs/plans/MANUAL_ISSUES_DESIGN.md` (agreed design, decision register in its §0):
migration `1722600000_add_manual_issue_reports_and_comments.sql`, `queries/reports.ts`,
`server/report-access.ts`, `/[orgSlug]/reports` routes, claim/move/activity APIs, Markdown +
IssueTimeline components, strict-separation filters on the error-issue list/search paths.

Proved by running, 2026-08-11:
- Migration replayed twice + down/up against a **disposable** postgres (podman, port 15433) — clean;
  `cd packages/db-migrations && go test ./...` green. Compose `migrate` applied it on top:
  `goose: successfully migrated database to version: 1722600000`.
- `pnpm build && pnpm check && pnpm test --sequence.shuffle` — 0 errors/warnings, 279 tests passed.
- `go build ./... && go vet ./...`, `go test ./tests/unit/...` — 308 passed.
- Full stack `docker compose up -d --build --force-recreate` (with `DASHBOARD_HOST_PORT=13000
  INGESTOR_HOST_PORT=18080` because foreign processes owned 8080/3000) then
  `SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/ -count=1` — **76 passed, 0 skipped**, no regression
  from the strict-separation filters.
- M1 flow proof: `apps/dashboard-web/src/lib/db/queries/reports.e2e-flow.integration.test.ts` runs
  create(→Triage) → list → claim → second-claim **conflict** → move → activity ordering against a real
  migrated disposable Postgres (hard-required via `M1_FLOW_INTEGRATION_REQUIRED=1`, same pattern as
  `SCHEMA_DRIFT_REQUIRED`). This test caught a real bug no mock test could: the Triage placeholder
  API key was 70 chars against `projects.api_key varchar(64)`, so **every first-use Triage
  provisioning threw** — fixed red-first (`randomBytes(24)`).

### M2 attachments VERIFIED 2026-08-11 (same branch)

MinIO in compose (+ `minio-init` one-shot, bucket `sentinel-attachments`), migration `1722700000`
(attachments table, single-parent CHECK, reaper partial index — replayed 2×+down/up on disposable
Postgres AND cross-ledger via `up -target=processor`), `$lib/server/storage.ts` (@aws-sdk/client-s3,
forcePathStyle), magic-byte sniffer (19 tests; ZIP-claiming-image/png rejected), `POST /api/uploads`
(25 MB cap pre+post parse, viewer-inclusive membership check), `GET /api/attachments/[id]`
(draft=uploader-only; linked=per-issue-type authz; comment-linked fails closed 501 until M3), orphan
reaper on the retention cron + opportunistic per-org sweep, link-on-create inside the
`createManualIssue` transaction.

Proved by running, 2026-08-11: dashboard gates green (315 tests incl. env-gated integration, shuffled;
`pnpm check` 0/0; `pnpm build` clean), db-migrations go test green, drift test 23 passed with
`SCHEMA_DRIFT_REQUIRED=1`, full-stack e2e **76 passed / 0 skipped**, and
`reports.attachments.flow.integration.test.ts` (`M2_ATTACHMENTS_INTEGRATION_REQUIRED=1`) against the
real compose MinIO+Postgres: upload → link-on-create → byte-identical download → reaper removes only
the orphan (object AND row) → mislabeled file rejected. Root-module Go gates deliberately not run this
phase (no root Go code touched; user instruction).

Two operational gotchas found: `scripts/wait-healthy.sh` has a hardcoded `ONESHOT_SERVICES` list —
any new one-shot compose service must be added there or a legitimate `Exited (0)` reads as failure
(fixed for `minio-init`); and under vitest's `resolve.conditions: ['browser']`, `@aws-sdk/client-s3`
loads its browser build whose stream collector needs `Blob.prototype.arrayBuffer` (absent in jsdom) —
the integration test swaps in Node's `Blob` before import; production is unaffected.

### M3 threads VERIFIED 2026-08-11 (same branch)

`queries/comments.ts` (createComment with reply-to-reply→root resolution, D18 single transaction,
comment-attachment claiming, `commented` activity, Q11 groundwork: any USER comment clears
`issues.waiting_on` + writes `question_answered`), `comment-access.ts` per-issue-type dispatch,
`GET/POST /api/issues/[issueId]/comments` (`?after=` polling, DB-clock cutoff via `select now()` —
app-clock vs DB-clock skew was a real bug caught while authoring the proof), `PATCH/DELETE` with
author/moderator rules, the M2 501 flipped (comment-linked attachments resolve via parent issue),
`CommentThread.svelte` (Slack-like, one-level replies, agent badge, visibility-paused ~10 s polling)
mounted on BOTH report and service-issue detail pages, comment counts in the reports list. No new
migration — `issue_comments` from 1722600000 went live.

Proved by running, 2026-08-11: dashboard gates green (`pnpm build`; `check` 1673 files 0/0;
`test --sequence.shuffle` 344 passed), db-migrations go test green, full-stack e2e **76 passed /
0 skipped**, and `comments.threads.flow.integration.test.ts` (`M3_THREADS_INTEGRATION_REQUIRED=1`,
real compose Postgres+MinIO): report → root comment w/ attachment (downloadable via flipped path) →
reply → reply-to-reply same-parent → SQL-set `waiting_on` cleared by human comment +
`question_answered` → after-filter exact → delete cascades replies + attachment row AND MinIO object.

### M4 notifications VERIFIED 2026-08-11 (same branch)

Migration `1722800000` (issue_subscriptions + notifications, replayed + down/up on disposable
Postgres, drift test 25/25 under `SCHEMA_DRIFT_REQUIRED=1`), `notify.ts` fan-out called INSIDE the
existing mutation transactions (D18) with actor exclusion and agent-subscriber skip, auto-subscribe
on create(reporter)/claim(claimant)/comment(participant)/assign, Q7 email policy via
`sendIssueNotificationEmail` (15-min per-(user,issue) throttle implemented as a query over emailed
notification attempts — no extra log table; `question_asked` bypasses per Q11; `linked`/
`progress_update` never email), `GET/PATCH /api/notifications`, subscription toggle API + UI on both
detail pages, NotificationBell with visibility-aware polling (extracted shared `visible-poll.ts`),
`/[orgSlug]/notifications` page. Design deviations, documented in code: claim-release reuses kind
`claimed` with `payload.released` (no `claim_released` in the CHECK) and move sends no notification
(no `moved` kind) — revisit in M5+ if needed.

Proved by running, 2026-08-11: dashboard gates green (390 tests shuffled, check 1695 files 0/0,
build), db-migrations go test green, full-stack e2e **76 passed / 0 skipped** (12 containers), and
`notifications.flow.integration.test.ts` (`M4_NOTIFICATIONS_INTEGRATION_REQUIRED=1`, real compose
Postgres): auto-subscribe → claimed notification with no self-notify → commented → mark-read drops
unread by exactly 1 → resolved → unsubscribe silences further events; email composition proven over
the `smtp://debug` jsonTransport path.

### M5 agents VERIFIED 2026-08-12 (same branch)

Migration `1722900000` (agents table; `project_api_keys.agent_id` + catalog-guarded scope-CHECK swap
adding `'agent'`; replayed + down/up, drift test 26/26). `apps/ingestor-go/auth/apikey.go` needed NO
change — its scope allowlist (`ingest`/`admin` only) already rejects agent keys; proven live: agent
key against `/ingest` → 403. `agent-auth.ts` (Bearer sha256 lookup, scope/status/expiry + agent
active; tenant scope from the credential only — B7), `agent-issue-scope.ts` (cross-org → 404),
`/api/agent/*` work-loop (list both issue types / atomic claim+release with real actorType — M1's
releaseClaim had hardcoded 'user', fixed / progress / blocking questions setting `waiting_on` +
`question_asked` fan-out in one transaction / comments+after-polling / status with
resolved_by_type='agent' / relations / uploads via shared `upload-core.ts`), every mutation writing
`audit_logs` (`agent.issue.*`, incl. key prefix) alongside `issue_activity`. Management: `agents`
CRUD API + `/[orgSlug]/settings/agents` UI gated by new `manage_agents` RBAC permission
(owner/admin only); agent keys use prefix `sent_agent_`, project_id forced NULL; the hardcoded
"AutoFix Agent" mock in IssueAssigneePicker is replaced by real org agents.

Proved by running, 2026-08-11/12: dashboard gates green (441 tests shuffled, check 1732 files 0/0,
build), db-migrations go test green, full-stack e2e **76 passed / 0 failed** plus new
`tests/e2e/agent_work_loop_test.go` (`M5_AGENT_INTEGRATION_REQUIRED=1`, real HTTP against the
compose dashboard): org-isolation on list, concurrent double-claim → exactly one 200 + one 409,
progress activity, blocking question → `waiting_on` set + reporter notified `question_asked`, human
reply via a real Auth.js session cookie clears it, agent polls the answer via `?after=`, resolve as
agent, audit rows counted and matched by action, ingest rejection 403. Known minor: the shared e2e
harness cleanup deletes users before `manual_issue_reports` (tolerated FK log line, orphan rows in
dev/CI DB) — pre-existing gap, tracked separately.

### PR #13 review remediation (R1–R20) VERIFIED 2026-08-12 (same branch)

A three-axis review (standards / spec / defect-hunt) of `main...feat/manual-issues` produced 20
findings, all fixed red-first — register and status in `docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md`.
Highest-impact: R1 ex-members kept receiving notification rows+emails (fan-out now re-checks org
membership in-transaction AND member removal deletes their subscriptions); R2 Triage inbox
double-create race (partial unique index `projects(org) WHERE is_inbox` + onConflict re-select —
which surfaced a real drizzle-orm 0.30 bug: `targetWhere` silently dropped, only deprecated `where`
emits the partial-index predicate); R3 unicode filename 500s (RFC 5987 encoding); R5 email throttle
starvation (`notifications.emailed_at` stamps actual sends); R6 retention now deletes MinIO objects;
R11 report-body edit/delete per §9; R13 processor upserts scoped `issue_type='system_error'`; R19
agent-key rate limiting live. Migration `1723000000` (idempotent, replayed, drift 26/26).

Proved by running, 2026-08-12: dashboard gates green (491 tests shuffled, check 0/0, build),
db-migrations + processor-store package go tests green, full-stack e2e **76 passed / 0 failed**, and
`reports.pr13-remediation.flow.integration.test.ts` (real compose Postgres+MinIO) proving R1/R2/R6/
R11 end-to-end. Process note for posterity: an implementor agent falsely reported R13 done (the
package did not compile); the validation layer caught it by running the build — the repo's
characteristic optimistic-status failure reproduced inside an AI workflow, and the countermeasure
(independent verification that actually executes commands) worked.

Not yet built (by design, later phases): threads UI (M3 — DONE, see above — the `issue_comments`
table shipped early in the M1 migration, deliberately inert), notifications (M4), agent API (M5).

## M6 (partial) — presigned large uploads + toolbar Markdown editor — DONE 2026-08-12

Two of the deferred M6 backlog items (`docs/plans/M6_PRESIGNED_UPLOADS_AND_TOOLBAR_PLAN.md`) shipped
on branch `feat/manual-issues`.

**Presigned large uploads (§4/Q4).** Files > 25 MB (up to `MAX_PRESIGNED_UPLOAD_BYTES` = 500 MB) now
bypass the proxy: `POST /api/uploads/presign` mints a direct-to-bucket PUT URL and a `pending`
`attachments` row; the client PUTs the bytes straight to MinIO; `POST /api/uploads/[id]/finalize`
does a **full GetObject read of the first `SNIFF_BYTES` (stream stopped early — NOT a ranged GET, see
below), runs the exact same `sniffContentType`/`resolveContentType` allowlist as the proxy path,
deletes the object + row on 413 (oversize) or 415 (unrecognized bytes), and only otherwise flips the
row to `ready` with the sniffed content-type and real object size. **Load-bearing invariant:**
`claimDraftAttachmentsOnto` refuses any row whose `status != 'ready'` in both its pre-check and its
conditional UPDATE — a presigned-but-unvalidated object can never be linked to an issue/comment.
Migration `1723100000_add_attachment_status.sql` (idempotent; `status` column + catalog-guarded
CHECK); `schema.ts` mirrored, drift 26/26.

Three real-MinIO gotchas fixed while proving this, all invisible to the mocked unit tests and only
caught by the committed integration flow (`reports.presign.flow.integration.test.ts`):
- **Do not sign Content-Type into the presigned PUT.** Signing it (and the browser/`fetch` echoing
  any Content-Type) makes MinIO reject the PUT with *"headers present … which were not signed"*.
  `createPresignedPutUrl` signs only host+key; the client PUTs a **type-stripped Blob**
  (`new Blob([file])`, empty type ⇒ no Content-Type header). The stored object type is irrelevant —
  finalize overwrites the DB `content_type` from the sniff and the download route serves *that*.
- **AWS SDK v3 (≥ ~3.729) default integrity checksums** pollute presigned URLs the same way; the S3
  client sets `requestChecksumCalculation`/`responseChecksumValidation: 'WHEN_REQUIRED'`.
- **The SDK *browser* build mis-signs a ranged GetObject against MinIO** (plain GET is fine). vitest
  forces that browser build (the `'browser'` resolve condition, same root as the documented Node-Blob
  swap). So `getObjectRangeBytes` does a full GetObject and breaks out of the stream after
  `SNIFF_BYTES`, then `destroy()`s it — portable across both builds, only the first chunk crosses the
  wire regardless of object size.
- **Presigned URLs must be signed against a BROWSER-reachable endpoint, not the server-side one.**
  The dashboard container's `S3_ENDPOINT` is the in-cluster `http://minio:9000` (correct for the
  upload proxy / download / finalize sniff, all server-side), but a presigned PUT URL is handed to
  the browser, which cannot resolve `minio`. `storage.ts` now signs presign URLs with a separate
  client pinned to `S3_PUBLIC_ENDPOINT` (falls back to `S3_ENDPOINT` for the dev/host case where both
  are localhost); compose sets it to `http://localhost:${MINIO_HOST_PORT:-9000}`. The SigV4 host is
  signed, so this MUST be the signing client's endpoint — rewriting the host post-signing breaks the
  signature. The M6 integration test masked this by running on the host where both endpoints are
  localhost; `storage.presign-endpoint.test.ts` now pins the internal≠public split (network-free).
- **Observability**: the dashboard has no span-creation API — obs = structured `log.*` (JSON with
  `trace_id`/`span_id` auto-injected via `runWithTraceContext`), no otel wrapping of S3 calls (the S3
  SDK is not otel-instrumented on the dashboard side, consistent with the rest). `createPresignedAttachment`/
  `finalizePresignedAttachment` emit `uploads.presign_created` / `uploads.finalize_succeeded` /
  `uploads.finalize_rejected` (with `reason: oversize|disallowed_content`), matching sibling event
  naming.

**Toolbar Markdown editor (§3/Q3).** A dependency-free Markdown-syntax toolbar (bold/italic/code/
strikethrough/heading/quote/ul/ol/link) over the existing textareas — deliberately NOT a Tiptap
WYSIWYG rewrite, honoring the design's binding "stays Markdown / no data migration" constraint. Pure
transform in `$lib/markdown-toolbar.ts` (exhaustively unit-tested), thin `MarkdownToolbar.svelte`
wired above the body textarea in the comment composer, new-report form, and the report edit form.

Proved by running, 2026-08-12: dashboard gates green (`pnpm build`; `pnpm check` 0/0;
`pnpm test --sequence.shuffle` **531 passed**), drift 26/26 against the real migrated Postgres, and
`reports.presign.flow.integration.test.ts` (`M6_PRESIGN_INTEGRATION_REQUIRED=1`, real compose
Postgres+MinIO) proving presign→direct PUT→finalize→ready→claim, the pending-cannot-link gate, and
415+cleanup on unrecognized bytes; full-stack e2e held at **76 passed / 0 skipped**.

---

## Agent-native layer (N1–N5) VERIFIED 2026-08-14 (branch `feat/agent-native`, pre-merge)

Provider-agnostic continuous-automation layer on top of the M5 `/api/agent/*` surface
(plan: `~/.claude/plans/calm-cooking-waterfall.md`; commits `bb94cce`..`c7e16ff`):

- **N1**: `issue_activity.seq` (bigint `GENERATED ALWAYS AS IDENTITY`, migration `1723200000`) +
  `agent_webhooks` (migration `1723300000`, **secret stored plaintext by design** — the server must
  SIGN outbound HMAC, unlike API keys which only verify hashes); `GET /api/agent/events`
  (org-scoped seq cursor, **2s created_at lag guard** against identity commit-order inversion,
  at-least-once/dedupe-by-seq contract); agent reads: issue detail (report | latestOccurrence
  branch), occurrences, projects.
- **N2**: `$lib/server/agent-ops.ts` 7-op registry extracted verbatim from the single routes (now
  thin `agentOpRoute()` wrappers); `POST /api/agent/batch` — ≤20 sequential ops, per-op status in a
  200 envelope, `stopOnError`+skipped reporting, NO outer transaction (deliberate: identical to N
  sequential calls), one rate-limit charge per batch.
- **N3**: webhook registration CRUD (RBAC `manage_agents`, secret shown once, SSRF URL validator —
  bypass attempts `0177.0.0.1`/`127.1`/`::ffff:127.0.0.1`/hex/decimal all rejected) + processor-go
  outbox dispatcher (`apps/processor-go/webhooks/`): payload field-for-field identical to the events
  feed (B5-checked), `X-Sentinel-Signature: t=<unix>,v1=hmac-sha256(secret, t + "." + body)`
  (byte-identical to an external oracle), CAS cursor advance (`WHERE last_delivered_seq=$old`),
  auto-`failed` after 20 consecutive failures, **gated OFF** (`WEBHOOK_DISPATCH_ENABLED=false`).
- **N4**: `tools/sentinel-cli` — independent Go module (stdlib-only, NOT in go.work, own CI job with
  `GOWORK=off`); commands 1:1 with the API; `events --follow` NDJSON with persisted cursor; exit
  code 5 = claim 409. Contract cross-checked against every route file: zero breaking mismatches
  (4 documented server-wins notes in code).
- **N5**: `docs/agents/SENTINEL_AGENT_GUIDE.md` (canonical, every claim verified against handlers;
  worked HMAC example recomputed independently), `openapi.agent.yaml`,
  `.agents/skills/sentinel-agent/SKILL.md` (canonical provider-neutral skill) + `.claude` shim,
  runnable examples.
- **N6** (`6d8e2ae`, verified 2026-08-14): `openapi.agent.yaml` is now **GENERATED** from zod
  schemas + a route registry in `src/lib/server/agent-api-spec/` (`pnpm openapi:agent`,
  deterministic — double-generate byte-identical). Never hand-edit the YAML. Three gates run inside
  the normal `pnpm test` (already CI-enforced): drift (in-memory generate vs committed YAML),
  completeness (walks `+server.ts` exports, exact bidirectional path+method equality with the
  registry), contract (real handlers' responses parsed under `.strict()` schemas). All three
  red-proven by mutation (schema edit / fake route / deleted schema field), then green. Adding or
  changing an agent route now FAILS CI until `agent-api-spec/` and the regenerated YAML follow.

Proved by running, 2026-08-14 (each phase adversarially validated by re-running gates, not by
reading): dashboard `pnpm build`/`check`/`test --sequence.shuffle` green throughout (637 passed at
N3; the sole recurring failure is the **pre-existing** `notifications.flow.integration.test.ts` 5s
timeout under 78-file parallel load — proven pre-existing by failing identically with the change
stashed and passing in isolation); root `go build`/`vet` clean, `go test ./apps/processor-go/...
./tests/unit/...` 324 passed; migrations replayed 3× across ledgers on disposable postgres:15;
guard-deletion checks red-then-green on the B7/cursor/SSRF tests; full-stack
`SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/ -count=1` — **79 passed / 0 failed / 0 skipped**
(76 prior + U37 events-ordering/org-isolation/batch, which create all their own state and assert a
second org's credential sees nothing).

---

## Agent-automation remediation (N7) VERIFIED 2026-08-14

Fixes for the 15 confirmed + 2 refuted-but-real findings in
`docs/audits/AGENT_AUTOMATION_AUDIT_2026-08-14.md` (branch `feat/agent-remediation`, commits
`ebe6be8`..`faf6f34`, pre-merge). Full finding→outcome mapping is now a table at the top of that
audit file; this entry summarizes what actually runs.

- **N7a** (`eafb88c`, A01/A06/R2): processor `StoreEvent` writes a `created` issue_activity row on
  race-exact new-issue detection (`RETURNING xmax=0`) and a throttled `occurrence_burst` row on
  repeat occurrences (1/issue/`OCCURRENCE_EVENT_MIN_INTERVAL_SECONDS`, default 1h). Migration
  `1723400000` widens the `issue_activity` event-type CHECK (idempotent, replay-proven 3x).
  New-service-error discovery via the events feed — the audit's near-blocker — now works; it did
  not before this phase.
- **N7b** (`fd752ee`, A02): `GET /api/agent/issues` gains `since`/`sort=firstSeen|lastSeen`/`limit`
  (clamped `[1,200]`) + opaque keyset cursor on `(sortColumn, id)`. Omitting the new params keeps
  the old unbounded `lastSeen`-desc behavior for compatibility.
- **N7c** (`fd752ee`, A03/A04): `issues.claimed_at` + a stale-claim reaper in the retention cron
  (`CLAIM_STALE_HOURS`, default 24, agent claims only, resets on any claimant activity); retention's
  occurrence-less-deletion path now requires resolved/ignored AND unclaimed, so a claimed or
  in-progress manual issue can no longer be silently deleted mid-triage.
- **N7d** (`3661d96`, A05/A12/A13): exact-retry no-op on `updateIssueStatus` (`changed:false`, no
  duplicate activity/notification); natural-key dedupe (2min window) on `createComment`/
  `recordAgentProgress`, blocking questions excluded by design; idempotent `releaseClaim` (releasing
  an already-unclaimed issue is 200, not 409); `caused_by` reverse-pair insert rejected 409
  (2-cycle guard only, matches `duplicate_of`'s existing behavior).
- **N7e** (`faf6f34`, A07–A09): `comments.edit`/`comments.delete` ops + route, ownership-gated
  (403 wrong author, 404 cross-issue); `issues.report.severity` op (`user_report` only, 400 on
  `system_error`); `claimedAt` exposed in agent list/detail + dashboard "agent working" badge
  (UI-only, no new status — deliberate).
- **N7f** (`faf6f34`, R1/A10/A11/A14/A15): `GET /api/agent/self` (identity + key metadata);
  `POST /api/agent/key/rotate` (grace-window rotation, `AGENT_KEY_ROTATION_GRACE_HOURS`, revoked/
  expired keys cannot rotate); `Retry-After` on 429 computed from the limiter's actual `resetAt`;
  claim-conflict 409 bodies enriched with `claimedBy`/`claimedAt`; `DEPLOYMENT.md`
  agent-provisioning runbook (A14 stays a documented one-time human bootstrap step, not a scripted
  path); CLI `upload <file> --issue <id> [--comment <text>]` one-shot.

Proved by running, 2026-08-14: `rtk go build ./... && rtk go vet ./...` clean;
`rtk go test ./tests/unit/...` — 308 passed. Each phase's own commit message additionally records
its adversarial validation (guard-deletion red-proofs per phase, migration replay 3x, dedupe
boundary tests at t+119/t+121, revoked-key-cannot-rotate proof, batch enum exclusion of non-issue
ops) — see the individual commit bodies for the full per-phase gate list; not independently
re-run here beyond the build/vet/unit-test slice above.

**Full-stack e2e re-proven 2026-08-15** against the rebuilt compose stack (N7a processor image +
migrations 1723400000/1723500000 applied): `SENTINEL_E2E=1 M5_AGENT_INTEGRATION_REQUIRED=1
INGESTOR_URL=http://localhost:18080 DASHBOARD_URL=http://localhost:13000 go test -tags=e2e
./tests/e2e/ -count=1` — **81 passed / 0 failed / 0 skipped** (incl. new U38 discovery and U39
lifecycle). TWO OPERATIONAL GOTCHAS discovered during this proof, both worth knowing:
1. **`M5_AGENT_INTEGRATION_REQUIRED=1` is mandatory for the agent e2e suite** — without it every
   agent test (U37–U39, M5 work-loop) SKIPS silently and the run still reports ok. A "green" e2e
   run without that env proves nothing about the agent surface.
2. **`error_class` values containing digit runs are normalized to `<NUMERIC_ID>`** by the
   processor (fingerprint stability) — a test asserting round-trip equality must use an
   alphabetic unique suffix (`alphaSuffix()` in tests/e2e/agent_n7_test.go), or it fails against
   the masked value. This bit U38 on its first full-stack run.
Also: on a machine where the compose project was created from a different worktree, `docker
compose` from this worktree hangs forever waiting on `service_completed_successfully` init
containers that only exist under the ORIGINAL project name — pass
`COMPOSE_PROJECT_NAME=<original>` to join the existing project instead.

`docs/agents/SENTINEL_AGENT_GUIDE.md` and
`.agents/skills/sentinel-agent/SKILL.md` were re-read end-to-end against current routes/CLI on
2026-08-14 (N7g) and found already consistent — both were kept current as part of N7e/N7f
themselves, so no correction was needed at close-out.

---

## N10 part 1 — per-project agent settings + repository connections VERIFIED 2026-08-18

Branch `feat/agent-project-settings` (commit `00d2394`, pre-merge), worktree
`gallant-roentgen-186e66`. Server-side policy surface the N8 `sentinel-worker` consumes
(AGENT_WORKER_PLAN.md rev 4 §4.5; risk acceptance recorded as DECISIONS.md D23).

- **Schema**: migration `1723900000` (sibling, repo credentials) → `1724000000_add_project_agent_settings.sql` (renumbered post-rebase: the sibling N10 part 2 branch had claimed 1723900000 concurrently — the exact silent-skip goose version collision):
  `project_agent_settings` (`fix_enabled` default false, `max_prs_per_day` CHECK > 0) and
  `project_repo_connections` (provider CHECK github|bitbucket, owner/repo/default_branch/test_cmd
  NOT NULL, agent_cmd/clone_depth nullable; **PK on project_id enforces one connection per
  project v1**). Idempotency: CREATE TABLE IF NOT EXISTS + pg_constraint DO-block guards.
  **Replay-proven independently**: disposable `postgres:16`, `go run ./cmd/migrate up -target
  {processor,ingestor,dashboard}` × 2 each — all six runs clean, both tables present via
  `to_regclass`. NO credentials columns — the encrypted git-credentials store is a sibling task.
- **Dashboard**: `[orgSlug]/projects/[projectId]/settings` "Agent automation" section +
  `/api/organizations/[orgId]/projects/[projectId]/{agent-settings,repo-connection}` (GET/PUT[/DELETE]).
  `manage_agents` RBAC on load AND every action (same `requireOrgMembership` + `hasPermission`
  mechanism as the agents pages); every mutation writes `audit_logs`
  (`agent_settings.updated`, `agent_repo_connection.created|updated|deleted`) with before/after.
  Shared provider constant in `$lib/constants/agent-repo.ts` (B12: never exported from routes).
- **Agent API**: `GET /api/agent/projects` rows carry
  `agentSettings: {fixEnabled, maxPrsPerDay, repo|null}`, defaults
  `{false, null, null}` when no rows exist; repo includes full `testCmd`/`agentCmd` deliberately
  (D23). Batch read (no N+1); B7 scope from `AgentAuthContext` only. OpenAPI regenerated via
  `pnpm openapi:agent`; drift/completeness/contract gates green; SENTINEL_AGENT_GUIDE.md updated.
- **Proofs**: `pnpm build` green; `pnpm check` 1870 files / 0 / 0; `pnpm test --sequence.shuffle`
  826 passed / 7 skipped / 2 failed — the 2 are the pre-existing shared-Postgres concurrency
  timeouts (`retention.claims.integration`, `notifications.flow.integration`), 8/8 in isolation.
  Full e2e re-proven against the live compose stack with the dashboard image REBUILT from this
  branch and migration applied by the `migrate` one-shot:
  `SENTINEL_E2E=1 M5_AGENT_INTEGRATION_REQUIRED=1 INGESTOR_URL=http://localhost:18080
  DASHBOARD_URL=http://localhost:13000 go test -tags=e2e ./tests/e2e/ -count=1` → **ok, 0 fail**.
  Delivery per the standing mandate: Sonnet implementors + Opus adversarial validators (schema
  track needed one fix round: decoration write-path tests, unscoped-delete blind spot,
  varchar/TEXT drizzle drift, dangling D-ref — all re-proven by mutation).
- **Operational gotcha (repeat of the 2026-08-15 one, sharper)**: cross-worktree
  `COMPOSE_PROJECT_NAME=<original> docker compose up -d --build --force-recreate migrate dashboard`
  under podman **built the image, ran migrate, then hung forever after creating (not starting) the
  new dashboard container — and REMOVED ingestor/processor/nats-init without recreating them**.
  Recovery: kill the compose process, `docker start sentinel-dashboard`, then
  `COMPOSE_PROJECT_NAME=<original> docker compose up -d --no-build nats-init ingestor processor
  dlq-drainer` (plain `up -d --no-build` does not hang).

---

## Keep here

- Observed runtime/build behavior with the command that produced it.
- Divergences between documented intent and executed code.

## Never store here

- Fix plans (those belong in `specs/<feature>/` or `docs/plans/`).
- Findings not reproduced by running something.

## N8a — sentinel-worker skeleton (2026-08-18, branch feat/agent-worker, NOT yet merged)

What runs, with the command that proved it:
- `cd tools/sentinel-worker && GOWORK=off go build ./... && GOWORK=off go vet ./... && test -z "$(gofmt -l .)" && GOWORK=off go test ./... -count=1 -race` → all packages ok.
- Dashboard gates after the claim_released `previousAssignee` change (queries/issues.ts):
  `pnpm build` done, `pnpm check` 1884 files / 0 errors, `pnpm test -- --sequence.shuffle`
  864 passed / 11 skipped.
- Live boot against the compose stack (dashboard :13000): healthz 200; readyz 503 with honest
  reason; recovery scan runs BEFORE identity resolution; bogus key → 401 retried on the
  backoff ladder without crash; SIGTERM → clean exit 0 (dispatcher ctx-drain).
NOT yet verified (deliberate, per plan §9): real-credential dry-run (U40, N8d e2e); guard/
keyguard/batch-writers/runner-circuits are tested-but-unwired seams — see the plan's §9
"N8a unwired seams" note before trusting their green suites (B3).

## N8b — llm package (2026-08-18, branch feat/agent-worker, NOT yet merged)

Proved: `cd tools/sentinel-worker && GOWORK=off go build ./... && GOWORK=off go vet ./... &&
test -z "$(gofmt -l .)" && GOWORK=off go test ./... -count=1 -race` → all packages ok (llm
suite includes the 11-scenario × 3-adapter parity table over real wire fixtures). NOT verified:
any call against a real provider (unit/httptest only, by design); llm is tested-but-unwired
until N8d (documented in plan §9 seams note). Gemini usage parity depends on summing
thoughtsTokenCount — asserted by golden; re-verify if the Gemini wire schema changes.

## N8c — gitprovider/repoctx/guard/settings (2026-08-18, branch feat/agent-worker, NOT merged)

Proved: full worker gate `GOWORK=off go build/vet/gofmt/test ./... -race` green (11 packages).
Security controls proven against EXPLOIT REPRODUCTIONS (red-first tests reproduce the attack):
askpass no-leak under url.insteadOf (host:port pin) + credential.helper store (neutralized) +
argv/.git/config inspection; repoctx confinement (traversal, committed-symlink escape, .git alias,
absolute path, -ref injection); guard §4.6 gate (filler-period table 2..12 × 5 fillers, short
secret, alnum-chunked secret, prose-padded dilution, secret-value + slog/%v/%#v redaction).
settings credentials memory-only (never journaled/state-dir/snapshot). NOT verified: any real git
host or real credential (httptest + local bare fixtures only); repoctx/guard enforce nothing on a
live decision until N8d wires the Advisor; CreatePR/PRStatus not exercised until N8f. Residual
(documented §4.6): paraphrase exfiltration below the verbatim threshold — backstopped by repoctx
confinement + repo-scoped tokens + PR review.

## N8d — TRIAGE + FOLLOW-UP Advisors (2026-08-19, branch feat/agent-worker, NOT merged)

Proved: full worker gate green incl -race + gofmt (12 packages). E2E RAN (not skipped) against
the live compose stack: `SENTINEL_E2E=1 INGESTOR_URL=http://localhost:18080
DASHBOARD_URL=http://localhost:13000 go test -tags=e2e ./tests/e2e/ -run
'TestU40...|TestU41...' -v` → both PASS (U40 7.87s, U41 4.26s), verified via raw `--- PASS`
lines. The worker now genuinely decides: triages a real ingested error, gates the summary through
guard.Check, claims via C1, journals the decision, replays on kill -9 without duplicating. WORKER_
EXECUTE=true now a supported mode; dry-run still sends nothing (asserted). guard output-gate and
WrapUntrusted are LIVE on the decision path (mutation-proved). NOT verified: real LLM provider
(fake in-process Advisor only, by design); FIX enqueue is a seam — no FIX job runs until N8f;
keyguard rotation not wired until N8e.
