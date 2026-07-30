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

### Repository shape

Go/SvelteKit monorepo for an error-tracking pipeline:
`sdk → ingestor-go (HTTP :8080) → NATS JetStream → processor-go → PostgreSQL`, with `dashboard-web`
(SvelteKit + Auth.js + Drizzle) reading the same database.

- `apps/ingestor-go` — auth, rate limit, validate, publish. The only externally exposed service.
- `apps/processor-go` — deserialize, normalize, mask, fingerprint, upsert issue + occurrence, index.
- `apps/dashboard-web` — UI and JSON API.
- `packages/shared-go` — pgx pool, NATS pub/sub, redis client.
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
rtk go test ./tests/unit/...                    # green, 241 assertions
cd packages/sdk-go && rtk go test ./...         # separate module — green
cd packages/db-migrations && rtk go test ./...  # separate module — green
docker compose up -d --build --force-recreate   # full stack incl. redis + migrate; plain `up -d` does NOT rebuild
./scripts/wait-healthy.sh                       # then this — blocks until every service reports healthy
cd apps/dashboard-web && pnpm build && pnpm check && pnpm test   # green: 690 files/0 errors, 63 tests
SENTINEL_E2E=1 rtk go test -tags=e2e ./tests/e2e/ -count=1        # green: 56 tests, 0 skips, ~125s — NEEDS the compose stack up
                                                                  # -tags=e2e is mandatory; without it U8-U10 are silently excluded
rtk buf lint && rtk buf generate                # proto lives at packages/proto/sentinel/v1/; generate is not optional after an edit
```

Running `tests/integration` is **not** in this list on purpose — see Working conventions below before you run it.

**CI exists as of P0-1** (`.github/workflows/ci.yml`, 7 jobs) but has not yet been proven green on a real
push from this branch — P2/P2b's changes are staged, uncommitted. `go-root`, `go-sdk` and `go-migrations`
pin `GOWORK=off` deliberately (see A2) — `contract` is the only workspace-mode job; `integration` is
job-level `continue-on-error` because its failures are real (10 of 75 tests, current count — re-verify, do
not quote this). Run the relevant command yourself before claiming anything works.

### Known-broken as of 2026-07-30

S1–S17 are **resolved**, and so is every defect P7 found — see `## Resolved` in `VERIFIED_STATE.md`.
**All 32 rows of the use-case matrix are green** (`SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/` → 56
passed, 0 skipped, 124.8s), gated in CI by the `e2e` job.

Remaining known gaps, none of them a regression from that work:

| | Symptom | Cause |
|---|---|---|
| — | `scripts/db/init.sql` is a **third, stale schema** | still `CHECK (status IN ('open','resolved','ignored'))`. It has now produced three separate defects (the `'stale'`/`'open'` retention bug, `'status_change'` vs `'status_changed'`, and Drizzle-vs-migrations column drift). Deleting it is the real fix and has not been done. |
| — | The DLQ has ~6,100 dead-lettered events and no consumer | `tools/dlq` can replay them but nothing drains, replays or alerts automatically. The processor correctly reports `attention: dead-lettered events awaiting replay`. |
| — | Invitation acceptance has no route | U22 proves the create half end to end; nothing anywhere consumes an invitation token. Asserted as a wall, not assumed. |
| — | `issues.count` can inflate on partial-failure redelivery (S16) | the issue upsert and the occurrence insert are separate transactions with no `event_id` idempotency. U28 asserts occurrences are exactly-once and checks the counter agrees, so a regression is visible. |

### Working conventions

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
