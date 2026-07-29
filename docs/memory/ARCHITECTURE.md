# Architecture

Last reviewed: 2026-07-29

## System Overview
Sentinel follows a decoupled, event-driven architecture using NATS as the central message broker. Data flows from source to the Ingestor, through NATS, into the Processor, and finally to PostgreSQL.

## Major Components
- **Ingestor-go**: Handles incoming traffic, authentication, and initial validation. Acts as a producer for NATS.
- **Processor-go**: Consumes events from NATS, performs heavy lifting (masking, normalization, fingerprinting), and stores results in the database.
- **Dashboard-web**: Frontend for visualization and management of ingested events.
- **NATS**: Distributed message broker for service decoupling.
- **PostgreSQL**: Primary data store for processed events and metadata.

## Boundaries
- **Ingestor vs Processor**: Communication is purely asynchronous via NATS. The Ingestor should never wait for processing to complete.
- **Internal vs External**: The Ingestor is the only component exposed to external data sources. The Processor and Database remain in internal network layers.

## Integrations
- **Protobuf**: Shared contracts between all services.
- **Testcontainers**: Used for integration testing with live NATS and Postgres instances.
- **Tailwind CSS**: Standardized styling for the dashboard.

## Risks / Complexity Hotspots
- **NATS Backpressure**: High ingestion rates could lead to NATS buffer overflows if the Processor lags.
- **Database Indexing**: Large event volumes require careful indexing strategies in PostgreSQL to maintain query performance.


## As-Built vs As-Documented (verified 2026-07-26; re-verified 2026-07-29 where noted)

The component diagram above describes the intended design. Several of its arrows do not exist in the running
binaries. Before reasoning about data flow, read [VERIFIED_STATE.md](VERIFIED_STATE.md).

- **Ingestor → NATS → Processor**: exists and works. This is the load-bearing path. **As of 2026-07-29 it
  also persists**: a POSTed event produces both an `issues` row and an `error_occurrences` row — verified
  live against `sentinel-postgres` (non-zero row counts), the first time this has ever been true (S12, see
  below).
- **Ingestor auth → tenant scoping**: **now exists — re-verified 2026-07-29, RESOLVED (S6/B7).** The
  authenticated identity from `APIKeyAuthenticator.Middleware` is enforced in `main.go`'s
  `applyAuthenticatedScope`, not discarded: a body/header naming a project outside the authenticated
  identity's organization is rejected 403. See "Tenancy resolution model" below for the full rule and
  `docs/memory/BUGS.md`'s B7 entry for how the fix avoided merely relocating the same hole.
- **Processor → Alerts → Notifiers**: still does *not* exist, unchanged by this round of work. `alerts` and
  `notifiers` are implemented, tested, and never constructed by `NewProcessorService` (S8/B3).
- **Dashboard → NATS → Ingestor key invalidation**: still does *not* exist, unchanged. The `API_KEYS` stream
  **is** created by `scripts/nats-init.sh` (subject `api_key.invalidated`, consumer
  `ingestor_apikey_invalidated`) — that half of the original finding is stale. What remains broken is the
  payload: the dashboard publishes `{keyId}` while the ingestor reads `data["key_hash"]`, so the cache
  entry is never deleted (S7/B5 — only the SDK↔ingestor half of B5 was fixed this round; this
  dashboard↔ingestor half was not touched).
- **Redis**: the Ingestor requires it for rate limiting and the API-key cache. `docker-compose.yml` **now
  declares a `redis` service** (`sentinel-redis`, `redis:7-alpine`) — re-verified 2026-07-29 — so the
  documented local stack no longer runs rate-limiting-disabled *by default*. The underlying fragility is
  unchanged: `main.go` still does `redisClient, _ := redis.NewClient(...)`, discarding the connection error,
  and `middleware.RateLimiter` still does `if rl.client == nil { next.ServeHTTP(...) }` — so if Redis is
  ever unreachable at ingestor startup, rate limiting still fails open silently, exactly as before (S10,
  still open; do not mark resolved).

