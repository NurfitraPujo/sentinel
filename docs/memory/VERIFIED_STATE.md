# Verified State of the Codebase

Last verified: **2026-07-26** (commit `ad2f967`) by running builds, `go vet`, `go test`, `svelte-check`, `vite build`,
and targeted probe tests against the real packages.

> [!IMPORTANT]
> This file records what the code **actually does when executed**, as distinct from what `specs/`, `docs/todos/`,
> and `WORKLOG.md` claim was shipped. Those files describe *intent and merge events*; this file describes
> *observed behavior*. When the two disagree, this file wins until re-verified.
>
> Re-run the verification commands below before trusting any entry older than the current HEAD.

---

> [!NOTE]
> **Partial re-verification 2026-07-28**: S1 and S2 below were re-run and are now RESOLVED — see the
> [Resolved](#resolved) section for the commands and new baselines. S3–S11 have not been re-verified since
> 2026-07-26 and remain exactly as documented.

## How to verify (copy-paste)

```bash
rtk go build ./...                                    # root module: PASSES
rtk go vet ./...                                      # PASSES (S1 resolved 2026-07-28, see Resolved)
rtk go test ./tests/unit/... -count=1                  # PASSES, 251 assertions (S1 resolved, see Resolved)
cd apps/dashboard-web && pnpm exec vite build          # PASSES (S2 resolved, see Resolved)
cd apps/dashboard-web && pnpm check                    # 707 files, 0 errors (S2 resolved, see Resolved)
```

There was **no CI** when these findings were recorded — `.github/` contained only `copilot-instructions.md`, with no `.github/workflows/`
directory. Nothing in this repository is verified automatically on push. Every item below reached `main`
because no gate existed to catch it.

---

## Module layout (three separate Go modules)

| Module | Path | Notes |
|---|---|---|
| root | `github.com/NurfitraPujo/sentinel` | apps + shared-go + tests |
| SDK | `github.com/NurfitraPujo/sentinel/packages/sdk-go` | independent, `go 1.21` |
| migrations | `github.com/NurfitraPujo/sentinel/packages/db-migrations` | independent |

There are **no `replace` directives**. The root module consumes `db-migrations` at a *published pseudo-version*
(`v0.0.0-20260723071735-63e4b00a5f52`), so **local edits to `packages/db-migrations` are invisible to any
`GOWORK=off` build** until committed and the pseudo-version bumped. Historically (through 2026-07-26) the
root module could not import `packages/sdk-go` at all, and no test in the root module could exercise the
SDK↔ingestor contract — that is why S3 shipped undetected. As of 2026-07-28, a committed `go.work` puts the
three modules in workspace mode for local dev and `tests/contract/`, while CI's `go-root` job keeps
`GOWORK=off` to still exercise the root module against the published pseudo-version. See A2 in
[ARCHITECTURE.md](ARCHITECTURE.md) for the full, current picture.

---

## Resolved

### S1 — The entire `tests/unit` package did not compile (RESOLVED 2026-07-28)

**Verified**: `go test ./tests/unit/... -count=1` → `ok`, 251 assertions run. New baseline: **251**, up from
the 0 that ran while the package failed to build.

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

## S3 — The `/ingest` endpoint rejects 100% of well-formed events

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

---

## S4 — The official Go SDK cannot talk to the ingestor

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

---

## S5 — Regression detection can never trigger

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

**Fix direction**: capture `release_version` from metadata *before* normalization, or exclude
`release_version` from the normalizer's key set.

---

## S6 — Ingest is not tenant-scoped: any valid key can write to any project

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

---

## S7 — API key revocation and expiry do not take effect

Two independent breaks in the 008 revocation path:

1. **Subject payload mismatch.** The dashboard publishes `{ keyId }`
   (`apps/dashboard-web/src/lib/db/queries/apikeys.ts:139`); the ingestor's invalidation handler reads
   `data["key_hash"]` (`apps/ingestor-go/auth/apikey.go:38`). The key it needs is never in the message, so
   the Redis cache entry is never deleted.
2. **The stream does not exist.** The ingestor subscribes to stream `API_KEYS`
   (`apps/ingestor-go/main.go:87`), but `scripts/nats-init.sh` only creates `ERROR_EVENTS`.
   `NewSubscriber`'s error is discarded (`subscriber, _ :=`), so the failure is silent.

Separately, `getAPIKeyData` filters on `status` only and **never checks `expires_at`**. `rotateApiKey` sets
`expires_at` on the old key but leaves `status = 'active'` — so **rotated keys remain valid forever**.

Practical revocation latency today is the 60-second Redis TTL, not "instant" as `WORKLOG.md` states — and
rotation grants no expiry at all.

---

## S8 — Alerting is fully implemented and completely unwired

`grep -rn "alerts\.\|Dispatch(" --include='*.go' apps/ packages/ | grep -v _test.go` returns **only the
declaration**. Verified dead paths:

- `apps/processor-go/alerts/dispatcher.go` (204 LOC) — `NewProcessorService` never constructs a `Dispatcher`;
  `processEventInternal` never calls `Dispatch`.
- `apps/processor-go/notifiers/{email,telegram}.go` (221 LOC) — zero non-test references. `Dispatcher.sendAlert`
  ends in `log.Printf("ALERT: ...")`; it is not connected to either notifier.
- `apps/ingestor-go/validation/validator.go` `ValidatePayload` (123 LOC) — the real ingest path uses
  protovalidate instead; `ValidatePayload` is referenced only by `tests/unit/ingestor_validation_test.go`
  (which does not compile — S1).

These are covered by ~1,100 lines of unit and integration tests (`processor_alerts_test.go`,
`processor_notifier_test.go`), which pass in isolation while the production binary never reaches the code.
`docs/todos/04-alerting-and-notification-integrations.md` is therefore still entirely open despite the
packages existing.

Also: `Dispatcher.loadConfigs` only refreshes on a 5-minute ticker with **no initial load**, so even if wired
it would ignore all alert configs for the first five minutes after boot.

---

## S9 — The degradation buffer's control flow is inverted

`apps/processor-go/service/processor_service.go:36-41`:

```go
if !s.degradation.CheckAndBuffer(ctx, data) {
    log.Printf("Event buffered due to database unavailability")
    return nil          // ← ACKs the NATS message
}
return s.processEventInternal(ctx, data)
```

`CheckAndBuffer` returns `true` in two different situations — DB healthy, *and* DB down but the event was
successfully buffered. So:

| DB state | buffer | `CheckAndBuffer` | actual behavior | intended |
|---|---|---|---|---|
| up | — | `true` | processes | ✅ |
| down | has room | `true` (pushed) | **also tries to process, fails, NAKs → duplicate on redelivery** | buffer only |
| down | full | `false` (dropped) | logs "buffered", returns nil → **event ACKed and lost** | drop loudly |

The log line fires exactly when the event was *not* buffered. This directly contradicts memory entry
**B1/D1**, which record the buffer as the mitigation for data loss on DB outage.

Additional cost: `CheckAndBuffer` issues a `db.Ping` **per event**, and `processEventInternal` calls
`degradation.Flush` at the end of *every* successful event (line 140) — a re-entrant call back into
`processEventInternal`.

---

## S10 — Rate limiting is non-atomic and fails open

`apps/ingestor-go/middleware/ratelimit.go` implements the sliding window as four separate Redis round-trips
(`ZRemRangeByScore` → `ZCard` → decide → `ZAdd`), not a Lua script or pipeline. Concurrent requests all read
the same count before any writes, so the effective limit under concurrency is unbounded.

`apps/ingestor-go/main.go:83` does `redisClient, _ := redis.NewClient(...)`. If Redis is unreachable at
startup the error is discarded and `redisClient` is `nil`, which makes the middleware
`if rl.client == nil { next.ServeHTTP(...) }` — **rate limiting silently disabled**, and the API-key cache
silently disabled with it. `RATELIMIT_STRICT_MODE=true` only covers the `ZCard`-error path, not the nil-client
path.

**`docker-compose.yml` has no `redis` service and sets no `REDIS_ADDR`**, so the documented local stack always
runs in this fail-open mode. Also note the ingestor requires Redis + Postgres at boot yet compose declares
neither a healthcheck dependency on Redis nor a migration step.

`D9` in `DECISIONS.md` describes "hierarchical" rate limiting (org → project → key); the implementation keys
solely on `api_key_hash`, with no org or project tier.

---

## S11 — Fingerprints collapse when there are no in-app frames

`apps/processor-go/fingerprint/fingerprint.go` builds its hash input from `ErrorClass` plus up to 3 frames
where `InApp == true`. When a stacktrace has no `in_app` frames — which is the default for the Go SDK, since
`packages/sdk-go/event.go` `Frame` has no `InApp` field at all and never sets one — the input degenerates to
the error class alone. **Every error of a given class in a project collapses into a single issue**, losing all
grouping fidelity.

---

## Lower-severity notes

- `packages/db-migrations/cmd/migrate/main.go` `sanitizeDSN` only matches `password=...` key-value form. The
  DSNs this project actually uses are URLs (`postgres://user:pass@host/db`), which it does not redact — the
  CLI **prints the password to stdout** on every invocation.
- `packages/shared-go/nats/subscriber.go`: `s.errors <- err` on a **capacity-1** channel. The ingestor never
  drains `Errors()`, so its subscriber goroutine blocks permanently after the second error.
- Same file: a handler error triggers a bare `msg.Nak()` with no max-delivery or DLQ, so a permanently
  malformed message redelivers forever.
- `handleBatchIngest` has **no batch-size cap and no request body limit**, and returns `202` even when every
  item fails — the caller cannot distinguish total failure from success.
- `apps/processor-go/event/event.go` imports the deprecated `github.com/golang/protobuf/proto`; the ingestor
  uses `google.golang.org/protobuf/proto`. Pick one.
- `apps/dashboard-web/src/lib/server/auth-config.ts:11`: `ALLOWED_EMAIL_DOMAIN = 'company.com'` is a
  hardcoded placeholder that blocks all Google sign-in in any real deployment. Should be env-driven.
- `apps/dashboard-web/src/lib/rate-limit.ts` is an in-process `Map` with no eviction of expired keys —
  ineffective behind more than one instance and an unbounded memory leak.
- `apps/dashboard-web/src/lib/rbac.ts` knows only `admin | developer | viewer`. The database defines
  `admin|developer|viewer|support` for `project_members` and `owner|admin|engineer|support|viewer` for
  `organization_members`. `hasPermission('support', …)` and `hasPermission('owner', …)` both return `false`.
- `ProcessorService.VerifyAuditLogTable` writes a row with the all-zero UUID into `audit_logs` on every boot.
- `docker-compose.yml` maps NATS twice (`4222:4222` and `4223:4222`).

---

## Feature-status reality check

`specs/*/spec.md` and `WORKLOG.md` mark 006, 007 and 008 as **Completed**. Verified end-to-end status:

| Feature | Claimed | Verified |
|---|---|---|
| 001 error service | shipped | ingest path returns 400 for all events (S3) |
| 006 issue lifecycle / regression | Completed | regression detection unreachable (S5); dashboard now builds (S2 resolved 2026-07-28) but the underlying regression logic is still broken |
| 007 Go client SDK | Completed | SDK payload rejected by ingestor 100% (S4), failures silent |
| 008 API key management | Completed | `api/organizations/[orgId]/keys/+server.ts` is a **mock returning hardcoded fixtures with `// TODO: implement RBAC check`** and no auth; revocation broken (S7) |
| alerting (todo 04) | packages exist | never invoked (S8) |

The real query layer for 008 (`src/lib/db/queries/apikeys.ts`) is written and reasonable; it is simply not
wired to the HTTP surface. Treat "Completed" in `specs/` as "the tasks file was checked off", not as
"verified working".

---

## Keep here

- Observed runtime/build behavior with the command that produced it.
- Divergences between documented intent and executed code.

## Never store here

- Fix plans (those belong in `specs/<feature>/` or `docs/plans/`).
- Findings not reproduced by running something.
