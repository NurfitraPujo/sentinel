# E2E Recovery Plan — Making Sentinel Actually Work End-to-End

**Drafted**: 2026-07-28 · **Baseline commit**: `ad2f967` · **Evidence base**: [VERIFIED_STATE.md](../memory/VERIFIED_STATE.md)

> [!IMPORTANT]
> This plan assumes **nothing in `specs/` is true** unless [VERIFIED_STATE.md](../memory/VERIFIED_STATE.md)
> confirms it. Every work item below ends in a **command that must pass**. A task is not done when the code
> is written; it is done when its acceptance command passes on a clean checkout.

> [!WARNING]
> **State as of 2026-07-28 (post P0/P1 pass, this update dated 2026-07-28)**: the `ad2f967` baseline above no
> longer describes the tree. P0 and P1 have executed and gates G1–G7 genuinely pass (`go build`/`go vet`
> clean, `tests/unit` runs 251 assertions, dashboard builds, `pnpm check` = 0 errors, 19 dashboard tests
> pass). Multiple agents working this plan concurrently have also found their assigned item's "current state"
> description already partially true, uncommitted, in the working tree before they started — do not trust a
> per-item "current state" paragraph below without re-verifying it against the tree in front of you first.
> Known corrections so far: **C1 is resolved** (see below); the `proto` CI job description misstated what
> `buf` actually catches (see P0-1's acceptance note); C5's "skip silently" premise does not match
> `tests/integration/setup_test.go`'s actual `TestMain` behavior (see C5). Re-verify every other item's
> "current state" the same way before planning against it.

---

## 0. Goal and definition of done

**Goal**: every feature the repo claims to have (001, 004, 005, 006, 007, 008, alerting) executes correctly
against a clean local stack and is protected by an automated gate that fails on regression.

The project is "working end-to-end" when **all eleven** of these hold on a fresh clone:

| # | Gate | Command |
|---|---|---|
| G1 | Root module builds and vets clean | `rtk go build ./... && rtk go vet ./...` |
| G2 | Unit suite runs and passes | `rtk go test ./tests/unit/...` |
| G3 | SDK module builds, vets, tests | `cd packages/sdk-go && rtk go test ./...` |
| G4 | Migrations module builds and tests | `cd packages/db-migrations && rtk go test ./...` |
| G5 | Dashboard builds | `cd apps/dashboard-web && pnpm build` |
| G6 | Dashboard typechecks with **0** errors | `pnpm check` |
| G7 | Dashboard tests run | `pnpm test` |
| G8 | Full stack boots from compose, all healthy | `docker compose up -d && ./scripts/wait-healthy.sh` |
| G9 | Integration suite passes, **skips are failures** | `SENTINEL_E2E=1 rtk go test ./tests/integration/... -count=1` |
| G10 | The SDK→dashboard journey works with the real published SDK | `rtk go test ./tests/e2e/... -count=1` |
| G11 | All of G1–G10 run on push | GitHub Actions green |

**Non-goals** (explicitly deferred, listed so they are not silently assumed): multi-region, horizontal
processor scaling, source maps / symbolication, non-Go SDKs, retention beyond the existing cron route.

### The single most important structural fact

There was **no CI** when this plan was drafted (P0-1 has since added `.github/workflows/ci.yml`). Every defect in VERIFIED_STATE.md reached `main` because nothing checked it. Fixing the
eleven bugs without fixing that produces the same state again in a month. **Phase 0 is therefore CI, and it
is not optional or reorderable.** It is deliberately sequenced *before* the bug fixes so that each subsequent
fix lands with a gate already watching it.

---

## 1. Constraints that shape every task below

These invalidate otherwise-obvious approaches. Read before planning any individual item.

| ID | Constraint | Consequence for this plan |
|---|---|---|
| C1 | ~~Three Go modules, no `go.work`, no `replace`~~ — **RESOLVED 2026-07-28 by P0-3** (A2) | P0-3 committed a `go.work` (`use . ./packages/sdk-go ./packages/db-migrations`) and removed it from `.gitignore`. The root module **can now import `packages/sdk-go`** in workspace mode, so a `tests/contract/` package (P2-4) is no longer blocked by the module graph — plan around it as available, not as a prerequisite still to be built. `no replace` remains true and is now covered by C2. |
| C2 | Root pins `db-migrations` at a **published pseudo-version** — **still true, unaffected by C1's resolution** | Local migration edits are invisible to any `GOWORK=off` build (notably CI's `go-root` job, which pins `GOWORK=off` on purpose — see A2) until committed and the pseudo-version bumped. Workspace mode (post-C1) hides this locally, which is exactly why `go-root` deliberately opts out of it. Any task touching migrations *and* Go code that must be verified the way `go-root` verifies it is still a two-commit dance. |
| C3 | `tests/unit/` is **one flat Go package** | One stale file disables ~2,800 lines (B4). Every signature change under `apps/` or `packages/shared-go/` must grep `tests/unit/` in the same commit. |
| C4 | Cross-boundary payloads (SDK↔ingestor JSON, NATS bodies) have **no compiler** (B5) | Field renames must be made on both sides in one change, and must be covered by a contract test, not a unit test. |
| C5 | Integration tests **skip silently** when infra is down (`e2e_test.go:89`, `newPostgresPool`) | A green run today proves nothing. P0-4 converts skips to failures under a flag. **Correction (2026-07-28): the premise is false as stated.** `tests/integration/setup_test.go`'s `TestMain` self-provisions Postgres/NATS/Redis/the ingestor via testcontainers and calls `os.Exit(1)` **unconditionally** on provisioning failure, independent of `SENTINEL_E2E` — proven with a broken `DOCKER_HOST`. `requireInfra` (P0-4's flag-gated `t.Fatalf`/`t.Skipf` helper) is correct by inspection, but by the time any individual test calls it, `TestMain` has already either succeeded (so `err == nil` and `requireInfra` is a no-op) or already called `os.Exit(1)` itself. **`requireInfra`'s `Fatalf` branch is therefore structurally present but mechanically unproven at runtime** — no observed test run has actually exercised it. Do not treat a future red run through that branch as evidence it works; the failure you'd be observing was very likely caused by `TestMain`, not by `requireInfra`. See the matching note on P0-4. |
| C6 | `task` (go-task) is **not installed** on this machine | Every command documented as `task X` is currently unrunnable. Plan provides raw equivalents. |
| C7 | Prefix all shell commands with `rtk`, including inside `&&` chains | Repo convention. |

