# Architecture

Last reviewed: 2026-07-26

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


## As-Built vs As-Documented (verified 2026-07-26)

The component diagram above describes the intended design. Several of its arrows do not exist in the running
binaries. Before reasoning about data flow, read [VERIFIED_STATE.md](VERIFIED_STATE.md).

- **Ingestor → NATS → Processor**: exists and works. This is the load-bearing path.
- **Ingestor auth → tenant scoping**: does *not* exist. The authenticated project is discarded; scoping comes
  from the untrusted request body (S6/B7).
- **Processor → Alerts → Notifiers**: does *not* exist. `alerts` and `notifiers` are implemented, tested, and
  never constructed (S8/B3).
- **Dashboard → NATS → Ingestor key invalidation**: does *not* exist. The `API_KEYS` stream is never created
  and the message body field names disagree (S7/B5).
- **Redis**: the Ingestor requires it for rate limiting and the API-key cache, but `docker-compose.yml`
  declares no `redis` service and the client error is discarded — the documented local stack always runs with
  rate limiting silently disabled (S10).

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

**Evidence**
- `specs/004-shared-db-migrations/architecture-migration-plan.md`
- `specs/004-shared-db-migrations/plan.md` → Structure Decision
- `specs/004-shared-db-migrations/spec.md` → Clarifications Q2

**Where to look next**
`packages/db-migrations/migrations/` and `packages/db-migrations/cmd/migrate/`.

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
- **`//go:build contract` tag convention**: files under `tests/contract/` (P2-4's deliverable; the directory
  does not exist yet) are expected to carry a `//go:build contract` build tag. This keeps them out of the
  default `go build ./...` / `go vet ./...` / `go test ./tests/unit/...` invocations that `go-root` runs
  under `GOWORK=off` — where an import of `packages/sdk-go` would fail to resolve, since the root module's
  `go.mod` has no `require`/`replace` for it. The `contract` CI job builds/tests that directory explicitly
  (workspace mode, no `GOWORK=off`), so the tag never needs to match there.

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
job's `env: GOWORK: "off"`).
`docs/memory/VERIFIED_STATE.md` — module layout table, S4.
`docs/plans/E2E_RECOVERY_PLAN.md` — constraint C1 (resolved 2026-07-28), P0-3.

**Where to look next**
`go.work`, `go.mod`, `packages/sdk-go/go.mod`, `packages/db-migrations/go.mod`, `.github/workflows/ci.yml`.
