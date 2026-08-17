# Claude Code Instructions

This repository is built to work with Spec Kit Memory Hub and Claude Code.

### Spec Kit

You MUST follow the memory-first workflow defined in [.specify/memory/workflow.md](file://.specify/memory/workflow.md) and proactively execute `/speckit.memory-md.prepare-context` before planning.

## Memory Source of Truth
- **Governance**: `.specify/memory/` (Constitution, Architecture, Workflow)
- **Durable**: `docs/memory/` and `docs/solutions/` (History, Decisions, Patterns, Solutions)
- **Active**: `specs/<feature>/` (Local context and synthesis)

A task is not fully complete until memory has been reviewed and systemic lessons are captured.

---

## Ground Truth (read before planning any change)

> [!IMPORTANT]
> `specs/*/spec.md` "Status: Completed" and `docs/memory/WORKLOG.md` milestones record **merge events, not
> verified behavior**. Several features marked Completed do not execute at runtime.
> **[docs/memory/VERIFIED_STATE.md](docs/memory/VERIFIED_STATE.md)** records what actually runs, with the
> command that proved it. Read it before trusting any feature-complete claim, and re-verify entries whose
> date predates the change you are making.
>
> **[docs/plans/E2E_RECOVERY_PLAN.md](docs/plans/E2E_RECOVERY_PLAN.md)** is the phased plan that fixes those
> findings, with a 32-row use-case matrix defining "working end-to-end". Work items are ID'd `P0-1`, `P2-3`,
> … — reference those IDs in commits.
>
> **[docs/plans/UI_PARITY_REMEDIATION_PLAN.md](docs/plans/UI_PARITY_REMEDIATION_PLAN.md)** is the same
> exercise for the dashboard (47 findings, `D01`–`D47`, all closed 2026-08-01). It exists because five
> reviewed, merged, test-passing dashboard features were audited and **three of them did not run at all**.
> **Beware the ID collision**: `D01`–`D47` there are *findings*; `D1`–`D18` in `DECISIONS.md` are
> *architecture decisions*. Always name the file when citing a `D` number.

### Repository shape

Go/SvelteKit monorepo for an error-tracking pipeline:
`sdk → ingestor-go (HTTP :8080) → NATS JetStream → processor-go → PostgreSQL`, with `dashboard-web`
(SvelteKit + Auth.js + Drizzle) reading the same database.

- `apps/ingestor-go` — auth, rate limit, validate, publish. The only externally exposed service.
- `apps/processor-go` — deserialize, normalize, mask, fingerprint, upsert issue + occurrence, index.
- `apps/dashboard-web` — UI and JSON API.
- `packages/shared-go` — pgx pool, NATS pub/sub, redis client, and `obs` (slog + OpenTelemetry).
  `obs.Bootstrap` registers the global trace propagator; see D15 and **B11** before touching it, because
  its failure mode is silence, not an error.
- `packages/proto` + `gen/` — the `ErrorEvent` contract (buf + protovalidate CEL).
- `packages/db-migrations` — goose migrations; **one flat directory** for all targets (see A1).
- `packages/sdk-go` — the public Go client.
- `tests/{unit,integration,load}` — root-module tests; integration tests use testcontainers ONLY when nothing answers `localhost:8080/health`; if the compose stack is up they run against the SHARED dev Postgres and can corrupt it. Set `FORCE_TESTCONTAINERS=1` to always isolate (`tests/integration/setup_test.go:62`).

**Three independent Go modules** (root, `packages/sdk-go`, `packages/db-migrations`), no `replace`
directives. As of P0-3, a committed `go.work` (`use . ./packages/sdk-go ./packages/db-migrations`) puts them
in workspace mode for local dev and cross-module contract tests (`tests/contract/`, tagged
`//go:build contract`), so the root module **can now import `packages/sdk-go`** locally. CI's `go-root` job
deliberately runs with `GOWORK=off` so it still exercises the root module exactly as a real `go get` would
see it — against `packages/db-migrations`'s **published pseudo-version**, not the local workspace copy. Local
edits under `packages/db-migrations/` remain invisible to that job until committed and the pseudo-version is
bumped. See A2 in `docs/memory/ARCHITECTURE.md` for the full rationale and the GOWORK divergence; this
constraint still invalidates test plans that assume `go-root` sees local, uncommitted `db-migrations` edits.

### Commands

```bash
rtk go build ./... && rtk go vet ./...          # root module — green
rtk go test ./tests/unit/...                    # green, 308 passed
cd packages/sdk-go && rtk go test ./...         # separate module — green
cd packages/db-migrations && rtk go test ./...  # separate module — green
docker compose up -d --build --force-recreate   # full stack incl. redis + migrate; plain `up -d` does NOT rebuild
./scripts/wait-healthy.sh                       # then this — blocks until every service reports healthy
cd apps/dashboard-web && pnpm build && pnpm check && pnpm test   # green: 1024 files/0 errors/0 warnings, 251 tests
                                                  # run ALL THREE. `pnpm build` is the ONLY one that enforces
                                                  # SvelteKit's route-export allowlist — check+test pass while
                                                  # build fails on it (B12). Add --sequence.shuffle to `pnpm test`
                                                  # before trusting it; order-independence decays silently (B13).
SENTINEL_E2E=1 rtk go test -tags=e2e ./tests/e2e/ -count=1        # green: 81 passed, 0 skips — NEEDS the compose stack up
                                                                  # -tags=e2e is mandatory; without it U8-U10 are silently excluded
                                                                  # U35 additionally needs `jaeger` up; it fails (never skips) if not
rtk buf lint && rtk buf generate                # proto lives at packages/proto/sentinel/v1/; generate is not optional after an edit
```

Running `tests/integration` is **not** in this list on purpose — see Working conventions below before you run it.

When you do run it, use `FORCE_TESTCONTAINERS=1` (see Working conventions). Current state on that path,
measured 2026-07-31: **78 passed, 0 failed, 9 skipped**. Note that on at least one authoring machine
(rootless podman) nine `TestProcessorService_*` tests fail with `postgres ping unavailable ... connection
refused` from a testcontainers lifecycle problem, while **passing in CI** — reproduce in CI before
"fixing" a failure on this path.

`TestIngestAndProcess` and `TestSearchIndexing` used to fail with `Expected status 202, got 401` — root
cause diagnosed and fixed 2026-07-31. `tests/integration/testcontainers/ingestor.go` ran the ingestor
container with `NetworkMode: "host"` and a hardcoded `HostPort: "8080"`. On a machine where the compose
stack also owns 8080, the container failed to bind, and three separate fallback paths silently returned
`{HostIP: "localhost", HostPort: "8080"}` with a nil error — plus a health check that probed
`http://localhost:8080/health`, which the COMPOSE ingestor happily answered. The suite then drove the
compose ingestor, pointed at the shared dev database, while the test had seeded its project into the
testcontainer database — hence 401. The fix: the container now binds a private port chosen via
`net.Listen("tcp", "127.0.0.1:0")` (or `TEST_INGESTOR_PORT` override), passed through as `PORT`; every
provisioning failure now returns `(nil, error)` instead of a silent fallback; and the health check polls
that private port specifically, so a foreign process answering on a well-known port can no longer be
mistaken for readiness. A second, related bug surfaced only once that one was fixed: the container also
defaults `REDIS_ADDR` under host networking, so it silently shared the docker-compose redis instead of
its own isolated one — and since `apps/ingestor-go/auth/apikey.go` caches API-key → project lookups in
Redis, a previous run's cached project id could leak into the next one within the TTL window, producing
`project not found` failures. `StartIngestor` now takes an explicit `redisAddr` and refuses to start with
one unset.

**CI is green on `main` as of 2026-08-01** — all 9 check runs (8 jobs; `go-sdk` runs a 2-leg
Go-version matrix) on the `push` event for merge commit `b895df1`
(PR #11). This is the FIRST green run on `main`; `gh run list --branch main` records the previous
baseline `b9e2018` as `failure`. Any older claim in this repo that CI "has not yet been proven green on a
real push" is stale. `go-root`, `go-sdk` and `go-migrations` pin `GOWORK=off` deliberately (see A2) —
`contract`, `integration` and `e2e` deliberately do NOT set it — they run in workspace mode so the
real SDK is importable (see the comment at `ci.yml`'s `e2e` job). Run the relevant command yourself before claiming anything
works; a green CI badge is evidence about `main`, not about your working tree.

Two CI failures during that work are worth knowing about, because both were "green locally, red in CI"
and neither is exotic — see B12/B13 and VERIFIED_STATE.md's UI-parity entry:
- **Migrations must be idempotent.** One flat directory serves several goose ledgers against the SAME
  physical database (A1), so every migration is replayed per target. Use `IF NOT EXISTS`; `ADD CONSTRAINT`
  has no such form and needs a `pg_constraint` catalog guard. An unguarded `ADD COLUMN` took down four
  migration tests.
- **A test must create the state it asserts on.** A guard that queried for "any issue row that exists"
  passed against a dev DB full of e2e leftovers and failed against CI's freshly-migrated, empty one.

### Known-broken as of 2026-08-01

S1–S18 are **resolved**, and so is every defect P7 found and the 47-finding UI parity register
(D01–D47) — see `## Resolved` in `VERIFIED_STATE.md`. **All 32 rows of the use-case matrix are green**
(`SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/` → **81 passed, 0 skipped**), gated in CI by the `e2e`
job.

`scripts/db/init.sql` — the third, stale schema that caused three separate defects of the same family —
was **deleted** on 2026-07-30. Nothing referenced it: no container mounted it, no test applied it, and the
Taskfile comment already recorded that it had never been wired up. It survived only as a source of wrong
values to copy from.

**Work that is deliberately deferred is documented as P9 in
[docs/plans/E2E_RECOVERY_PLAN.md](docs/plans/E2E_RECOVERY_PLAN.md)**, each with the reason and the
acceptance bar. Read it before concluding something is missing by accident; this repo's characteristic
failure is status recorded optimistically and then believed. P9-1 (org-wide alert UI), P9-2
(observability), P9-3/S18 (`event_id` idempotency) and P9-4 (invitation acceptance) are all **DONE** —
P9-1 and P9-4 shipped in `b9e2018`/`f8d66ac` but did not actually run until the UI parity remediation
(D01, D04); see `docs/plans/UI_PARITY_REMEDIATION_PLAN.md`.

**P9-2 (observability) is DONE as of 2026-07-31**: `slog` everywhere, `/metrics` on both Go services, and
one distributed trace spanning ingestor → NATS → processor, gated by U35. The findings that work chose
*not* to fix are listed as **P9-5** in the same plan — read that before assuming a gap is accidental.
`docker compose up -d` now also starts `jaeger` (dev/CI trace backend, `http://localhost:16686`); nothing
depends on it being up, by design.

Remaining known gaps, none of them a regression from that work:

| | Symptom | Cause |
|---|---|---|
| — | The DLQ needs an operator to drain it | `tools/dlq` can inspect, replay (`-execute`), discard (`-purge`) and drain transient-only (`-drain`), and a `sentinel-dlq-drainer` compose service runs it on a schedule — but it ships gated OFF (`DLQ_DRAINER_ENABLED` and `DLQ_DRAINER_EXECUTE` both `false`), so nothing drains until someone flips both. Permanent-class messages are never auto-replayed by design (D14). Streams are bounded (D13), so a backlog can no longer exhaust storage. |
| — | ~~Invitation acceptance has no route~~ **RESOLVED 2026-08-01** | The route always existed (`src/routes/invitations/[token]/+page.server.ts`); it was unreachable for already-signed-in invitees because `invitations` was missing from `hooks.server.ts`'s `reservedRoutes`. That, and every acceptance-flow defect alongside it (plaintext tokens D06, non-atomic redemption D07, owner-demotion D08, inviter-authority D31, reaping D42), are fixed and merged. This row previously conflated "unreachable for one case" with "does not exist" — itself an instance of this repo's characteristic failure. |
| — | Invitations depend entirely on working email delivery | **A deliberate consequence of D06, not a defect.** The raw token is no longer returned to any client and the modal's copy-paste link was removed, so with no `EMAIL_SERVER` configured an admin can create an invitation with no way to convey it. The create response reports `delivered` so this is at least observable. Affirm or revisit deliberately. |

**S18 (`issues.count` inflation on redelivery — long mislabeled "S16") is RESOLVED as of 2026-07-31**:
`event_id` idempotency end to end and a single-transaction write path (`store.StoreEvent`), gated by
U36 and a 7-test integration suite, every guard mutation-tested. See IDEMPOTENCY_PLAN.md, D16, and
VERIFIED_STATE.md S18. The write path's duplicate handling is READ COMMITTED-specific and the
`NULLIF('')` mapping is load-bearing (proto3 has no null) — read D16's consequences before touching
`StoreEvent`.

### Working conventions

- **Run every gate CI runs before claiming green** — for the dashboard that is `pnpm check` AND
  `pnpm build` AND `pnpm test`. `pnpm build` is the only one that enforces SvelteKit's route-export
  allowlist (`+page.server.ts` may export only `load`, `actions`, …); `check` and `test` pass while
  `build` fails on it. Two constants exported from route files broke the build while the other two gates
  stayed green (B12). Shared constants belong in `$lib/server/`.
- **Every new migration must be idempotent.** One flat directory serves all goose targets and each target
  has its own ledger against the SAME physical database (A1), so migrations are replayed. `IF NOT EXISTS`
  on columns/indexes; `ADD CONSTRAINT` has no such form and needs a `pg_constraint` catalog guard. Verify
  by replaying against a disposable Postgres before pushing — an unguarded `ADD COLUMN` took down four
  migration tests in CI while passing locally.
- **A test must create the state it asserts on**, and a fix must be proven to FAIL before it passes.
  Delete the production line and watch the test go red; if it stays green the test is decoration. A mock
  that returns itself for every call cannot distinguish "did the right thing" from "did nothing" — assert
  the call (B10 addendum). Run `pnpm test --sequence.shuffle` before trusting a suite (B13).
- **Never `return` early from inside a `db.transaction` callback to signal failure** — that COMMITS
  whatever the callback already did. Throw, and catch outside the transaction, or the rollback you think
  you are getting does not happen (D18).
- **On a machine where another stack owns NATS 4222**, bring sentinel up with
  `NATS_HOST_PORT=14222 NATS_MONITOR_HOST_PORT=18222 docker compose up -d` and run host-side suites with
  `NATS_URL=nats://localhost:14222`. `tests/e2e`'s preflight verifies it reached `sentinel-nats` and fails
  with instructions otherwise — a foreign NATS on 4222 accepts the connection and answers happily, so this
  is not a hypothetical. `tests/integration` honours an explicit `NATS_URL` as of 2026-07-30 (its compose
  defaults used to override it silently).
- **Prefix shell commands with `rtk`** (see `~/.claude/CLAUDE.md`), including inside `&&` chains.
- Before changing any exported signature under `apps/` or `packages/shared-go/`, grep `tests/unit/` — it is
  one flat Go package, so a single stale file disables all of it (B4).
- Before marking a feature complete, **grep a call path from the package back to `main()` or an HTTP route.**
  Passing package tests have repeatedly coexisted with entirely unreachable code (B3).
- Anything read out of `event.Metadata` for semantic use must be read **before** `Normalize` (B6).
- Tenant scope must derive from the credential, never from the request body (B7).
- Cross-boundary payloads (SDK↔ingestor JSON, NATS message bodies) have no compiler checking them. Changing a
  field name on one side requires editing the other side in the same change (B5).
- `docker compose up -d` does **not** rebuild images on its own — use `--build --force-recreate`, or you are
  validating a stale image.
- A `.proto` edit changes nothing at runtime by itself — protovalidate reads the descriptor compiled into
  `gen/`. Always follow it with `buf generate` and commit the regenerated files.
- **Never point `tests/integration` at the shared dev database.** Each test target's goose ledger
  (`schema_migrations`, `processor_migrations`, `dashboard_migrations`, ...) tracks the *same* physical
  database independently; one test's `down` can drop a table a different ledger still believes is applied,
  corrupting the dev stack for everyone. Let `TestMain` self-provision via testcontainers, or point
  `DB_URL_*` at a disposable Postgres you're prepared to lose.
- Never read auth context by a bare string literal (e.g. `ctx.Value("project_key")`). Use the typed
  accessors in `apps/ingestor-go/auth/context.go` (`auth.APIKeyHashFromContext`, `auth.WithIdentity`, ...) —
  a bare-string assertion against a typed context key fails silently (`ok == false`, no panic) and was
  exactly how rate limiting was disabled for 100% of requests during this work (R1 in `VERIFIED_STATE.md`).