## Keep Here
- Stable system boundaries.
- Ownership lines between modules or services (e.g., Ingestor owns auth, Processor owns masking).
- Integration constraints that affect many features.

## Never Store Here
- Step-by-step implementation plans.
- One-off feature details.
- Stale diagrams without current boundaries.

---

### 2026-07-17 - Unified Migration Directory Boundary

**Status**
Active

**Why this is durable**
Database schema is cross-cutting infrastructure used by every Go service in the monorepo. The location and ownership of migration files is an architectural boundary, not an implementation detail — once split per app, duplicate tables and version drift become inevitable.

**Decision**
All PostgreSQL schema definitions live in a single flat directory at `packages/db-migrations/migrations/`. The `packages/db-migrations/cmd/migrate` CLI always reads from this root regardless of target database; multi-database support is achieved exclusively via per-target connection strings (e.g. `DB_URL_PROCESSOR`, `DB_URL_INGESTOR`), never via per-target migration subdirectories.

This boundary prevents:
- Duplicate table creation across apps (e.g. the `events` table).
- Versioning fragmentation where each target owns its own sequence.
- Drift between apps' schema views of the same domain.

**Tradeoffs**
- **Gained**: Single source of truth, deterministic versioning, simpler CLI surface.
- **Made harder**: Schema changes affect every target — requires extra review discipline.
- **Reconsider**: If a target legitimately needs a private schema extension, it must be justified against this boundary.

**Future mistake prevented**
Adding per-app migration subdirectories, dual-versioning tracks, or splitting schema into package-private files.

**Status re-verified 2026-07-29 — the directory-location decision still holds; two things erode it in
practice and are worth knowing before trusting "one flat directory" as the whole picture:**
1. **A third, independently hand-maintained schema copy exists.** `scripts/db/init.sql` is a standalone SQL
   file, frozen at the 1716508800 baseline, that duplicates (does not extend) the schema `packages/db-migrations`
   produces — e.g. it still carries the pre-fix `CHECK (status IN ('open','resolved','ignored'))` on `issues`
   that the S12 fix below removed from the real migration ledger. `tests/integration/setup_test.go` and
   several sibling test files apply this file directly, meaning integration tests can run against a schema
   that has *never seen* any migration after 1716508800, including the S12 fix. This is not itself in scope
   for this round of work — recorded as a known drift, not resolved.