---

## 2. Phase map and dependency order

```
P0  Make failure visible          ──┬─> P1  Restore the build      ──┬─> P2  Fix the wire contract
    (CI, go.work, real stack)       │      (compile, typecheck)      │      (proto/SDK/ingestor)
                                    │                                │
                                    └─> P3  Security & tenancy  <────┘
                                                 │
                                    P4  Processor correctness  <─────┤
                                                 │                   │
                                    P5  Wire the unwired  <──────────┤
                                                 │                   │
                                    P6  Dashboard completeness  <────┘
                                                 │
                                    P7  E2E proof harness (the actual deliverable)
                                                 │
                                    P8  Ops hardening + memory reconciliation
```

**P0 and P1 are strictly sequential and strictly first.** P2–P6 can be parallelised across people once P1
lands, with the one exception noted in P3-1 (depends on P2-1's `project_id` field). P7 can only be written
after P2–P6, because it asserts their behavior.

---

## P0 — Make failure visible

*Rationale: until a broken build fails loudly and automatically, every fix below is unprotected.*

### P0-1 · Add CI

**Create** `.github/workflows/ci.yml`. Jobs, all on `push` + `pull_request`:

| Job | Steps | Notes |
|---|---|---|
| `go-root` | `go build ./...`, `go vet ./...`, `go test ./tests/unit/... -count=1` | Run with `GOWORK=off` to preserve module-graph fidelity (C2) |
| `go-sdk` | `cd packages/sdk-go && go build ./... && go vet ./... && go test ./... -count=1` | Independent module; matrix over `go 1.21` (its declared minimum) and latest |
| `go-migrations` | `cd packages/db-migrations && go build ./... && go test ./... -count=1` | |
| `dashboard` | `pnpm install --frozen-lockfile`, `pnpm build`, `pnpm check`, `pnpm test` | `pnpm check` must be **error-gating**, not advisory |
| `integration` | boot compose services, run migrations, `SENTINEL_E2E=1 go test ./tests/integration/... -count=1` | Needs P0-4 and P0-5 |
| `proto` | `buf lint`, `buf breaking --against '.git#branch=main'`, `buf generate` + `git diff --exit-code gen/` | **Correction (2026-07-28): does NOT catch S3's class of bug.** `buf lint` is naming/style only; `buf breaking` compares descriptor *structure*, not constraint semantics, and **passes today with `string.len = 10000` still in place**; `buf generate` + diff only proves `gen/` matches whatever the `.proto` says — a confidently-wrong constraint regenerates cleanly. This job is still worth having (it catches drift and naming/breaking-change classes of bug) but it would not have caught S3. |
| `contract` | `go test -tags=contract ./tests/contract/... -count=1` (see P0-3, P2-4) | Workspace mode — the **only** job without `GOWORK=off`. Files there **must** carry `//go:build contract` |

**Acceptance**: **correction (2026-07-28)** — only P2-4's contract test (`tests/contract/`, running real `protovalidate` against a real payload) and U4 in the P7 e2e harness catch this bug class. A PR that reintroduces the `string.len = 10000` bug fails the `contract` job (once P2-4 lands); it does **not** fail `proto`, which is structurally incapable of catching a semantically-wrong-but-well-formed CEL/field constraint.

**Deliberate first commit**: land CI **while the tree is still red**, with the currently-failing jobs marked
`continue-on-error: true` and a tracking issue per job. Flip each to hard-fail as its phase completes. This
prevents "we'll add CI at the end", which is how this state was reached.

### P0-2 · Add a `test` script and vitest to the dashboard

`apps/dashboard-web/package.json` declares no `test` script and no `vitest`, yet three `.test.ts` files
exist. Add `vitest` + `@testing-library/svelte` to devDependencies and `"test": "vitest run"`.

**Acceptance**: `pnpm test` runs the three existing test files.

### P0-3 · Commit a `go.work` and unblock cross-module testing

**Status (2026-07-28): DONE.** `go.work` was in `.gitignore`, which was *why* the SDK contract was
untestable (C1, now resolved — see constraints table above). The steps below were:

- Create `go.work` with `use ./ ./packages/sdk-go ./packages/db-migrations`. — done; committed at repo root.
- Remove `go.work` from `.gitignore`; add `go.work.sum` to `.gitignore` or commit it (prefer committing for
  reproducibility). — done; both `go.work` and `go.work.sum` are committed, `.gitignore` no longer lists
  `go.work`.
- **Keep `GOWORK=off` in the `go-root` CI job** so the published-pseudo-version path (C2) is still exercised.
  This is the safeguard against the workspace masking a real version skew. — done; see `.github/workflows/ci.yml`'s `go-root` job.

**Risk**: workspace mode changes which `db-migrations` the root module compiles against locally vs in the
`go-root` job. That divergence is intentional and is documented in `ARCHITECTURE.md` A2.

**Acceptance**: a new `tests/contract/` package can `import "github.com/NurfitraPujo/sentinel/packages/sdk-go"`
and compile. **Not yet exercised**: `tests/contract/` itself does not exist yet — that is P2-4's deliverable.
The module-graph precondition it depends on (this item) is satisfied; the package itself is not written.

### P0-4 · Make integration skips fail

`tests/integration/e2e_test.go:89` and `newPostgresPool` call `t.Skipf` when infra is unreachable. That is
correct for a laptop and catastrophic for CI — it is the direct reason S3 survived (C5).

Introduce a helper:

```go
// tests/integration/setup_helper_test.go
func requireInfra(t *testing.T, err error, what string) {
    if err == nil { return }
    if os.Getenv("SENTINEL_E2E") == "1" {
        t.Fatalf("%s unavailable and SENTINEL_E2E=1: %v", what, err)
    }
    t.Skipf("skipping: %s unavailable: %v", what, err)
}
```

Replace **every** `t.Skipf` in `tests/integration/` with it. Set `SENTINEL_E2E=1` in the CI `integration` job.

**Acceptance**: `SENTINEL_E2E=1 go test ./tests/integration/...` with the stack down **fails**; with the stack
up it runs the assertions.

> [!NOTE]
> **Status (2026-07-28): structurally done, mechanically unproven.** `requireInfra` was added to
> `tests/integration/setup_helper_test.go` and is wired into every call site that used to `t.Skipf`, and is
> correct by inspection. But `tests/integration/setup_test.go`'s `TestMain` provisions all infra
> (Postgres/NATS/Redis/ingestor) via testcontainers itself, and calls `os.Exit(1)` **unconditionally** the
> moment any of that provisioning fails — before any individual test, and before `SENTINEL_E2E` is ever
> consulted (proven with a broken `DOCKER_HOST`). In every run observed so far, `requireInfra` is called with
> `err == nil` (TestMain already succeeded) — its `t.Fatalf` branch has not actually fired. **Do not treat a
> future red CI run as proof this acceptance criterion works** unless you have confirmed the failure came
> from inside a test via `requireInfra`, not from `TestMain`'s own `os.Exit(1)`. See constraint C5.

### P0-5 · Make the local stack real

`docker-compose.yml` cannot currently run the system:

| Defect | Fix |
|---|---|
| **No `redis` service**, no `REDIS_ADDR` — so rate limiting and the API-key cache are always in fail-open mode (S10) | Add `redis:7-alpine` with a healthcheck; add `REDIS_ADDR: redis:6379` to `ingestor`; add `depends_on: redis: {condition: service_healthy}` |
| **No migration step** — services boot against an empty schema | Add a `migrate` one-shot service running `packages/db-migrations/cmd/migrate up`, with `ingestor`/`processor`/`dashboard` depending on `service_completed_successfully` |
| NATS mapped twice (`4222:4222` **and** `4223:4222`) | Delete the `4223` mapping |
| `nats-init` creates only `ERROR_EVENTS`; ingestor subscribes to `API_KEYS` (S7) | Add the `API_KEYS` stream to `scripts/nats-init.sh` |
| Dashboard has `DATABASE_URL` but no `AUTH_SECRET`, `AUTH_URL`, Google/SMTP vars — Auth.js cannot start | Add them, sourced from `.env` |
| No `.env.example` at repo root | Create one documenting every variable each service reads |

**Acceptance**: `docker compose up -d` from a clean clone reaches all-healthy, and `curl localhost:8080/health`
returns 200. Add `scripts/wait-healthy.sh` so CI and humans use the same gate.

### P0-6 · Repair the Taskfile

- `db-migrate` runs `docker compose exec -T sentinel-postgres psql -f /docker-entrypoint-initdb.d/init.sql`.
  **Both halves are wrong**: `sentinel-postgres` is a *container name*, not the compose *service* name
  (`postgres`), and no `init.sql` is mounted anywhere. It also bypasses goose entirely, contradicting **D3**.
  Rewrite it to invoke the goose CLI.
- `test-e2e` depends on `db-migrate`, so it is broken transitively.
- **Verify** whether `db:{{.TARGET}}-status` / `-up` / `-down` / `-baseline` / `-reset` actually work —
  go-task does not template task *names*, so these are likely five literal, uncallable task names. If so,
  convert to a single `db-migrate` task taking `TARGET` as a var.
- `task` is not installed on the current machine (C6). Document installation in `README.md`, and make CI call
  the underlying commands directly rather than via `task`.

**Acceptance**: `task db-migrate` applies migrations to a fresh compose postgres; `task --list` shows no
templated names.

---

## P1 — Restore the build

*Nothing below can be verified until the code compiles.*

### P1-1 · Un-break `tests/unit` (S1, B4)

`tests/unit/ingestor_middleware_test.go:17,21` calls the deleted token-bucket API:

```
too many arguments in call to middleware.NewRateLimiter
   have (nil, number, time.Duration)   want (*redis.Client)
ratelimiter.Allow undefined
```

Rewrite the file against the current Redis-backed `NewRateLimiter(*redis.Client)`. Because the new limiter
requires Redis, either use `miniredis` for a real in-process server (preferred — it exercises the ZSET logic)
or move the test to `tests/integration/` with a testcontainer.

**Then audit the other ten files** — they have not compiled or run since the 008 merge, so some assertions
are certainly stale beyond this one signature.

**Acceptance**: `rtk go test ./tests/unit/... -count=1` passes and reports **11 files' worth** of tests, not
a build error. Record the actual test count in the commit message as the new baseline.

### P1-2 · Un-break the dashboard build (S2)

| Item | File | Fix |
|---|---|---|
| Literal escaped backticks — the entire build outage | `src/lib/db/queries/issues.ts:202` | `` regressionCount: sql`${issues.regressionCount} + 1`, `` |
| `Cannot find module '$lib/db'` in **6 files** | `api/organizations/switch`, `api/organizations/[orgId]/invitations`, `api/issues/[issueId]/relations`, `api/projects/[projectId]/issues`, `.../issues/batch`, `lib/db/queries/organizations.ts` | Import `$lib/server/db` (the alias that exists) |
| Filters on nonexistent `issues.releaseVersion` | `api/projects/[projectId]/issues/+server.ts:43` | `release_version` lives on `error_occurrences` — join, or drop the filter |
| `locals.getSession()` — never populated | same file | Use `locals.auth()` (Auth.js v1 API), matching `hooks.server.ts` |
| `Cannot find module 'vitest'` | 3 `.test.ts` files | Resolved by P0-2 |

**Delete the stale `build/` and `.svelte-kit/` directories** as part of this task — they are from an older
successful build and actively mask the failure, making the app look runnable.

**Acceptance**: `pnpm build` succeeds and `pnpm check` reports **0 errors** (currently 39).

---

## P2 — Fix the wire contract

*This is the phase that makes ingest work at all. Everything downstream is dead until it lands.*

### P2-1 · Establish one canonical event schema

Today there are **three** divergent definitions of the same event and no compiler between them (C4):

| Concern | `packages/proto/error_event.proto` | `apps/ingestor-go/validation.ErrorPayload` | `packages/sdk-go/event.go` |
|---|---|---|---|
| message | `message` (broken rule) | `Message` → `message` | `error_message` ❌ |
| metadata | `metadata` | `Metadata` → `metadata` | `context` ❌ |
| platform | required, `^[a-z0-9]+$` | required | **absent** ❌ |
| release | — | **absent** | `release_version` (never read) ❌ |
| frame in_app | `in_app` | `InApp` → `in_app` | **absent** ❌ (causes S11) |
| project id | — | — | — |

**Decision: the proto is the source of truth; the SDK changes to match it, not the reverse.** The server-side
names (`message`, `metadata`) already match the database columns and the dashboard, so moving the server
would be a far larger blast radius.

Schema changes to make in `packages/proto/error_event.proto`:

1. **Fix S3** — line 40: `[(buf.validate.field).string.len = 10000]` → `.max_len = 10000`. `len` means
   *exactly* 10000 bytes, which is why every event 400s. Note the CEL block directly above already states the
   rule correctly (`this.message.size() <= 10000`), so consider deleting the now-redundant field rule instead.
2. **Audit every other `string.len` / numeric rule in the file for the same mistake.** Do not fix only the one
   that was caught.
3. Add `release_version` as a first-class field (currently smuggled through metadata, which is what breaks S5).
4. Add `project_id` (UUID string) — set by the ingestor from the *authenticated* key, consumed by the
   processor. This is the mechanism for P3-1 and it removes the `SELECT id FROM projects WHERE name = $1`
   lookup entirely.

Regenerate `gen/` via `buf generate`; CI job `proto` (P0-1) enforces that `gen/` is in sync.

### P2-2 · Update the ingestor to the canonical schema

- Add `ReleaseVersion string \`json:"release_version"\`` to `validation.ErrorPayload`.
- Map it into the proto message in the ingest service.
- Set `project_id` from the authenticated context (P3-1), not the body.

### P2-3 · Update the Go SDK (S4, S11)

In `packages/sdk-go/event.go`:

| Change | Reason |
|---|---|
| `error_message` → `message` | S4: message is always empty server-side |
| `context` → `metadata` | S4: all user tags and PII-scrubbed context are silently dropped |
| **add** `platform` (set to `"go"`) | S4: `platform` is required and regex-validated — this alone is a hard 400 |
| **add** `release_version` from `Config.ReleaseVersion` | S5: without it regression detection can never fire |
| **add** `InApp bool \`json:"in_app"\`` to `Frame`, and populate it | S11: with no in-app frames every error of a class collapses into one issue |

In `packages/sdk-go/transport.go` `sendBatch`:

- **Check `resp.StatusCode`.** It currently ends `defer resp.Body.Close()` with no status inspection, which is
  why a 100% rejection rate was completely silent. On 4xx: log the status and response body when
  `Config.Debug` is set; increment a dropped-event counter. On 5xx / network error: retry with capped
  exponential backoff, then drop with a log.
- Consider surfacing an optional `Config.OnError func(error)` hook so applications can alert on ingest failure.

`in_app` determination for Go: a frame is in-app when its file path is under the module root and not under
`/usr/local/go/` or a `vendor/`/module-cache path. Implement it explicitly; do not leave it always-false.

**Version bump**: this is a breaking wire change for anyone on the current SDK. Since the current SDK is 100%
rejected, nobody has working data — say so explicitly in the changelog and release as `v0.2.0`.

### P2-4 · Add the contract test that makes this class of bug impossible

Create `tests/contract/sdk_ingestor_test.go` (enabled by the `go.work` from P0-3).

> [!IMPORTANT]
> Every file in `tests/contract/` **must** begin with `//go:build contract`. CI's `go-root` job pins
> `GOWORK=off` (constraint C2), and under that setting an import of `packages/sdk-go` does not resolve —
> an untagged file turns `go-root` red with a confusing `no required module provides package` and makes
> P2-4 look like it broke P0-3. The `contract` job runs in workspace mode with `-tags=contract`.

```go
// Marshal a fully-populated sentinel.Event from the real SDK,
// decode it into the real validation.ErrorPayload,
// map it to the real proto message,
// and run the real protovalidate validator.
// Assert: zero unknown fields, zero empty required fields, validation passes.
```

Use `json.Decoder.DisallowUnknownFields()` so a future SDK-side rename **fails the build** rather than
silently dropping data. Add the mirror direction too: assert every non-optional `ErrorPayload` field is
actually produced by the SDK.

**Acceptance**: the test fails if any single field name in P2-3's table is reverted. This is the durable fix
for B5; the field renames alone are not.

### P2-5 · Decide the batch endpoint semantics

`sendBatch` POSTs to `{endpoint}` for one event and `{endpoint}/batch` for many — and marshals a bare object
in the first case, an array in the second. Meanwhile `handleBatchIngest`:

- has **no batch-size cap and no request body limit** (trivially DoS-able on the only externally exposed
  service);
- returns **202 even when every item fails**, so the caller cannot distinguish total failure from success.

Fix: cap batch size (e.g. 500) and wrap the body in `http.MaxBytesReader`; return `207`-style per-item results
or at minimum `{ingested, failed, errors[]}` with a non-2xx when `ingested == 0`. Then teach the SDK to read it.

**Acceptance**: a batch of 3 valid + 2 invalid events returns a body that names the 2 failures, and the SDK
logs them under `Debug`.

---

## P3 — Security and tenancy

### P3-1 · Ingest must be tenant-scoped (S6, B7) — **highest severity in the repo**

`apps/ingestor-go/auth/apikey.go:83` resolves the authenticated project into the request context; then
`handleIngest`/`handleBatchIngest` (`main.go:146-165`) **decode `project_key` straight from the request body**
and publish it. `processor-go` resolves that with `SELECT id FROM projects WHERE name = $1`.

**Net effect: a holder of any active `ingest`-scope key for any organization can write events into any other
tenant's project by naming it in the JSON body.**

Fix, in order:

1. Add a `apps/ingestor-go/auth/context.go` with a **private key type** (`type ctxKey int`), replacing the bare
   string keys `"project_key"`, `"rate_limit_rpm"`, `"api_key_hash"` — bare strings are collision-prone across
   packages and are a Go vet-able antipattern.
2. Handlers read the resolved project from context and **overwrite** `payload.ProjectKey` / set `project_id`.
   If the body names a *different* project, return **403** rather than silently rewriting — silent rewrite
   hides misconfigured clients.
3. Publish `project_id` (P2-1) so the processor never does a name lookup.
4. `projects.name` has **no UNIQUE constraint** (`1716508800_init.sql`), so two orgs can own same-named
   projects and `GetProjectByKey` picks arbitrarily. Add `UNIQUE (organization_id, name)` — *not* a global
   unique, which would break multi-tenancy. Once (3) lands, `GetProjectByKey` should be deleted.

**Acceptance**: new integration test — key for project A, body naming project B → **403**, and zero rows land
in B. This test must exist before the fix is called done.

### P3-2 · Make revocation and expiry actually work (S7)

Three independent breaks:

| Break | Location | Fix |
|---|---|---|
| Payload mismatch — dashboard publishes `{ keyId }`, ingestor reads `data["key_hash"]` | `apps/dashboard-web/src/lib/db/queries/apikeys.ts:139` vs `apps/ingestor-go/auth/apikey.go:38` | Publish `{ keyId, keyHash }`; ingestor deletes the cache entry by hash. Assert the shape in a contract test (C4) |
| Stream `API_KEYS` never created; `NewSubscriber`'s error is **discarded** (`subscriber, _ :=`, `main.go:87`) | `scripts/nats-init.sh` | Create the stream (P0-5) **and** make the error fatal — a silent failure here is what hid this for a full release |
| `getAPIKeyData` filters on `status` only, **never checks `expires_at`** | `apps/ingestor-go/auth/apikey.go` | Add `AND (expires_at IS NULL OR expires_at > now())` |
| `rotateApiKey` sets `expires_at` on the old key but leaves `status='active'` → **rotated keys are valid forever** | dashboard query layer | Set status, or rely on the `expires_at` check above — do both |

Also reduce the Redis cache TTL or publish an explicit invalidation ack, so "instant revocation" in
`WORKLOG.md` becomes true rather than "within 60 seconds".

**Acceptance**: integration test — authenticate, revoke via the dashboard API, next request is **401 within
1 second**. Second test: rotate, then the old key is 401.

### P3-3 · Fix rate limiting (S10)

- Replace the four unpipelined round-trips (`ZRemRangeByScore` → `ZCard` → decide → `ZAdd`) with a **single
  Lua script** (or `EVALSHA`). Under concurrency the current form lets every request read the same count
  before any write, so the effective limit is unbounded.
- `main.go:83`: `redisClient, _ := redis.NewClient(...)` **discards the error**, leaving `redisClient == nil`,
  which makes the middleware pass everything through *and* disables the API-key cache. Make it fatal, or
  require an explicit `RATELIMIT_ENABLED=false` opt-out. Fail-open must be a decision, never an accident.
- `RATELIMIT_STRICT_MODE=true` covers only the `ZCard`-error path, not the nil-client path — extend it.
- **D9** describes *hierarchical* (org → project → key) limiting; the implementation keys solely on
  `api_key_hash`. Either implement the tiers or amend D9. Do not leave the decision record lying.

**Acceptance**: an integration test firing 200 concurrent requests against a limit of 100 observes ≤100
accepted (currently it will observe ~200).

### P3-4 · Dashboard authorization

- `src/routes/api/organizations/[orgId]/keys/+server.ts` is a **mock returning hardcoded fixtures**, with
  `// TODO: implement RBAC check` and **no auth on either GET or POST** — while 008 is marked Completed. The
  real query layer (`src/lib/db/queries/apikeys.ts`) is written and reasonable; wire the route to it.
- `src/lib/rbac.ts` knows only `admin | developer | viewer`. The database defines
  `admin|developer|viewer|support` for `project_members` and `owner|admin|engineer|support|viewer` for
  `organization_members`. **`hasPermission('owner', …)` returns `false`** — organization owners are currently
  denied everything. Reconcile the enum against the migrations and add a test that asserts every DB role
  value is known to `rbac.ts`.
- `src/lib/server/auth-config.ts:11`: `ALLOWED_EMAIL_DOMAIN = 'company.com'` is a hardcoded placeholder that
  blocks all Google sign-in in any real deployment. Make it env-driven, empty = allow all.
- `src/lib/rate-limit.ts` is an in-process `Map` with no eviction — ineffective behind more than one instance
  and an unbounded memory leak. Move to the shared Redis, or scope it explicitly to single-instance dev.

**Acceptance**: authenticated-as-viewer → 403 on key creation; unauthenticated → 401; owner → 200. Table-driven
test over every role in the database enum.

---

## P4 — Processor correctness

### P4-1 · Read release_version before normalization (S5, B6)

`apps/processor-go/event/event.go:41-49` runs `NormalizeMap` over metadata **before** reading
`release_version` out of it at line 46. `normalizer.versionRegex` rewrites every semver to the literal
`<VERSION>`, so `ReleaseVersion` is always `"<VERSION>"`. Downstream `store.isRegressionVersion` parses that
with `strconv.Atoi` → `0` for every component, making all version comparison meaningless — the headline
feature of 006 is unreachable.

Fix: with `release_version` promoted to a first-class field (P2-1) the metadata path becomes a *fallback*.
Read that fallback **before** `Normalize`, and add `release_version` to the normalizer's exclusion set.

**Acceptance**: unit test asserting `Normalize` on `{"release_version": "1.4.2"}` yields
`ReleaseVersion == "1.4.2"`, plus an integration test proving a regression row is written when an older
release's issue reappears in a newer one.

### P4-2 · Fix the degradation buffer (S9, supersedes B1/D1)

`apps/processor-go/service/processor_service.go:36-41`:

```go
if !s.degradation.CheckAndBuffer(ctx, data) {
    log.Printf("Event buffered due to database unavailability")
    return nil          // ← ACKs the NATS message
}
return s.processEventInternal(ctx, data)
```

`CheckAndBuffer` returns `true` both when the DB is healthy **and** when the DB is down but the event was
buffered:

| DB | Buffer | Returns | Actual | Intended |
|---|---|---|---|---|
| up | — | `true` | processes | ✅ |
| down | has room | `true` | **also tries to process, fails, NAKs → duplicate on redelivery** | buffer only |
| down | full | `false` | logs "buffered", returns nil → **ACKed and lost** | drop loudly |

The log line fires exactly when the event was *not* buffered. **This directly contradicts D1/B1, which record
the buffer as the mitigation for data loss on DB outage — it currently causes it.**

Fix: replace the boolean with an explicit tri-state (`Processed | Buffered | Dropped`) so the call site cannot
misread it. Additionally:

- `CheckAndBuffer` issues a `db.Ping` **per event** — cache health with a short TTL / background probe.
- `processEventInternal` calls `degradation.Flush` at the end of **every** successful event (line 140), a
  re-entrant call back into itself. Move flushing to a dedicated goroutine triggered on health recovery.

**Acceptance**: integration test that kills postgres mid-stream, sends N events, restores postgres, and
asserts **exactly N** occurrences land — no loss, no duplicates.

### P4-3 · Fix fingerprint collapse (S11)

`apps/processor-go/fingerprint/fingerprint.go` hashes `ErrorClass` plus up to 3 frames where `InApp == true`.
With no in-app frames the input degenerates to the error class alone, so **every error of a given class in a
project collapses into one issue**. P2-3 makes the Go SDK send `in_app`, but the fallback must be fixed
independently for any client that does not.

Fix: when no in-app frames exist, fall back to the top N frames regardless of `in_app`. Consider including a
normalized message shape as a tiebreaker.

**Note**: this changes fingerprints for existing data. Decide explicitly whether to backfill/rewrite or accept
a one-time regrouping, and record it in `DECISIONS.md`.

**Acceptance**: unit test — two different errors of class `*errors.errorString` with different stacktraces and
no `in_app` frames produce **different** fingerprints.

### P4-4 · NATS reliability (lower-severity notes)

- `packages/shared-go/nats/subscriber.go` sends `s.errors <- err` on a **capacity-1** channel. The ingestor
  never drains `Errors()`, so its subscriber goroutine **blocks permanently after the second error**. Either
  drain it in the ingestor (the processor already does) or make the send non-blocking with a drop counter.
- A handler error triggers a bare `msg.Nak()` with no max-delivery limit and no DLQ, so a permanently
  malformed message **redelivers forever**. Set `MaxDeliver` on the consumer and route exhausted messages to a
  DLQ subject.
- Pick one protobuf runtime: `apps/processor-go/event/event.go` imports the deprecated
  `github.com/golang/protobuf/proto` while the ingestor uses `google.golang.org/protobuf/proto`.

**Acceptance**: integration test publishing a malformed message asserts it lands in the DLQ after N attempts
instead of looping.

---

## P5 — Wire the unwired

### P5-1 · Wire alerting (S8, B3)

`grep -rn "alerts\.\|Dispatch(" --include='*.go' apps/ packages/ | grep -v _test.go` returns **only the
declaration**. 425 LOC of dispatcher + notifiers, covered by ~1,100 lines of passing tests, is never reached
by the production binary. `docs/todos/04-alerting-and-notification-integrations.md` is therefore still fully
open despite the packages existing.

1. `NewProcessorService` (`processor_service.go:24`) constructs only `store`, `indexer`, `degradation`.
   Construct `alerts.NewDispatcher(db)` too.
2. Call `Dispatch(ctx, issueID, projectID, errorClass, message)` from `processEventInternal` after the issue
   upsert — for new issues and regressions, not every occurrence.
3. `Dispatcher.sendAlert` (line 183) ends in `log.Printf("ALERT: ...")` and is connected to **neither**
   notifier. Wire `notifiers.NewEmailWorker` / `NewTelegramWorker` (both take a config struct) behind the
   dispatcher's existing `SetSenderForTest` seam — promote it to a real, non-test `SetSender`.
4. `loadConfigs` only refreshes on a 5-minute ticker with **no initial load**, so even once wired it would
   ignore every alert config for the first five minutes after boot. Load once at construction.
5. Wire `src/routes/settings/alerts/+page.svelte` and `api/alerts/+server.ts` to the same config table so the
   UI actually controls the dispatcher.

**Acceptance**: integration test — configure an alert, ingest a new error class, assert the notifier is
invoked (fake SMTP / captured sender) **within one event**, not five minutes.

### P5-2 · Resolve the duplicate validator (S8)

`apps/ingestor-go/validation/validator.go` `ValidatePayload` (123 LOC) is referenced **only** by
`tests/unit/ingestor_validation_test.go` — which does not compile (S1). The real ingest path uses
protovalidate. Two validators with divergent rules is exactly how S3 hid in plain sight.

**Decide and act**: either delete `ValidatePayload` and its test, or make it the single pre-proto validation
layer and delete the overlapping proto field rules. Do not keep both. Recommend: keep protovalidate as the
sole authority (it is generated from the shared contract) and delete `ValidatePayload`.

---

## P6 — Dashboard functional completeness

Beyond the compile fixes in P1-2 and the authorization work in P3-4:

### P6-1 · Audit every API route against a real request

Eleven `+server.ts` routes exist. Six of them do not currently compile. For **each**, verify: authentication,
tenant scoping (does it filter by the caller's org?), RBAC, input validation (zod is a dependency — is it
used?), and error shape.

Routes: `api/alerts`, `api/cron/retention`, `api/issues/[issueId]/relations`,
`api/organizations`, `api/organizations/switch`, `api/organizations/[orgId]/invitations`,
`api/organizations/[orgId]/keys` (+ `[keyId]`, `[keyId]/rotate`), `api/projects/[projectId]/issues`,
`.../issues/batch`.

Particular attention to `api/cron/retention` — an unauthenticated destructive endpoint would be severe. Verify
it requires a shared secret.

### P6-2 · Audit every page against a real session

Ten `+page.svelte` files. Note the **duplicated issue detail route**: `issues/[id]/` and
`[orgSlug]/projects/[projectId]/issues/[issueId]/`. Decide which is canonical (the org-scoped one, per D6) and
delete or redirect the other — an org-less route is a tenancy leak waiting to happen. Cross-check against
**B2** (reserved path collision guard for dynamic slug routing) that `[orgSlug]` still guards reserved paths
like `api`, `auth`, `settings`, `search`.

### P6-3 · Verify the Drizzle schema matches the goose migrations

The dashboard's Drizzle schema and `packages/db-migrations/migrations/*.sql` are two hand-maintained
descriptions of one database with nothing checking they agree — the same B5 pattern that produced S4. The
`issues.releaseVersion` error found in P1-2 is one instance already.

Add a test that introspects the migrated database and asserts every Drizzle-declared table/column exists with
a compatible type.

**Acceptance**: `pnpm test` includes a schema-parity test that fails when a migration adds a column Drizzle
does not know about.

---

## P7 — The E2E proof harness

*This is the actual deliverable of the plan. Everything above is a prerequisite.*

Create `tests/e2e/` (root module, enabled by `go.work`) that boots the **full compose stack** — postgres,
redis, NATS, migrate, ingestor, processor, dashboard — and drives each use case through the **real published
SDK** and the **real HTTP surfaces**. No mocks, no in-process shortcuts, no skips.

### Use-case coverage matrix

Every row must have a named test. A feature is not "working" until its row is green.

| # | Feature | Use case | Asserted end state |
|---|---|---|---|
| U1 | 001 ingest | SDK captures an error → `/ingest` | HTTP 202; one `issues` row; one `error_occurrences` row |
| U2 | 001 ingest | Batch of 10 via `/ingest/batch` | 10 occurrences; response body reports 10 ingested |
| U3 | 001 ingest | Batch with 3 valid + 2 invalid | body names the 2 failures; 3 occurrences (P2-5) |
| U4 | 001 ingest | Oversized message (>10000) | 400 with a field-level error — **and a 9999-char message is 202** (the S3 regression test) |
| U5 | 001 ingest | Missing `platform` | 400 naming `platform` |
| U6 | grouping | Same error class, 3 distinct stacktraces, no `in_app` | **3** distinct issues (S11) |
| U7 | grouping | Same error 100× | 1 issue with `count=100` |
| U8 | 007 SDK | Real `packages/sdk-go` client end-to-end | occurrence has non-empty `message`, populated `metadata`, `platform="go"`, correct `release_version` (S4) |
| U9 | 007 SDK | SDK receives a 4xx | error surfaced via `Debug`/`OnError`, not silent (S4) |
| U10 | 007 SDK | `Flush(timeout)` before exit | no events lost |
| U11 | 006 lifecycle | Resolve an issue, then the same error recurs in a **newer** release | `regression_status` set, `regression_count=1`, `last_regressed_at` set, `issue_activity` 'regressed' row (S5) |
| U12 | 006 lifecycle | Same error recurs in an **older** release | **no** regression recorded |
| U13 | 006 lifecycle | Issue relations (link/unlink) | relation rows via the API |
| U14 | 008 keys | Create key via dashboard API → ingest with it | 202 |
| U15 | 008 keys | Revoke → ingest | **401 within 1s** (S7) |
| U16 | 008 keys | Rotate → old key | 401 (S7) |
| U17 | 008 keys | Expired key (`expires_at` in past) | 401 (S7) |
| U18 | **tenancy** | Key for project A, body names project B | **403**, zero rows in B (S6) — *the security regression test* |
| U19 | 008 ratelimit | 200 concurrent, limit 100 | ≤100 accepted, 429s carry `Retry-After` (S10) |
| U20 | 008 ratelimit | Redis unreachable at boot | service **refuses to start** (or logs an explicit opt-out), never silently fails open (S10) |
| U21 | 005 orgs | Two orgs, same project name | no cross-visibility; both resolvable (S6) |
| U22 | 005 orgs | Invite → accept → role applies | member row + permitted actions |
| U23 | RBAC | Each of `owner/admin/engineer/developer/support/viewer` against each protected route | matches the matrix; **no role errors as unknown** (P3-4) |
| U24 | auth | Magic-link sign-in (D2) | session established |
| U25 | auth | Google sign-in with domain restriction unset | permitted (P3-4) |
| U26 | alerting | New issue with an alert configured | notifier invoked within one event (S8) |
| U27 | alerting | Alert config created in UI | takes effect without a 5-minute wait (S8) |
| U28 | resilience | Postgres down mid-stream, then restored | exactly-N occurrences, no loss, no duplicates (S9) |
| U29 | resilience | Malformed NATS message | DLQ after N deliveries, no infinite redelivery (P4-4) |
| U30 | 004 migrations | Fresh DB → `up` → `down` → `up` | schema round-trips; goose version table correct |
| U31 | dashboard | Issue list / detail / search render for a seeded org | correct counts, correct tenant scoping |
| U32 | retention | Cron retention endpoint | deletes only beyond the window; **requires auth** |

### Harness requirements

- **Skips are failures** under `SENTINEL_E2E=1` (P0-4).
- Each test seeds and tears down its own org/project; no shared mutable state.
- Poll with a deadline for the async NATS hop rather than `sleep` — flaky waits are how a suite gets disabled.
- Run in CI on every push. Target: full suite < 10 minutes.

---

## P8 — Ops hardening and memory reconciliation

### P8-1 · Remaining hardening

| Item | Location | Fix |
|---|---|---|
| **Migration CLI prints the DB password to stdout on every invocation** — `sanitizeDSN` only matches `password=...` key-value form, but this project uses `postgres://user:pass@host/db` URLs | `packages/db-migrations/cmd/migrate/main.go` | Parse as URL and redact userinfo. Treat as a credential-leak fix, not a cosmetic one |
| Writes an all-zero-UUID row into `audit_logs` on **every boot** | `ProcessorService.VerifyAuditLogTable` | Use a transaction that rolls back, or `SELECT` against `information_schema` |
| No request body limit on the only externally exposed service | `handleBatchIngest` | P2-5 |
| No structured logging, no metrics, no tracing anywhere | all services | Add `slog` with request IDs; `/metrics` for ingest rate, 4xx/5xx, NATS lag, processor latency. **Note: this repo had zero observability into its own 100% rejection rate — that is a first-class finding, not a nice-to-have** |
| No graceful shutdown drain in the processor | `apps/processor-go/main.go` | On SIGTERM, stop pulling and finish in-flight before exit |

### P8-2 · Reconcile documentation with reality

Once each phase lands:

- **Update `VERIFIED_STATE.md`** — this file is the project's most valuable asset now. Move each S-item to a
  "Resolved (verified on <date> by <command>)" section as it is fixed. Never delete the history; the record of
  *how* it broke is what prevents recurrence.
- **`docs/memory/DECISIONS.md`**: D1 (degradation buffer) must be rewritten — S9 shows the mechanism it
  credits currently *causes* data loss. D9 (hierarchical rate limiting) must either be implemented or amended
  (P3-3). Add a decision for the fingerprint change (P4-3) and the canonical-schema choice (P2-1).
- **`docs/memory/BUGS.md`**: B1 is marked `superseded-by-B7`; confirm and add resolution notes to B3–B7.
- **`docs/memory/ARCHITECTURE.md`**: A2 must be updated for the `go.work` change (P0-3), including the
  deliberate `GOWORK=off` divergence in CI.
- **`specs/*/spec.md`**: 006, 007, 008 are marked Completed while broken; 001, 004, 005 say Draft while
  partially shipped. **Neither label means anything today.** Replace the binary status with a link to the
  relevant `VERIFIED_STATE.md` entry, or add a "Verified" field distinct from "Merged".
- **`CLAUDE.md`**: shrink the "Known-broken" table as items resolve. Keep the "Working conventions" section —
  it is what prevents the next occurrence.
- **`README.md`**: currently 72 bytes. Write real setup instructions and verify them by following them on a
  clean clone.

### P8-3 · Institutionalize the lesson

The single generalizable finding from the review (B3) is: **passing package tests coexisted with entirely
unreachable code across four separate features.** Add to the definition-of-done in
`.specify/memory/workflow.md`:

> A feature is not complete until a test exercises it through its **production entry point** — an HTTP route
> or `main()` — not merely through its package API.

---

## 3. Sequencing summary

| Phase | Blocking? | Can parallelise? | Gate to advance |
|---|---|---|---|
| **P0** Make failure visible | **Yes — first** | P0-1..6 amongst themselves | CI runs; compose boots healthy |
| **P1** Restore the build | **Yes — second** | P1-1 ∥ P1-2 | G1, G2, G5, G6 green |
| **P2** Wire contract | Blocks P3-1, P7 | P2-1→2→3 serial; P2-4,5 after | Contract test green; U4/U5/U8 pass |
| **P3** Security & tenancy | P3-1 blocks U18 | P3-1..4 parallel after P2-1 | U14–U21, U23 pass |
| **P4** Processor correctness | Blocks U11, U28 | P4-1..4 parallel | U6, U11, U12, U28, U29 pass |
| **P5** Wire the unwired | — | P5-1 ∥ P5-2 | U26, U27 pass |
| **P6** Dashboard | — | P6-1..3 parallel | U22–U25, U31, U32 pass |
| **P7** E2E harness | Last | Tests written per-feature alongside P2–P6 | **All 32 use cases green** |
| **P8** Hardening & docs | Continuous | — | G1–G11 green; docs match reality |

**If you can only do three things**, do these — they are the highest severity-to-effort ratio in the repo:

1. **P2-1's one-token fix** (`string.len` → `max_len`, `error_event.proto:40`) — unblocks 100% of ingest.
2. **P1-2's one-line fix** (escaped backticks, `issues.ts:202`) — unblocks the entire dashboard build.
3. **P3-1** (tenant scoping) — closes a cross-tenant write vulnerability.

But note that doing only these leaves **no gate**, and the review evidence is that this repository
regenerates these defects without one. P0-1 is the item that changes the trajectory.
