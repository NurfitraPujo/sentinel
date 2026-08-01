# Verified State of the Codebase

Last verified: **2026-07-29**. HEAD at verification time is `cd84d17` (P0+P1, "add CI, make the local
stack real, restore build and tests") on branch `chore/p0-p1-green-tree`, with **P2/P2b staged and
uncommitted on top** — 36 files, +2501/-254. Baseline before any of this recovery work was `ad2f967`.
Verified by running builds, `go vet`, `go test` (root module and `-tags=contract`), the independent
`packages/sdk-go` module's own test suite, `pnpm check`/`pnpm test`, and live probes against the running
compose stack (`podman exec sentinel-postgres psql -U sentinel -d sentinel`, `curl` against the ingestor).

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

---

## How to verify (copy-paste)

```bash
rtk go build ./...                                    # root module: PASSES
rtk go vet ./...                                      # PASSES
rtk go test ./tests/unit/... -count=1                  # PASSES, 241 assertions (was 251 at S1's fix; masker/ratelimit tests added since)
rtk go test -tags=contract ./tests/contract/... -v -count=1   # PASSES, 4/4 (proves S3/S4/S5/S11/S16 against REAL sdk-go + REAL ingestor decode/validate path)
cd packages/sdk-go && rtk go test ./... -count=1       # PASSES, 4 packages (SDK is an independent module — not covered by the root-module commands above)
cd apps/dashboard-web && rtk pnpm exec vite build      # PASSES (S2)
cd apps/dashboard-web && rtk pnpm check                # 707 files, 0 errors (S2)
cd apps/dashboard-web && rtk pnpm test                 # PASSES, 5 files / 19 tests (S2)
```

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

## Keep here

- Observed runtime/build behavior with the command that produced it.
- Divergences between documented intent and executed code.

## Never store here

- Fix plans (those belong in `specs/<feature>/` or `docs/plans/`).
- Findings not reproduced by running something.