2. **Migrations are re-applied under several independent goose ledgers against the same physical database**
   (`schema_migrations`, `seq_migrations`, `processor_migrations`, `baseline_test_migrations` — see
   `docs/memory/BUGS.md`'s "Multiple Independent Migration Ledgers" entry). The single-directory decision
   controls where migration *files* live; it does not by itself guarantee a single *applied* state, because
   nothing stops multiple `TableName`-scoped ledgers from independently believing they own the same target
   database.

Neither point argues for reversing A1 — splitting the directory would make both worse, not better. They
argue for a follow-up (not undertaken this round): retire `scripts/db/init.sql` in favor of driving
integration test setup through the real `packages/db-migrations` migration runner, and point test ledgers at
disposable databases rather than the shared dev instance.

**Evidence**
- `specs/004-shared-db-migrations/architecture-migration-plan.md`
- `specs/004-shared-db-migrations/plan.md` → Structure Decision
- `specs/004-shared-db-migrations/spec.md` → Clarifications Q2
- `scripts/db/init.sql` vs `packages/db-migrations/migrations/1716508800_init.sql` (drift, 2026-07-29)
- `docs/memory/BUGS.md` — "Multiple Independent Migration Ledgers Pointed at One Physical Database"

**Where to look next**
`packages/db-migrations/migrations/` and `packages/db-migrations/cmd/migrate/`. Also `scripts/db/init.sql`
and `tests/integration/setup_test.go` for the drift noted above.

---

### 2026-07-26 - Three-Module Go Layout, Workspace Mode for Local/Contract Testing Only (updated 2026-07-28)

**Status**
Active — superseded in part by P0-3 (`docs/plans/E2E_RECOVERY_PLAN.md`, constraint C1, now resolved
2026-07-28).

**Why this is durable**
Module boundaries determine what can ever be tested together. Until P0-3, this layout silently forbade an
entire class of test, which is why the SDK↔ingestor contract broke undetected (S4/B5). The fix is a
deliberate **two-mode split** — workspace mode locally and in the `contract`/`go-sdk`/`go-migrations` CI
jobs, `GOWORK=off` in the `go-root` CI job — and future readers must not collapse that split back into "just
add go.work" or "just remove it": both halves are load-bearing.

**Decision (as-built)**
The repository still contains three independent Go modules — root (`github.com/NurfitraPujo/sentinel`),
`packages/sdk-go`, and `packages/db-migrations` — with **no `replace` directives**. As of P0-3, a `go.work`
file **is committed** at the repo root (removed from `.gitignore`):

```
go 1.25.8

use (
	.
	./packages/db-migrations
	./packages/sdk-go
)
```

- In workspace mode (the default whenever `go.work` is present and `GOWORK` is unset), **the root module can
  import `packages/sdk-go`**. This unblocks `tests/contract/` (P2-4), which exercises the SDK's real wire
  payload against `ingestor-go`'s decoder — the test that S4/B5 previously made impossible to write.
- **Constraint C2 still holds and is unchanged by P0-3**: the root module's `go.mod` still requires
  `packages/db-migrations` at a **published pseudo-version** (`v0.0.0-20260723071735-63e4b00a5f52`), not a
  `replace`. Workspace mode overrides that resolution *locally* with the on-disk copy — convenient for
  development, but it means a developer's machine and a `GOWORK=off` CI run can legitimately build against
  different code for `packages/db-migrations`. Local edits under `packages/db-migrations/` are still
  invisible to any `GOWORK=off` build until committed and the pseudo-version bumped.
- **Deliberate CI divergence**: `.github/workflows/ci.yml`'s `go-root` job pins `env: GOWORK: "off"` on
  purpose, so it keeps exercising the root module exactly as a real downstream `go get` would see it — against
  the published pseudo-version, never the local workspace copy. This is the safeguard against workspace mode
  silently masking a real version-skew bug between what's on disk and what's actually published. The
  `go-sdk` and `go-migrations` jobs **also** set `GOWORK=off`, for the same reason applied to themselves: the
  root `go.work` is found by walking *up* from `packages/sdk-go`, which inflates that module's graph from 19
  to 169 modules and forces `go 1.25.8` onto every matrix leg — silently destroying the `go 1.21` leg that
  exists to prove the public SDK's declared minimum version. **`contract` is the only job that runs in
  workspace mode**, because importing `packages/sdk-go` from the root module is its entire purpose.
- **`//go:build contract` tag convention**: files under `tests/contract/` (P2-4's deliverable — **the
  directory now exists**, added 2026-07-29 as `tests/contract/sdk_ingestor_test.go`, 471 lines) carry a
  `//go:build contract` build tag. This keeps them out of the default `go build ./...` / `go vet ./...` /
  `go test ./tests/unit/...` invocations that `go-root` runs under `GOWORK=off` — where an import of
  `packages/sdk-go` would fail to resolve, since the root module's `go.mod` has no `require`/`replace` for
  it. The `contract` CI job builds/tests that directory explicitly (workspace mode, no `GOWORK=off`), so the
  tag never needs to match there. **Verified 2026-07-29**: `GOWORK=off go build ./...` succeeds (the tagged
  file is excluded), and `go test -tags=contract ./tests/contract/...` passes (4 tests) in workspace mode.
  See `docs/memory/BUGS.md`'s "Cross-Boundary Payload Contracts Drift" entry for what the test actually
  proves about the SDK↔ingestor seam, and note it does *not* cover the dashboard↔ingestor NATS seam, which
  is the other half of that same B5 entry and remains untested.

**Tradeoffs**
- **Gained**: `packages/sdk-go` is still independently versionable and `go get`-able at its own cadence (the
  stated goal of feature 007), with a minimal dependency tree pinned at `go 1.21`. Workspace mode adds a real
  contract-testing path (P2-4) without touching that.
- **Made harder**: reasoning about "what does the root module see" now depends on `GOWORK`, not just on
  `go.mod`. Any change touching both a migration and the code that reads it is still a two-commit dance
  under `GOWORK=off` (i.e. in the `go-root` CI job and in any local `GOWORK=off` run).
- **Reconsider**: if `packages/db-migrations` gains a `replace` directive or its pseudo-version dependency is
  otherwise eliminated, the `GOWORK=off` job and this whole divergence note become unnecessary and should be
  removed together.

**Future mistake prevented**
1. Planning a "just add a test for it" fix to an SDK/ingestor or migration/code contract without first
   noticing the module graph — now: without first noticing which `GOWORK` mode the test will actually run
   under.
2. Assuming `go.work`'s presence means every CI job sees the same `packages/db-migrations` code as disk —
   `go-root` deliberately does not.
3. "Fixing" the `go-root` job by removing `GOWORK=off` because workspace mode "should just work everywhere" —
   that is the exact mask C2 warns about.

**Evidence**
`go.work` (committed, `use . ./packages/db-migrations ./packages/sdk-go`), `go.mod` (no `replace`),
`packages/sdk-go/go.mod`, `.gitignore` (no longer lists `go.work`), `.github/workflows/ci.yml` (`go-root`
job's `env: GOWORK: "off"`), `tests/contract/sdk_ingestor_test.go` (P2-4, added 2026-07-29 — the test this
whole workspace-mode split exists to enable).
`docs/memory/VERIFIED_STATE.md` — module layout table, S4.
`docs/plans/E2E_RECOVERY_PLAN.md` — constraint C1 (resolved 2026-07-28), P0-3.

**Where to look next**
`go.work`, `go.mod`, `packages/sdk-go/go.mod`, `packages/db-migrations/go.mod`, `.github/workflows/ci.yml`,
`tests/contract/sdk_ingestor_test.go`.

---

### 2026-07-29 - The Wire Contract's Single Source of Truth Is the Proto; Two Independent Hand-Copies Must Match It

**Status**
Active

**Why this is durable**
`ErrorEvent` (the protobuf message in `packages/proto/sentinel/v1/error_event.proto`) is the only place the
field names, types, and size limits of an ingested error event are declared once. Every other representation
of that same event — `packages/sdk-go`'s `Event` struct (what the SDK sends), and
`apps/ingestor-go/validation.ErrorPayload` (what the ingestor decodes) — is a **hand-maintained JSON
mirror** of that proto, written independently, in a different language module, with no compiler or codegen
step tying it back. B5 in `docs/memory/BUGS.md` is the record of what happens when these three drift: S3
(a redundant field-level rule fighting the message-level CEL rule), S4/S16 (field-name and semantic
mismatches between the SDK and the ingestor), and S14 (CEL rules with no length bound at all against a
bounded VARCHAR column) are all instances of the same underlying fact — nothing enforces agreement between
the proto and its two hand-copies except a test that decodes real bytes across the boundary.

**Decision (as-built)**
The proto is authoritative for three things, and the other two representations must match it by convention,
checked only by `tests/contract/sdk_ingestor_test.go` (P2-4):
1. **Field names on the wire.** The JSON tag on `sdk-go`'s `Event` struct field and the JSON tag on
   `ingestor-go`'s `ErrorPayload` field for "the same" datum must be textually identical strings — there is
   no third name to compare both against except the proto's own field name, which is usually (not always;
   `ProjectKey`/`project_key` maps to `project_key` on the wire but is not literally the same string as the
   proto field name in every case) a reasonable default.
