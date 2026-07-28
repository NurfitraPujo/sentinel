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
- `tests/{unit,integration,load}` — root-module tests; integration uses testcontainers.

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
rtk go build ./... && rtk go vet ./...   # root module — green
rtk go test ./tests/unit/...             # green, 251 assertions
cd packages/sdk-go && rtk go test ./...  # separate module
docker compose up -d && ./scripts/wait-healthy.sh   # full stack incl. redis + migrate
task test-integration                    # testcontainers; TESTCONTAINERS_PROVIDER=podman supported
cd apps/dashboard-web && pnpm build && pnpm check && pnpm test   # green, 0 type errors
rtk buf lint && rtk buf generate         # proto lives at packages/proto/sentinel/v1/
```

**CI exists as of P0-1** (`.github/workflows/ci.yml`, 7 jobs). Before that there was none, which is why
everything below reached `main`. Two things to know: `go-root`, `go-sdk` and `go-migrations` pin
`GOWORK=off` deliberately (see A2) — `contract` is the only workspace-mode job; and the `integration` job
is still job-level `continue-on-error` because its ~15 failures are real and owned by P2/P3/P4. Run the
relevant command yourself before claiming anything works.

### Known-broken as of 2026-07-28

S1 and S2 are **resolved** — see the `## Resolved` section of `VERIFIED_STATE.md`. S3–S11 are all still live.

Do not treat these as your regression — they are pre-existing. Full detail and evidence in
`docs/memory/VERIFIED_STATE.md`.

| | Symptom | Cause |
|---|---|---|
| S3 | `/ingest` returns 400 for **every** event | `string.len = 10000` in `packages/proto/sentinel/v1/error_event.proto:40` means *exactly* 10000 bytes; should be `max_len` |
| S4 | Go SDK events are 100% rejected, silently | field names diverge (`error_message`/`context`, no `platform`); SDK ignores the HTTP status |
| S5 | Regression tracking never fires | `Normalize` rewrites `release_version` to `<VERSION>` before it is read |
| S6 | Any key can write to any tenant's project | handlers read `project_key` from the body, not the authenticated context |
| S7 | Key revocation/expiry ineffective | `{keyId}` vs `key_hash` mismatch; `API_KEYS` stream never created; `expires_at` never checked |
| S8 | `alerts` + `notifiers` never run | never constructed in `NewProcessorService` |
| S9 | Degradation buffer logic inverted | `CheckAndBuffer` returns `true` both when healthy and when buffered |
| S10 | Rate limiting non-atomic, fails open | 4 unpipelined Redis calls; `redisClient, _ :=` discards the error; no redis in compose |

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
