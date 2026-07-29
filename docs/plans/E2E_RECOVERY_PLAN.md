# E2E Recovery Plan — Making Sentinel Actually Work End-to-End

**Drafted**: 2026-07-28 · **Baseline commit**: `ad2f967` · **Evidence base**: [VERIFIED_STATE.md](../memory/VERIFIED_STATE.md)

> [!IMPORTANT]
> This plan assumes **nothing in `specs/` is true** unless [VERIFIED_STATE.md](../memory/VERIFIED_STATE.md)
> confirms it. Every work item below ends in a **command that must pass**. A task is not done when the code
> is written; it is done when its acceptance command passes on a clean checkout.

> [!WARNING]
> **State as of 2026-07-29, branch `chore/p0-p1-green-tree`, commit `cd84d17` (P0+P1) with a large staged,
> uncommitted change set on top (P2 + the P2b addendum below — 36 files, +2501/-254)**: the `ad2f967` baseline
> above no longer describes the tree, and neither does the 2026-07-28 revision of this banner. P0, P1, and P2
> have all executed. **The pipeline produces rows in `issues`/`error_occurrences` for the first time in the
> project's history** — before this change the database had zero rows, ever (S12). Gates G1–G8 pass (verified
> 2026-07-29 by running each command; see the gate table in §0). G9 does **not** pass yet (10 real failures out
> of 75 tests, current count — see §0). G10 cannot pass: `tests/e2e/` does not exist (P7 has not started). G11
> is unverified: nothing has been pushed from this branch, so "GitHub Actions green" has never actually been
> observed, only the equivalent commands run locally. Multiple agents working this plan concurrently have
> repeatedly found their assigned item's "current state" description already partially true, uncommitted, in
> the working tree before they started — do not trust a per-item "current state" paragraph below without
> re-verifying it against the tree in front of you first. Known corrections so far: **C1 is resolved**; the
> `proto` CI job description misstated what `buf` actually catches (see P0-1's acceptance note); C5's "skip
> silently" premise does not match `tests/integration/setup_test.go`'s actual `TestMain` behavior (see C5).
> **New hazard observed live during this pass**: the shared dev Postgres (`sentinel-postgres`, port 5432)
> is currently missing the `project_api_keys` table entirely (`\dt` confirms it), while `schema_migrations`
> still claims migration `1722000000` (which creates that table) is applied, and `issues`/`error_occurrences`
> still hold real rows from earlier proof work. This is a live instance of the "running the integration suite
> corrupts the shared dev database" hazard recorded below — it was already in this state when this pass began
> and this pass could not repair it (schema-repair statements were blocked by the permission system). Any
> item below that needed a live curl/API-key round trip against the dev stack could not be freshly verified
> for that reason and is marked accordingly; restoring the stack (`down` then `up` migration 1722000000, or a
> fresh `docker compose up -d --build --force-recreate` + migrate) is a prerequisite for the next pass.

---

## 0. Goal and definition of done

**Goal**: every feature the repo claims to have (001, 004, 005, 006, 007, 008, alerting) executes correctly
against a clean local stack and is protected by an automated gate that fails on regression.

The project is "working end-to-end" when **all eleven** of these hold on a fresh clone. **Status column
verified 2026-07-29** by actually running each command against this tree (staged, uncommitted P2/P2b on top
of `cd84d17`) — see the note after the table for evidence/detail on the non-green rows.

| # | Gate | Command | Status (2026-07-29) |
|---|---|---|---|
| G1 | Root module builds and vets clean | `rtk go build ./... && rtk go vet ./...` | ✅ PASS |
| G2 | Unit suite runs and passes | `rtk go test ./tests/unit/...` | ✅ PASS (253 assertions) |
| G3 | SDK module builds, vets, tests | `cd packages/sdk-go && rtk go test ./...` | ✅ PASS (23 tests, 4 packages) |
| G4 | Migrations module builds and tests | `cd packages/db-migrations && rtk go test ./...` | ✅ PASS |
| G5 | Dashboard builds | `cd apps/dashboard-web && pnpm build` | ✅ PASS |
| G6 | Dashboard typechecks with **0** errors | `pnpm check` | ✅ PASS (707 files, 0 errors, 2 warnings) |
| G7 | Dashboard tests run | `pnpm test` | ✅ PASS (5 files, 19 tests) |
| G8 | Full stack boots from compose, all healthy | `docker compose up -d && ./scripts/wait-healthy.sh` | ✅ PASS (stack was already up; `sentinel-postgres`/`redis`/`nats`/`ingestor`/`processor`/`dashboard` all healthy; `curl localhost:8080/health` → `{"status":"healthy"}`) |
| G9 | Integration suite passes, **skips are failures** | `SENTINEL_E2E=1 rtk go test ./tests/integration/... -count=1` | ❌ FAIL — **65 pass, 10 fail** (current count, do not quote an older one; full log in scratchpad, not committed). Three distinct root causes, none of them a regression from this pass's own changes: **(a)** `TestIngestAndProcess`, `TestSearchIndexing`, `TestAPIKeyAuthenticator_ValidKey` all fail `Expected status 200/202, got 401` — traced to source: these tests' `seedAuthProject`/project-seeding helpers still `INSERT INTO projects (name, api_key, api_key_hash) ...` (the **pre-008** single-column scheme), but `apps/ingestor-go/auth/apikey.go`'s `getAPIKeyData` now exclusively queries the 008 `project_api_keys` table — the seeded "valid" key was never written to the table the middleware actually reads, so it 401s. **This is a stale test fixture, not a production defect** — the fixture needs updating to seed `project_api_keys`, matching the same "test never migrated with the schema" shape as the `scripts/db/init.sql` gap below. **(b)** `TestSequentialMigrations` fails `expected: 0, actual: 3` on a post-migration `issues` row count — traced to source: earlier tests in the *same* `go test` process share one TestMain-provisioned Postgres container and leave rows behind; this is a test-isolation gap in the suite, unrelated to (a) or to the shared dev-DB hazard in the banner. **(c)** `TestStorePackage_UpsertIssue_InsertsNewIssue`/`..._DuplicateIncrementsCountAndUpdatesLastSeen`/`..._DuplicateWithOlderLastSeenKeepsGreatest`/`TestStorePackage_InsertOccurrence_InsertsRow`/`TestStorePackage_GetIssueIDByFingerprint_ReturnsID`/`TestProcessorService_ProcessEvent_UpsertIssueFailsWhenIssuesTableMissing` all fail `violates check constraint "check_status"` — this **is** the `scripts/db/init.sql` third-schema gap (see the P2b addendum below): `store.go` inserts `status='unresolved'`, `init.sql`'s schema only allows `open/resolved/ignored`. |
| G10 | The SDK→dashboard journey works with the real published SDK | `rtk go test ./tests/e2e/... -count=1` | ❌ NOT BUILT — `tests/e2e/` does not exist. P7 has not started. |
| G11 | All of G1–G10 run on push | GitHub Actions green | ⚪ UNVERIFIED — `.github/workflows/ci.yml` exists (P0-1) and its jobs match G1–G8 by inspection, but nothing has been pushed from this branch, so no run has ever actually been observed. It also cannot be green today regardless, since G9/G10 are red. |

`tests/contract/` (P2-4's deliverable, gates the wire-contract regression class) also passes:
`GOWORK= rtk go test -tags=contract ./tests/contract/... -count=1` → ok, 4 tests. This isn't one of the
numbered G-gates but is load-bearing for P2 and is exercised by CI's `contract` job.

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
                                    │                                │      ──> P2b (unplanned: S12-S17,
                                    │                                │           R1, PII, uniqueness —
                                    │                                │           found while doing P2)
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
| `proto` | `buf lint`, `buf breaking --against '.git#branch=main'`, `buf generate` + `git diff --exit-code gen/` | **Correction (2026-07-28, historical): did NOT catch S3's class of bug.** `buf lint` is naming/style only; `buf breaking` compares descriptor *structure*, not constraint semantics, and *at the time this was written* **passed with `string.len = 10000` still in place**. **S3 is now fixed** (the redundant field rule was deleted — see the P2-1 acceptance note and `VERIFIED_STATE.md`'s S3 entry) — this row describes why `buf`'s own checks didn't catch it, which remains true as a description of `buf`'s capabilities, not a claim that the bug is still present. `buf generate` + diff only proves `gen/` matches whatever the `.proto` says — a confidently-wrong constraint regenerates cleanly. This job is still worth having (it catches drift and naming/breaking-change classes of bug) but it would not have caught S3 on its own. |
| `contract` | `go test -tags=contract ./tests/contract/... -count=1` (see P0-3, P2-4) | Workspace mode — the **only** job without `GOWORK=off`. Files there **must** carry `//go:build contract` |

**Acceptance**: **correction (2026-07-28)** — only P2-4's contract test (`tests/contract/`, running real `protovalidate` against a real payload) and U4 in the P7 e2e harness catch this bug class. A PR that reintroduces the `string.len = 10000` bug fails the `contract` job (once P2-4 lands); it does **not** fail `proto`, which is structurally incapable of catching a semantically-wrong-but-well-formed CEL/field constraint. **P2-4 has since landed and passes** (`tests/contract/` — verified 2026-07-29: `ok, 4 tests`), so this protection is now actually in place, not just planned.

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

> [!NOTE]
> **Status (2026-07-29): DONE, verified by code inspection.** `apps/ingestor-go/main.go` now defines
> `maxBatchSize = 500`, wraps both single and batch bodies in `http.MaxBytesReader`, rejects a batch over the
> cap with `400`, and returns `{ingested, failed, errors[]}` shaped JSON. `packages/sdk-go/transport.go`
> reads `Ingested`/`Failed` off a 2xx response, logs the failed count under `Debug`, and counts partial
> failures in the dropped-event counter (`TestSendBatchSurfacesPartialFailureOn2xx`,
> `TestSendBatchIgnoresUnparsableBodyOn2xx` both pass in `packages/sdk-go`'s test run). **Not independently
> re-run against the live ingestor** in this pass — the dev-DB hazard in the banner blocked a fresh
> API-key round trip — but the mock-server-level SDK tests and the code path are consistent with the brief's
> S15 entry, which reports this landed.

---

## P2b — Newly-discovered defects fixed while implementing P2 (not in the original plan)

*Added 2026-07-29. This phase did not exist when the plan was drafted — the agents executing P2 found seven
additional, previously-undocumented defects blocking the pipeline from ever writing a row, fixed them, and
recorded decision **D10** (`docs/memory/DECISIONS.md`) for the NATS delivery semantics introduced along the
way. Listed here so a future reader can see where this work came from instead of finding orphaned code.*

### P2b-1 · S12 — two contradictory `issues.status` CHECK constraints made every insert fail

`1716508800_init.sql:22`'s inline, auto-named `CHECK (status IN ('open','resolved','ignored'))` was never
dropped by `1721900000`'s explicitly-named `check_status CHECK (status IN ('unresolved','resolved','ignored'))`
— both applied, and Postgres enforces the intersection: `{resolved, ignored}`. The processor writes
`'unresolved'`. **Net effect: the `issues` table has been uninsertable since the constraint was added — the
database had zero rows, ever, for the life of the project**, regardless of every other fix in this plan.
Fixed: drop the first constraint so exactly one (the named, `unresolved`-permitting one) remains.

**Verified 2026-07-29**: `podman exec sentinel-postgres psql -U sentinel -d sentinel -c "\d issues"` on the
live dev stack shows a single `check_status` constraint. (`scripts/db/init.sql`, a *third*, never-migrated
copy of the schema, still has the pre-fix constraint — see "Newly-discovered gaps" below; it is why 6 of
G9's current 10 integration failures are `violates check constraint` errors.)

### P2b-2 · S13 — poison-message livelock; no MaxDeliver, no DLQ

A bare `msg.Nak()` with no delivery cap meant one permanently-malformed message redelivered forever and, being
re-fetched ahead of newer messages every cycle, starved the whole pipeline: ~510 unique events produced 5,874
processing attempts, and no newly-published event was observed reaching Postgres. Fixed in
`packages/shared-go/nats/subscriber.go`: `MaxDeliver` (default 5) with `NakWithDelay` backoff
(1s/5s/15s/30s/60s), and exhausted/permanently-failed messages are published to a DLQ subject
(`<STREAM>_DLQ`) instead of being redelivered indefinitely. Full rationale and the tradeoffs recorded in
**D10** in `docs/memory/DECISIONS.md` — do not duplicate that record here.

**Gap this introduces, not yet closed**: nothing consumes the DLQ. See "Newly-discovered gaps" below.

### P2b-3 · S14 — a second instance of the S3 bug class: unbounded fields against bounded columns

`platform`/`environment` (VARCHAR(50)), `error_class` (255), `trace_id`/`span_id` (64), and
`release_version` (100, added by P2-1 itself) had no length rule in the proto. A 5000-char `error_class`
returned `202` and then failed to store — the same "accepted then silently lost" shape as S3, just on a
different field. Fixed: CEL rules added matching every column width.

### P2b-4 · S16 — the SDK's single `ProjectKey` field held the secret AND was sent as the project selector

Before this fix, `Config.ProjectKey` held the API key and was also sent as the body's `project_key`, which
the server resolved against `projects.name` — so **every SDK event was `202`'d and then permanently
dead-lettered, silently**. Split into `Config.APIKey` (secret, `X-API-Key` header only) and
`Config.ProjectKey` (project's unique name, body). `Config.Validate()` detects a value with an API-key prefix
in the wrong field and fails loudly. See the root `README.md`'s new "Using the Go SDK" section for the
user-facing distinction — this is a breaking wire/config change for every existing SDK caller.

### P2b-5 · S17 — ingestor hot-spin: 55.8% idle CPU flooding the log pipeline

`Fetch` received a deadline-less context, returned instantly on no messages, and the loop spun with no
backoff, consuming 55.8% CPU while idle and logging hard enough to suppress the whole pod's output. Fixed
with a per-fetch deadline derived from `BatchWait` plus rate-limited drop logging. Measured after: 0.08% CPU.

### P2b-6 · R1 — introducing typed context keys silently disabled rate limiting for 100% of requests

Self-inflicted by this same body of work: replacing bare string context keys (`"project_key"`,
`"api_key_hash"`, `"rate_limit_rpm"`) with a private `ctxKey` type in `auth/context.go` was correct, but
`middleware/ratelimit.go` still read the old bare-string keys — `context.Value` compares the *dynamic type*
of the key, so the type-mismatched assertion silently returned `ok == false` on every request, and the
middleware's `if !ok { next.ServeHTTP(...); return }` passed everything through uncounted. Three test files
had hand-injected the bare string keys directly, so nothing caught it. Fixed: `ratelimit.go` now reads via
`auth.APIKeyHashFromContext`/`auth.RateLimitRPMFromContext`; tests build context via `auth.WithIdentity`.
**Verified**: 12 sequential requests against a limit of 5 → `202 202 202 202 202 429 429 429 429 429 429 429`,
with `Retry-After: 60`, `X-RateLimit-Limit: 5`, `X-RateLimit-Remaining: 0`. This is the origin of the
"never read auth context by string literal" convention now in `CLAUDE.md`.

### P2b-7 · PII scrubbers were incomplete, and the `context`→`metadata` rename made a gap live

Both the SDK's and the server's PII scrubbers were missing common sensitive keys. The `context`→`metadata`
rename (P2-3/S4) made this live for the first time — data that previously never arrived (S4) now does, PII
gap included. Fixed: SDK `defaultPIIKeys` extended (`pass`, `passwd`, `bearer`, `card_number`, `cvv`,
`social_security`); the server-side masker switched from **exact** key equality to **substring** matching —
it previously let `user_password` and `x-api-key` through untouched because neither equals `password` or
`api_key` exactly.

### P2b-8 · Project name uniqueness was never enforced

`projects.name` had no uniqueness constraint at all, so two organizations could hold same-named projects and
`GetProjectByKey` picked arbitrarily. Added `CREATE UNIQUE INDEX idx_projects_org_name ON projects
(organization_id, name)` — organization-scoped, not global, so same-named projects across different orgs
remain allowed (multi-tenancy requirement) while duplicates within one org are rejected.

**Verified 2026-07-29** (live dev stack): `\d projects` shows `idx_projects_org_name UNIQUE, btree
(organization_id, name)`.

### P2b-9 · Org-wide key project resolution order, now explicit

For an organization-wide key, `X-Project-Key` header takes precedence over the body's `project_key` (header
wins if both are present and disagree); the body is the fallback. Both resolve **within the authenticated
key's organization only** — this is the mechanism P3-1 depends on. A project-scoped key whose body names a
different project is rejected with `403` regardless of which resolution path is used.

### P2b addendum · Newly-discovered gaps with no phase of their own yet

*Found while doing the P2b work above; none of these were anticipated by the original plan and none are
fully covered by an existing phase item, so they are recorded here rather than silently left to be
rediscovered. Verified 2026-07-29 unless noted.*

- **The DLQ (P2b-2/S13) has a producer and no consumer.** Nothing drains it, replays it, or alerts on its
  depth — `Subscriber.DLQPublishFailures()` exists but isn't exported to any metric, and
  `grep -rn "_DLQ\|dlq.*[Ss]ubscri" apps/ packages/` finds only the *publishing* side
  (`packages/shared-go/nats/subscriber.go`, `apps/processor-go/main.go`'s `DLQSubject`/`DLQStream` config).
  A dead-lettered event is durably stored but invisible to the product — an error tracker silently parking
  its own errors. This gap is already recorded in full, with a remediation sketch, in **D10**
  (`docs/memory/DECISIONS.md`) — do not duplicate that text here, just note it needs a phase (candidate: a
  new P5-3, since it's the same "wire the unwired" shape as P5-1's alerting gap).
- **`scripts/db/init.sql` is a *third*, independently hand-maintained schema**, frozen at the pre-S12 state
  (`status VARCHAR(20) ... CHECK (status IN ('open', 'resolved', 'ignored'))`, `scripts/db/init.sql:26`) —
  it never received the S12 fix that the two goose migrations did. `tests/integration/setup_test.go`'s
  `runMigrations` applies **this file**, not the goose migration chain, to provision its testcontainers
  Postgres. Since `store.go` inserts `status='unresolved'`, every `TestStorePackage_*` test that inserts an
  issue fails against it — this is 6 of G9's current 10 integration failures (see the G9 row above). This is
  the same "N hand-maintained copies of one schema, nothing checks they agree" pattern as P6-3 (Drizzle vs.
  goose) — a third copy nobody had cross-checked. Fix direction: either delete `init.sql` and point
  `setup_test.go` at the real goose migration chain (preferred — it's what P0-5's compose `migrate` service
  and every other consumer use), or bring it current and add it to whatever future check enforces P6-3's
  schema-parity requirement.
- **Two dashboard API routes 500 on every request due to `schema.ts` drift from the goose migrations** (the
  same B5 pattern P6-3 already flags, now with concrete instances):
  - `api/issues/[issueId]/relations/+server.ts` — `schema.ts`'s `issueRelations` table is missing the
    columns the live `issue_relations` table declares `NOT NULL`: `created_by_type varchar(20)` and
    `created_by varchar(255)` (verified live: `\d issue_relations`). Any insert through this route fails.
  - `api/projects/[projectId]/issues/batch/+server.ts` (via `src/lib/db/queries/issues.ts`) — two drifts:
    `issueActivity.metadata jsonb` has no such column in the live table (which has `old_value`/`new_value`
    instead — verified live: `\d issue_activity`), and `issues.ts:57`/`:79` write the literal
    `'status_change'` where the live `issue_activity_event_type_check` constraint requires `'status_changed'`.
  - Both are already named in the "Known-broken" table of `CLAUDE.md`; P6-3 (schema-parity test) is the
    right home for the durable fix, but the two 500s themselves are quick, independent one-line-ish fixes
    that don't need to wait for the parity-test infrastructure.
- **`S7` (key revocation/expiry) remains fully open** — see P3-2 above, which already covers it; re-verified
  unchanged on 2026-07-29 (`getAPIKeyData` still has no `expires_at` predicate; `rotateApiKey` still leaves
  `status='active'`). Listed here only so a reader scanning "what's new" doesn't conclude it was fixed
  alongside the other S-items in this phase — it wasn't.
- **`issue_activity.old_value` is left `NULL`** where spec 006 pairs `old_value`/`new_value` to record a
  state transition — `src/lib/db/queries/issues.ts`'s activity-insert call sites populate `new_value` but
  never `old_value`. Not yet independently re-verified against the live schema beyond confirming the column
  exists and is nullable.
- **`event_id` is emitted by the SDK and has no server-side destination.** Grepped for in the ingestor's
  payload handling and the proto; not present. Low severity, but it's dead weight on the wire and a
  candidate for either wiring up (idempotency key?) or removing from the SDK.
- **Three `tests/integration` fixtures predate the 008 API-key-management migration and now fail against
  it** (found while diagnosing G9, 2026-07-29): `TestIngestAndProcess`, `TestSearchIndexing`, and
  `TestAPIKeyAuthenticator_ValidKey` all seed their "valid" key via `INSERT INTO projects (name, api_key,
  api_key_hash) ...` — the single-column scheme from before 008 — but `apps/ingestor-go/auth/apikey.go`'s
  `getAPIKeyData` now reads exclusively from `project_api_keys`, so the seeded key is never found and the
  request 401s. This is a test-fixture staleness bug, not a production regression, but it means these three
  tests currently prove nothing and count as false-negative noise in G9's failure count. Fix: update the
  seeding helpers (`seedAuthProject` and its siblings) to insert into `project_api_keys` instead. This is the
  same "the schema moved, the test didn't" shape as `scripts/db/init.sql` above and P6-3 — a fourth instance
  of the pattern.

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

Every row must have a named test. A feature is not "working" until its row is green. **P7 (the harness that
exercises these against the real stack via the real SDK) has not been started — `tests/e2e/` does not exist.**
The Status column below is therefore never "a named test is green" for most rows; it records what was
established by other means (existing unit/integration/contract tests, or direct inspection/live querying) as
of 2026-07-29, and says so explicitly so it isn't mistaken for a P7 result.

| # | Feature | Use case | Asserted end state | Status (2026-07-29) |
|---|---|---|---|---|
| U1 | 001 ingest | SDK captures an error → `/ingest` | HTTP 202; one `issues` row; one `error_occurrences` row | ✅ **PROVEN** (per brief) — first time ever; database previously had zero rows (S12) |
| U2 | 001 ingest | Batch of 10 via `/ingest/batch` | 10 occurrences; response body reports 10 ingested | ⚪ Unverified live. `handleBatchIngest`'s `{ingested, failed, errors[]}` shape and the 500-item cap exist in code (P2b addendum); no named test drives 10 occurrences through to Postgres and counts them. Blocked from a fresh live check by the dev-DB hazard (banner). |
| U3 | 001 ingest | Batch with 3 valid + 2 invalid | body names the 2 failures; 3 occurrences (P2-5) | 🟡 Partial — `packages/sdk-go`'s `TestSendBatchSurfacesPartialFailureOn2xx` proves the SDK-side parsing of a partial-failure response against a mock server; no test proves the *server* actually produces that shape end to end with 3 real occurrences landing. |
| U4 | 001 ingest | Oversized message (>10000) | 400 with a field-level error — **and a 9999-char message is 202** (the S3 regression test) | ✅ **PROVEN** (per brief) — len sweep 0/1/24/9999/10000 → 202, 10001 → 400 |
| U5 | 001 ingest | Missing `platform` | 400 naming `platform` | 🟡 Partial — `tests/contract/sdk_ingestor_test.go`'s `TestSDK_RejectsWithoutPlatform_WouldFailValidation` proves this at the protovalidate-descriptor level; not proven against a live `/ingest` request. |
| U6 | grouping | Same error class, 3 distinct stacktraces, no `in_app` | **3** distinct issues (S11) | ✅ **PROVEN** (per brief, S11's proof is this exact assertion) |
| U7 | grouping | Same error 100× | 1 issue with `count=100` | 🟡 Blocked — the closest test, `TestStorePackage_UpsertIssue_DuplicateIncrementsCountAndUpdatesLastSeen`, is currently **failing** (see G9), not because the count logic is wrong but because the third schema (`scripts/db/init.sql`, see below) rejects the `status='unresolved'` insert before the count logic is ever reached. |
| U8 | 007 SDK | Real `packages/sdk-go` client end-to-end | occurrence has non-empty `message`, populated `metadata`, `platform="go"`, correct `release_version` (S4) | 🟡 Partial — `packages/sdk-go`'s `TestGoSDKEndToEndPipeline` and `TestNewEventFieldsMatchWireContract` prove this against an `httptest` mock server (all pass); not yet proven against the real ingestor + Postgres, which is P7/U8's actual bar. |
| U9 | 007 SDK | SDK receives a 4xx | error surfaced via `Debug`/`OnError`, not silent (S4) | ✅ Component-level PASS — `TestSendBatchDropsOn4xxWithoutRetry` (mock server); not yet run against the real ingestor. |
| U10 | 007 SDK | `Flush(timeout)` before exit | no events lost | ⚪ Unverified — no test found asserting this specific property. |
| U11 | 006 lifecycle | Resolve an issue, then the same error recurs in a **newer** release | `regression_status` set, `regression_count=1`, `last_regressed_at` set, `issue_activity` 'regressed' row (S5) | ✅ **PROVEN** (per brief) |
| U12 | 006 lifecycle | Same error recurs in an **older** release | **no** regression recorded | ⚪ Unverified — no named test found. |
| U13 | 006 lifecycle | Issue relations (link/unlink) | relation rows via the API | ❌ Blocked — `api/issues/[issueId]/relations/+server.ts` uses the `issueRelations` Drizzle table, whose TS definition is missing the DB's NOT-NULL `created_by_type`/`created_by` columns (see "Newly-discovered gaps"); every insert through this route 500s. |
| U14 | 008 keys | Create key via dashboard API → ingest with it | 202 | ❌ Blocked — `api/organizations/[orgId]/keys/+server.ts` is still a **mock** returning hardcoded fixtures with `// TODO: implement RBAC check` (verified 2026-07-29, unchanged from the original finding); it does not write a real key, so nothing downstream can be tested through it. P3-4 has not landed. |
| U15 | 008 keys | Revoke → ingest | **401 within 1s** (S7) | ❌ Blocked — S7 confirmed still open (`getAPIKeyData` still filters on `status` only; `apps/ingestor-go/auth/apikey.go` has no `expires_at` check, verified 2026-07-29) |
| U16 | 008 keys | Rotate → old key | 401 (S7) | ❌ Blocked — same S7; `rotateApiKey` in `apps/dashboard-web/src/lib/db/queries/apikeys.ts` still sets only `expires_at`, not `status` (verified 2026-07-29) |
| U17 | 008 keys | Expired key (`expires_at` in past) | 401 (S7) | ❌ Blocked — same S7 |
| U18 | **tenancy** | Key for project A, body names project B | **403**, zero rows in B (S6) — *the security regression test* | 🟡 The underlying behavior is proven correct (per brief, manually exercised). **But no automated test encodes it** — `grep -rn "403\|StatusForbidden" tests/integration/*.go` returns nothing (verified 2026-07-29). P3-1's own acceptance criterion ("this test must exist before the fix is called done") is therefore not met; the fix could regress silently. |
| U19 | 008 ratelimit | 200 concurrent, limit 100 | ≤100 accepted, 429s carry `Retry-After` (S10) | 🟡 Partial — R1's fix makes *sequential* limiting correct (proven: 12 requests/limit 5 → 5×202 then 429s with correct headers, per brief). The *concurrent* case this row actually asks about is still at risk: `middleware/ratelimit.go` is unchanged 4-round-trip `ZRemRangeByScore`/`ZCard`/decide/`ZAdd` (verified 2026-07-29, S10 still open) — under real concurrency this can still overshoot. |
| U20 | 008 ratelimit | Redis unreachable at boot | service **refuses to start** (or logs an explicit opt-out), never silently fails open (S10) | ❌ Not done — `apps/ingestor-go/main.go:103` is still `redisClient, _ := redis.NewClient(...)`, discarding the error (verified 2026-07-29); `ratelimit.go:44` still has `if rl.client == nil { next.ServeHTTP(...); return }`, an unconditional fail-open regardless of `RATELIMIT_STRICT_MODE`. |
| U21 | 005 orgs | Two orgs, same project name | no cross-visibility; both resolvable (S6) | ✅ Structurally verified — live `\d projects` on the dev DB shows `idx_projects_org_name UNIQUE, btree (organization_id, name)` (org-scoped, not global) (verified 2026-07-29). No named test drives the full "both resolvable, no cross-visibility" assertion end to end. |
| U22 | 005 orgs | Invite → accept → role applies | member row + permitted actions | ⚪ Unverified — needs a live session; out of reach without a browser-driven check. |
| U23 | RBAC | Each of `owner/admin/engineer/developer/support/viewer` against each protected route | matches the matrix; **no role errors as unknown** (P3-4) | ❌ Not done — `src/lib/rbac.ts` still declares only `'admin' \| 'developer' \| 'viewer'` (verified 2026-07-29, unchanged); `owner`/`engineer`/`support` (real DB enum values) are still unknown to it. |
| U24 | auth | Magic-link sign-in (D2) | session established | ⚪ Unverified — needs a live browser session; not attempted this pass. |
| U25 | auth | Google sign-in with domain restriction unset | permitted (P3-4) | ❌ Not done — `src/lib/server/auth-config.ts:10` still hardcodes `const ALLOWED_EMAIL_DOMAIN = 'company.com'` (verified 2026-07-29, unchanged; not env-driven, so "unset" isn't even expressible) |
| U26 | alerting | New issue with an alert configured | notifier invoked within one event (S8) | ❌ Blocked — S8 confirmed still open: `NewProcessorService` (`apps/processor-go/service/processor_service.go`) constructs only `store`, `indexer`, `degradation` — no `alerts.NewDispatcher` (verified 2026-07-29) |
| U27 | alerting | Alert config created in UI | takes effect without a 5-minute wait (S8) | ❌ Blocked — same S8; the dispatcher isn't wired at all, so "takes effect" has no meaning yet |
| U28 | resilience | Postgres down mid-stream, then restored | exactly-N occurrences, no loss, no duplicates (S9) | ❌ Blocked — S9 confirmed **unchanged** by direct code read (2026-07-29): `GracefulDegradation.CheckAndBuffer` still returns a single bool that is `true` both when the DB is healthy and when the event was successfully buffered, and `false` (silently ACKed and lost) when the buffer is full — the exact inversion described in the original finding. `ProcessorService.ProcessEvent` still does `if !s.degradation.CheckAndBuffer(...) { return nil }`. |
| U29 | resilience | Malformed NATS message | DLQ after N deliveries, no infinite redelivery (P4-4) | 🟡 Partial — the mechanism now exists (S13/P2b-2: `MaxDeliver`, backoff, DLQ publish) and was measured once (`Redelivered=0` after the fix, per brief). No named test in `tests/integration` or `packages/shared-go` asserts "N attempts then DLQ" as a repeatable check (`TestNatsPackageSubscriberNakRedelivers` exists but predates/doesn't name this specific assertion). |
| U30 | 004 migrations | Fresh DB → `up` → `down` → `up` | schema round-trips; goose version table correct | ✅ **PROVEN** (per brief) — up → down×5 → up on a throwaway DB |
| U31 | dashboard | Issue list / detail / search render for a seeded org | correct counts, correct tenant scoping | ⚪ Unverified — needs a live browser session against a seeded org; not attempted this pass. Note `pnpm build`/`pnpm check`/`pnpm test` all pass (G5–G7), which proves the code compiles and its unit tests pass, not that these pages render correctly against real data. |
| U32 | retention | Cron retention endpoint | deletes only beyond the window; **requires auth** | 🟡 Partial — `api/cron/retention/+server.ts` does require a matching `x-cron-secret` header against `env.CRON_SECRET`, returning 401 on a missing/wrong secret (verified 2026-07-29 by reading the route) — the auth half looks done. The "deletes only beyond the window" half was not independently verified. |

**Legend**: ✅ proven by a command that was actually run · 🟡 partially verified (component-level test or
structural check, not the full row) · ❌ blocked, with the specific blocker named · ⚪ unverified, no attempt
made this pass (usually because it needs a live browser session, which was out of scope for this pass).

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
| **P2b** Newly-discovered wire/reliability defects (S12–S17, R1, PII, project uniqueness) | Blocks P2/P4/P7 correctness | Landed together, not designed to parallelise | See P2b's per-item verification notes above; not yet backed by dedicated automated tests for all nine items |
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