2. **Size limits.** Every proto field with a CEL length rule (`packages/proto/sentinel/v1/error_event.proto`'s
   `(buf.validate.message).cel` block) exists because a downstream Postgres column is bounded — see the
   per-field comments added 2026-07-29 mapping each rule to its column (e.g. `platform` ≤ 50 chars ↔
   `error_occurrences.platform VARCHAR(50)`; `error_class` ≤ 255 ↔ `issues.error_class VARCHAR(255)`;
   `trace_id`/`span_id` ≤ 64 ↔ their VARCHAR(64) columns; `release_version` ≤ 100 ↔
   `error_occurrences.release_version VARCHAR(100)`; `fingerprint` ≤ 64 ↔ `issues.fingerprint VARCHAR(64)`).
   A field added to the proto without a matching CEL rule sails through validation with a 202 and then fails
   to store — this is exactly how S14 shipped, twice (it is the same defect class as S3).
3. **Required-ness.** `project_key`, `platform`, `environment`, `error_class` are `(buf.validate.field).required
   = true` on the proto; both hand-copies must treat an empty value for these as a client error, not silently
   pass it through.
4. **One field rule per constraint, never two.** `message` (proto field 4) deliberately carries **no**
   `(buf.validate.field).string` rule at all — its 10000-character limit lives *only* in the message-level CEL
   expression. This is not an oversight; a field-level `string.len` rule and a CEL rule expressing the same
   constraint differently (`len` means *exactly* N, not *at most* N) is how S3 shipped and went undetected —
   see the inline comment at that field in the proto and the B5 entry in `BUGS.md`.

`ErrorEvent.project_id` (proto field 16) is the newest addition to this contract: it exists on the wire so
the ingestor can eventually pass through a server-resolved (not client-supplied) tenant scope instead of
resolving `project_key` by name on every event. As of this round, `store.ResolveProjectID` prefers
`project_id` when present and falls back to the legacy `GetProjectByKey` name lookup otherwise — see
"Tenancy resolution model" below for how `project_id`/`project_key` get *authenticated* values before they
ever reach this field.

**Tradeoffs**
- **Gained**: the SDK stays a genuinely independent, minimal-dependency Go module (no proto/buf toolchain
  required by SDK consumers) while the ingestor validates against the real generated proto types.
- **Made harder**: every field added to the proto is a three-place edit (proto, SDK struct, ingestor struct)
  with no compiler check tying them together except `tests/contract/`, and that test only runs in the
  `contract` CI job / local workspace mode (see the three-module-layout entry above) — a change made and
  tested only under `GOWORK=off` (the `go-root` job's default) will not catch a contract break.
- **Reconsider**: if `packages/sdk-go` ever accepts a proto/buf build-time dependency, codegen could replace
  the hand-copy entirely and this whole class of drift becomes structurally impossible rather than
  test-caught.

**Future mistake prevented**
Adding or renaming a field on one side of this boundary (proto, SDK struct, or ingestor struct) without the
other two in the same change, and without running `tests/contract/` before considering the change done.

**Evidence**
`packages/proto/sentinel/v1/error_event.proto` (field comments added 2026-07-29 cross-referencing each
column), `packages/sdk-go/event.go`, `apps/ingestor-go/validation/validator.go`,
`apps/ingestor-go/mapping/mapper.go`, `tests/contract/sdk_ingestor_test.go`. `docs/memory/VERIFIED_STATE.md`
S3, S4, S14, S16.

**Where to look next**
`packages/proto/sentinel/v1/error_event.proto`, `packages/sdk-go/event.go`,
`apps/ingestor-go/validation/validator.go`, `tests/contract/sdk_ingestor_test.go`.

---

### 2026-07-29 - Tenancy Resolution Model: Credential Determines Org, Project Resolves Within It, Header Beats Body

**Status**
Active

**Why this is durable**
This is the rule that closed S6/B7 (any valid key could write into any tenant's project). Getting it wrong
in either direction reopens that hole under a different name — see `docs/memory/BUGS.md` B7 for the
near-miss this fix specifically had to avoid (moving the trusted value into a new, equally
attacker-controlled channel without validating the new channel's scope).

**Decision (as-built)**
1. **The organization is fixed by the credential and only the credential.** `auth.OrganizationIDFromContext`
   is populated by `APIKeyAuthenticator.Middleware` from the authenticated `project_api_keys` row and is
   never overridden by anything client-supplied, at any layer.
2. **The project is resolved *within* that organization, never globally.** Two credential shapes:
   - **Project-scoped key** (`project_api_keys.project_id IS NOT NULL`): the project is fixed by the
     credential itself. A request body naming a *different* project is a client misconfiguration and is
     rejected with 403 — never silently rewritten to the authenticated project, and never silently accepted
     as an override.
   - **Organization-wide key** (`project_id IS NULL`): the credential fixes only the organization; the
     caller must name the target project. Resolution goes through `auth.ResolveProjectInOrg`, whose query is
     always `WHERE name = $1 AND organization_id = $2` — the `organization_id` comes from the authenticated
     context, never from the request. A name that exists but belongs to a different org is
     indistinguishable, from the error alone, from a name that does not exist at all (403 either way) — this
     is deliberate, to prevent using the endpoint to enumerate other tenants' project names.
3. **Header takes precedence over body; the two are never merged.** An org-wide key may name its target
   project via the `X-Project-Key` request header (resolved once, in the auth middleware, before the body is
   even decoded) or via the body's `project_key` field (resolved later, in `applyAuthenticatedScope`, if the
   header was absent). When the header is present, it alone determines the target and the body's
   `project_key` is never consulted for that request — not compared, not merged, not used as a
   confirmation. This makes "which one wins" a non-question for any future reader: there is no case where
   both are read and reconciled.
4. **`projects(organization_id, name)` is now UNIQUE** (`CREATE UNIQUE INDEX idx_projects_org_name`,
   `1721800000_add_organization_layer.sql`, added 2026-07-29). Same-named projects across *different*
   organizations are still allowed and expected (this is what makes the org-scoped resolution in point 2
   necessary in the first place); duplicate names *within* one organization are now a constraint violation,
   closing the "arbitrary pick among duplicates" failure mode `GetProjectByKey`'s old unscoped
   `WHERE name = $1` was exposed to.

**Tradeoffs**
- **Gained**: a cross-tenant write requires forging or stealing a credential, not just guessing or knowing
  another tenant's project name.
- **Made harder**: an org-wide key's caller must get the header/body precedence right — a client that sets
  both, expecting the body to be a fallback confirmation, will find the header silently wins and the body is
  ignored. This is a minor footgun for SDK authors, not a security issue.
- **Reconsider**: if a future caller legitimately needs "header AND body must agree" semantics (belt-and-braces
  validation) rather than "header wins," that is a deliberate change to point 3, not a bug fix.

**Future mistake prevented**
1. Resolving a client-supplied project identifier (by any name — header, body, query param) against a table
   with no organization scope in the `WHERE` clause. This is the exact shape S6 had and the exact shape a
   naive header-based "fix" would have reintroduced.
2. Trusting `payload.ProjectID`/`payload.ProjectKey` anywhere downstream of `applyAuthenticatedScope` as
   client input — by the time `svc.Ingest` runs, both fields have been overwritten with the authenticated
   identity's values (or the request has already been rejected 403).

**Evidence**
`apps/ingestor-go/main.go` (`applyAuthenticatedScope`), `apps/ingestor-go/auth/apikey.go`
(`ResolveProjectInOrg`), `apps/ingestor-go/auth/context.go` (`WithIdentity`, `IsOrgWideKey`,
`OrganizationIDFromContext`), `1721800000_add_organization_layer.sql` (`idx_projects_org_name`). Verified:
project-scoped key + body naming another project → 403; org-wide key naming a project in another org → 403,
zero rows written there. `docs/memory/VERIFIED_STATE.md` S6; `docs/memory/BUGS.md` B7.

**Where to look next**
`apps/ingestor-go/main.go`, `apps/ingestor-go/auth/apikey.go`, `apps/ingestor-go/auth/context.go`,
`packages/db-migrations/migrations/1721800000_add_organization_layer.sql`.
