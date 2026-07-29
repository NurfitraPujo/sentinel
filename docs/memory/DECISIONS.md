# Technical Decisions (`docs/memory/`)

This file stores durable technical and implementation decisions. For governance-level decisions or project standards, see `.specify/memory/DECISIONS.md`.

## Entry Lifecycle

Each decision follows this lifecycle:

```
Active → Needs Review → Superseded → (pruned)
```

- **Active**: The decision is current and must be honored by all features and AI agents.
- **Needs Review**: Implementation reality or new context suggests this decision may be outdated. It should still be honored until reviewed and explicitly changed.
- **Superseded**: A newer decision has replaced this one. Keep it for historical context until the next audit, then consider pruning.
- **Pruned**: During an audit, remove superseded entries that no longer provide historical value. This keeps the file focused.

### When to change status

| Current Status | Change To    | When                                                                                                       |
| -------------- | ------------ | ---------------------------------------------------------------------------------------------------------- |
| Active         | Needs Review | Verified implementation or tests contradict the decision, or recurring features follow a different pattern |
| Active         | Superseded   | A newer decision explicitly replaces this one                                                              |
| Needs Review   | Active       | Team confirms the decision still holds after review                                                        |
| Needs Review   | Superseded   | Team confirms a replacement decision                                                                       |
| Superseded     | _(remove)_   | Audit finds no remaining historical value                                                                  |

### Rules

- Never delete an Active decision without replacing or superseding it.
- Never silently ignore a decision. If it feels wrong, mark it Needs Review and resolve it.
- Keep at most 3–5 Superseded entries for context. Prune older ones during audits.

---

### 2024-05-20 - Graceful Degradation via In-Memory Buffering

**Status**
**SUPERSEDED by D10 (2026-07-29) — the mechanism was DELETED, not repaired.**

**What this decision specified**
It required (past tense — none of this exists in code any more) that when PostgreSQL was
unavailable, the processor buffer incoming events in memory (MaxBufferSize =
10,000) and flush them once the connection was restored, so a temporary outage could not lose events.

**Why it is gone**
The mechanism never delivered its guarantee, and could not be made to. `CheckAndBuffer` returned `true`
for two situations the caller could not tell apart — DB healthy, and DB down but buffered — so a down
database still fell through to a live write, and a FULL buffer was logged as "buffered" and then ACKed
and lost. The 2026-07-29 repair of that inversion made it worse: it ACKed events held only in process
memory, so a crash mid-outage destroyed them (measured: 3 events buffered+ACKed, processor SIGKILLed,
restarted → 0 rows, no redelivery, no DLQ entry).

**The durable lesson — this is the part worth keeping.** In-process buffering cannot be both crash-safe
and duplicate-free for this pipeline:
- buffer + ACK loses events on crash or redeploy, because the buffer is process memory; and
- buffer + NAK double-processes, because the flush replays AND redelivery replays, while
  `issues.count` is an `ON CONFLICT` increment, so the duplicate is visible in the product.
There is no third option without a server-side idempotency key, and `event_id` has no server-side
destination at all (VERIFIED_STATE.md S16). **Do not reintroduce in-process event buffering as a
database-outage mitigation until that key exists.**

**What replaced it**
`GracefulDegradation` is now only a database-health gate: `Evaluate` returns `StatusProcessed` or
`StatusUnavailable`, nothing else. `ProcessorService.ProcessEvent` returns a non-nil error on
`StatusUnavailable` unconditionally, so **D10** owns recovery — bounded retry with backoff
(1s/5s/15s/30s/60s, MaxDeliver 5) and then `ERROR_EVENTS_DLQ`. Removed from
`apps/processor-go/degradation/buffer.go`: `EventBuffer`, `BufferedEvent`, `NewEventBuffer`, `Push`,
`Drain`, `Size`, `MaxBufferSize`, `SetFlushHandler`, `triggerAsyncFlush`, `CheckAndBuffer`, `Flush`,
`BufferSize`, and the `StatusBuffered`/`StatusDropped` values. A grep across `apps/ packages/ tests/
tools/ scripts/` returns zero live Go references — only historical comments that explain the removal.

**Evidence (2026-07-29)**
Real compose stack, real `sentinel-ingestor`/`sentinel-processor` binaries: Postgres stopped
mid-stream, 5 events POSTed to `/ingest` (all 202), Postgres restarted → **5 issues with `count=1`
each, 5 occurrences, zero loss, zero duplicates**. Processor log shows 10 × `returning event to NATS
for bounded retry (D10)` → `Database connection restored` → exactly 5 `Processing event` lines.
Harder drills: SIGKILL of the processor mid-outage → still 5/5 exactly once; an outage longer than the
retry budget → all 3 events dead-lettered, `dlq_depth=3` visible on `/health` at ~56s (the observed
capture boundary).

**Accepted residual risk — read this before trusting "no duplicates"**
That result is scoped to FULL outages, where `Evaluate` short-circuits before any write. The
issue/occurrence write pair is **not idempotent under redelivery**: `UpsertIssueWithOutcome` commits
its own transaction before `InsertOccurrence` runs, so a retryable failure between them inflates
`issues.count` by one per redelivery (proved: 1 event, 5 deliveries → `issues.count=5, occurrences=0`).
Pre-existing, not introduced by the deletion — but redelivery is now the ONLY recovery path, so it
matters more. Fix by wrapping both writes in one transaction, or by landing the `event_id` idempotency
key. Knowingly accepted until then.

### 2026-05-15 - Magic Link Authentication via Auth.js Email Provider

**Status**
Active

**Why this is durable**
Local development environments may not have access to Google Workspace OIDC. Magic link authentication provides a fallback that doesn't bypass project RBAC.

**Decision**
- Use Auth.js built-in Email provider for magic link support
- Tokens are cryptographically random, expire in 15 minutes, and are single-use (handled by Auth.js)
- Magic link authentication does NOT bypass project RBAC - user must still have project membership
- For local dev, use `smtp://debug` to output email JSON to stdout instead of sending

**Tradeoffs**
- **Gained**: Simple local auth without Google OAuth setup
- **Made harder**: Requires SMTP configuration for production
- **Reconsider**: If magic link deliverability issues arise in production

**Future mistake prevented**
Custom auth implementation that bypasses RBAC or uses insecure token handling.

**Evidence**
Implementation in `apps/dashboard-web/src/lib/auth.ts` and `apps/dashboard-web/src/routes/auth/signin/+page.svelte`.

**Where to look next**
`specs/001-sentinel-error-service/tasks.md` (Phase 5: Local Development Support)

---

### 2026-07-17 - Adopt Goose for All Database Migrations

**Status**
Active

**Why this is durable**
Database migrations are cross-cutting infrastructure used by every Go service in the monorepo. Choosing a tool means every future schema change inherits its guarantees (transactional DDL, advisory-lock concurrency, static-SQL policy) and its constraints (Go runtime, `pgx/v5` driver).

**Decision**
Use `github.com/pressly/goose/v3` as the only migration tool across the project, wrapped by `packages/db-migrations/cmd/migrate`. Migrations live as static `.sql` files in the unified directory; dynamic schema logic MUST be implemented via `goose`'s Go-based migrations with proper parameterization — never via CLI flags or string concatenation.

**Tradeoffs**
- **Gained**: Native Go integration, transactional DDL, advisory-lock concurrency, `.sql` and `.go` migration support, `pgx` compatibility, no external binary when compiled.
- **Made harder**: Go runtime is now required for any tooling that produces or validates migration files.
- **Reconsider**: If a non-Go service needs migrations outside the Go CLI workflow.

**Future mistake prevented**
Re-evaluating migration tools per app, introducing dynamic SQL through CLI flags, or accepting migration tooling that lacks transactional DDL.

**Accepted risk (2026-07-29) — already-applied migration files were edited in place**
`goose` stores no checksums of applied migration files (only version numbers in its tracking table), so
it cannot detect that an already-applied file changed underneath it. During P2b, three already-applied
migrations — `1721800000_add_organization_layer.sql`, `1721900000_add_issue_lifecycle_and_relations.sql`,
`1722000000_add_api_key_management.sql` — were edited in place (see D4's Tension entry for the idempotency
change specifically, and the S12 fix for the dropped `issues_status_check` constraint). Any database that
already ran the old file content will never see these edits; its schema silently diverges from a fresh
`up` on the same version. Normally this would be a correctness incident on its own. **The repo owner has
confirmed nothing is deployed**, so no database exists yet where the pre-edit version was ever the final
state of a long-lived environment — the only affected databases are local/dev/test, disposable, and were
re-migrated as part of this same work. This is an accepted, one-time risk for this specific recovery, not
a precedent: future migrations must not be edited in place once any database anyone cares about has
applied them; add a new migration instead.

**Evidence**
- `specs/004-shared-db-migrations/research.md` (tool selection rationale)
- `specs/004-shared-db-migrations/plan.md` (Primary Dependencies)
- `specs/004-shared-db-migrations/security-constraints.md` (R-002)
- Implementation: `packages/db-migrations/goose.go`, `packages/db-migrations/driver.go`

**Where to look next**
`packages/db-migrations/` and `Taskfile.yml` `db:*` task namespace.

---

### 2026-07-17 - Strict Loud-Failure Migration Policy

**Status**
Active

**Why this is durable**
Silent or partially-applied schema changes corrupt production data and are expensive to detect. This policy is enforced by `goose` itself (transactional DDL + advisory lock) and is the contract every migration author must honor.

**Decision**
Migrations MUST follow these non-negotiable rules:
1. **Single-run only**: Concurrent migration runs against the same target are blocked by `goose`'s advisory lock. Parallel development is handled by the versioned filename convention.
2. **Versioned filenames** (e.g. `<timestamp>_name.sql`); the CLI fails loudly on version collision or out-of-order versions — no auto-rebasing.
3. **Stop on first failure**: a failed `up` or `down` aborts the run. Partial state is the user's responsibility to clean up; the tool surfaces the error, never retries.
4. **No silent rollback on errors**.

**Tradeoffs**
- **Gained**: Deterministic schema state; cheap reasoning; no double-applied migrations; safe concurrent deploys.
- **Made harder**: Recovery from partial failures is manual and requires documentation.
- **Reconsider**: If multi-region active/active migrations become a requirement, the single-run assumption will need revisiting.

**Future mistake prevented**
Wrapping migrations in retry loops, swallowing partial-failure errors, or introducing a parallel migration path that bypasses the advisory lock.

**Tension (2026-07-29) — migrations were made idempotent; this is not what rule 3/4 above intend, and needs an explicit carve-out**
During the P2b recovery, `1721800000_add_organization_layer.sql`, `1721900000_add_issue_lifecycle_and_relations.sql`,
and `1722000000_add_api_key_management.sql` — all three **already applied** to the running dev database
— were edited to add `IF EXISTS` / `IF NOT EXISTS` guards on every `ALTER TABLE ADD COLUMN`, `CREATE
INDEX`, `CREATE TABLE`, `DROP ...`, plus two `DO $$ ... IF NOT EXISTS (SELECT ... FROM pg_catalog) $$`
blocks for `ADD CONSTRAINT`, which Postgres has no direct `IF NOT EXISTS` DDL for
(`rtk git diff --cached HEAD -- packages/db-migrations/migrations/`). A second `up` of any of these
files against an already-migrated schema now silently no-ops instead of erroring.

**Why this was done**: the repo's local/test topology has **at least five independent goose
version-tracking tables** pointed at the **same physical Postgres database**: `schema_migrations` is the
CLI's own default ledger (`packages/db-migrations/goose.go:22,28,55,73`), and
`tests/integration/db_migrations_test.go` defines four more distinct `MigrationOptions.TableName` values
exercised against that same database — `seq_migrations` (line 73), `processor_migrations` (line 108),
`dashboard_migrations` (line 121), `baseline_test_migrations` (line 153) — plus a sixth,
`status_test_migrations` (line 43), used only for `status` command tests. Each ledger tracks its own
idea of "applied" independently of the others, so a migration one ledger considers new can already have
been run by a different ledger against the identical tables — and did, which is how U30 (up→down×5→up
round-trip) and the S12 fix needed to be replay-safe to even test. This is a genuine, reproduced local
hazard (see also VERIFIED_STATE.md's "running the integration suite corrupts the shared dev database"
hazard note), not speculative.

**Tension**: rule 3 ("stop on first failure... partial state is the user's responsibility") and rule 4
("no silent rollback on errors") describe a tool that surfaces every anomaly loudly. An `ADD COLUMN IF
NOT EXISTS` that silently no-ops when the column is already there is the opposite instinct: it
absorbs an anomaly (two ledgers disagreeing about state) instead of surfacing it. A genuinely wrong
second application — e.g., a column that should have a different type the second time, or an `ADD
COLUMN IF NOT EXISTS` masking a real forgotten migration further up the chain — would now also silently
no-op instead of failing loudly, which is precisely what D4 exists to prevent.

**Recommendation**: idempotency guards should be a narrow, explicitly-labeled carve-out for the
multi-ledger-single-database hazard above — not a general migration-authoring style. The comments added
in the migration files themselves already do this (they cite U30 and the multi-ledger scenario inline),
which is the right instinct; this entry makes it a decision instead of a comment. The durable fix is
architectural, not idempotency: collapse to one goose version-tracking table per physical database (or
give each target's tests their own throwaway database instead of sharing the dev one — U30's own test
already uses "a throwaway DB", which suggests the pattern exists and the shared-database ledgers are the
outlier). Until that lands, treat `IF EXISTS`/`IF NOT EXISTS`/guarded `DO $$` blocks in migrations that
have already reached any shared database as an accepted, narrow exception to rule 4 — not a precedent
for new migrations authored against a single-ledger target, which should keep failing loudly on replay
as originally specified.

**Evidence**
- `specs/004-shared-db-migrations/spec.md` → Edge Cases
- `specs/004-shared-db-migrations/plan.md` → Technical Context Constraints
- `specs/004-shared-db-migrations/security-constraints.md` → Confirmed Secure Patterns (transactional DDL, concurrency locking)
- Idempotency retrofit: `packages/db-migrations/migrations/1721800000_add_organization_layer.sql`, `1721900000_add_issue_lifecycle_and_relations.sql`, `1722000000_add_api_key_management.sql`

**Where to look next**
`packages/db-migrations/goose.go` and the `db:*` Taskfile targets.

---

### 2026-07-17 - Production Safety Guardrails for Destructive Migration Tasks

**Status**
Active

**Why this is durable**
`task db:<target>:reset` and `baseline` are catastrophic if run against production. Taskfile targets are easy to invoke from CI or shell history.

**Decision**
- Destructive Taskfile targets (`reset`, `baseline`) MUST either include an interactive confirmation prompt OR be hard-blocked when `ENVIRONMENT=prod`.
- Database connection strings MUST be passed via environment variables (`DB_URL_<TARGET>`); the migration CLI MUST NOT log full DSNs on error or startup.
- Migrations SHOULD run under a dedicated, least-privilege DB user with `CREATE/ALTER/DROP` on schema objects.

**Tradeoffs**
- **Gained**: Defense in depth against accidental destructive operations and credential leakage in CI logs.
- **Made harder**: Slightly more ceremony to run destructive tasks in non-production environments.
- **Reconsider**: If `ENVIRONMENT` semantics are unified across CI/CD.

**Future mistake prevented**
Running `task db:reset` in prod, leaking DSNs in CI logs, granting the migration role data-access permissions.

**Checked (2026-07-29)**: reviewed against the P2b in-place migration edits (see D3's Accepted Risk and
D4's Tension entries). Not implicated — this decision governs destructive Taskfile targets, DSN handling,
and least-privilege DB roles, none of which the migration-file edits touched or bypassed.

**Evidence**
- `specs/004-shared-db-migrations/plan.md` → Security & Governance Context
- `specs/004-shared-db-migrations/security-constraints.md` → Findings 1, 3 and Recommendations R-001, R-003
- Implementation: `Taskfile.yml` `db:*` targets; `packages/db-migrations/cmd/migrate`

**Where to look next**
`Taskfile.yml`, `packages/db-migrations/cmd/migrate/main.go`.

---

### 2026-07-24 - Organization-First Multi-Tenancy & Role Inheritance

**Status**
Active

**Why this is durable**
Defines the multi-tenant resource hierarchy and authorization precedence for Sentinel across all current and future features.

**Decision**
Projects must belong to exactly one Organization. User authorization is resolved by inheriting the user's `organization_members.role` (`owner`, `admin`, `engineer`, `support`, `viewer`) across all org projects by default, unless a specific project override exists in `project_members`. Session context resolution prioritizes the URL slug `/[orgSlug]/...` as the primary source of truth, with `user_session_preferences` used solely for root landing.

**Tradeoffs**
- **Gained**: Clean multi-tenant data isolation and low operational friction for org admins.
- **Made harder**: Single project access grants require explicit `project_members` override configuration.
- **Reconsider**: If standalone non-organizational project monitoring is required.

**Future mistake prevented**
Implementing fragmented per-project access checks or allowing data leakage across organization boundaries.

**Evidence**
- Implementation: `apps/dashboard-web/src/lib/db/queries/organizations.ts`, `apps/dashboard-web/src/hooks.server.ts`
- Spec & Plan: `specs/005-organization-layer/spec.md`, `specs/005-organization-layer/data-model.md`

**Where to look next**
`apps/dashboard-web/src/lib/db/queries/organizations.ts` and `apps/dashboard-web/src/hooks.server.ts`.

---

### 2026-07-25 - Real-time Ingestion Regression Detection with Polymorphic Assignees & Async Relations

**Status**
Active

**Why this is durable**
Establishes the real-time regression reopening architecture inside the high-throughput Go ingestion worker while decoupling asynchronous issue linkage to protect ingestion throughput.

**Decision**
- Perform automated version-aware regression reopening (`detectAndHandleRegression`) directly inside `apps/processor-go/store/store.go` during event ingestion.
- Maintain 0% read/lock overhead on `issue_relations` on the high-throughput ingestion path.
- Support polymorphic issue assignment and timeline activity actors (`assignee_type: "user" | "agent"`).

**Tradeoffs**
- Gained: Sub-second regression reopening upon recurrence in newer release versions, zero ingestion throughput degradation.
- Made harder: Must maintain regression version comparison logic in both Go (`processor-go`) and TypeScript query helpers.
- Reconsider: If complex issue linkage graph traversals need to be evaluated during real-time event filtering.

**Future mistake prevented**
Querying or locking relational issue graphs on the high-throughput error ingestion hot path.

**Update (2026-07-29) — the detector was structurally incapable of firing; both root causes are now fixed**
This decision was recorded (and left `Active`) while `detectAndHandleRegression`'s two prerequisites
were both broken:
1. `event.Deserialize` never copied the proto's `ReleaseVersion` field onto the internal `Event`, so
   `UpsertIssue`'s regression comparison always ran against an empty string.
2. The regression branch's `issue_activity` insert targeted a `metadata` column that does not exist on
   that table (`packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql:83-92`
   defines `old_value`/`new_value`, not `metadata`) — SQLSTATE 42703 aborted the whole transaction, so
   every regression event lost both the issue update and its occurrence, and `errors.go` classified
   42703 as retryable, so it burned the full NATS delivery budget before dead-lettering instead of
   failing fast.

Both are fixed: `ReleaseVersion` is proto field 15, copied by `Deserialize`
(`apps/processor-go/event/event.go:61-91`) and read before `Normalize` runs, per B6; the
`issue_activity` insert now writes `new_value` (`apps/processor-go/store/store.go:164-168`).

**Evidence (runtime proof, 2026-07-29)**: a regression event produced `issues.regression_status =
'regressed'`, `regression_count = 1`, and an `issue_activity` row with
`new_value = {"releaseVersion":"3.0.0","previousResolvedVersion":"2.5.0"}` (VERIFIED_STATE.md U11).

**Residual gap — not fixed**: `issue_activity.old_value` is left `NULL` on every regression insert
(`apps/processor-go/store/store.go:164-168` inserts only `new_value`). The schema
(migration `1721900000`, lines 83-92) pairs `old_value`/`new_value` to record a before/after transition;
right now only the "after" half is captured, so a transition's prior state (e.g. which version it was
last resolved in, before this regression cleared it) is reconstructable only via `previousResolvedVersion`
inside `new_value`'s JSON, not via the column the schema provisioned for it.

**Evidence**
- Implementation: `apps/processor-go/store/store.go`, `apps/dashboard-web/src/lib/db/queries/issues.ts`, `packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql`
- Fix: `apps/processor-go/event/event.go` (ReleaseVersion copy, read-before-Normalize), `apps/processor-go/store/store.go:159-168` (issue_activity column fix)

**Where to look next**
`apps/processor-go/store/store.go` and `apps/dashboard-web/src/lib/db/queries/issues.ts`.

---

### 2026-07-25 - Non-Blocking Dual-Endpoint Client SDK Protocol with Auto-Initialization & Context-Aware Telemetry Correlation

**Status**
Active

**Why this is durable**
Client SDKs in high-throughput applications must guarantee zero execution latency penalties (< 50 µs) and non-blocking caller execution, while preserving OpenTelemetry trace context and sanitizing client-side PII.

**Decision**
1. **Dual Endpoint Ingestion**: `ingestor-go` supports both single-event (`POST /ingest`) and array batch (`POST /ingest/batch`) ingestion endpoints to reduce network request count by up to 90% during error spikes.
2. **Lock-Free Channel Transport**: SDK uses a buffered Go channel (`chan *Event`) with non-blocking `select` push and FIFO eviction on ring buffer overflow (`MaxBufferSize`).
3. **Auto-Initialization Fallback**: If uninitialized code invokes `CaptureError`, default configuration auto-initializes without panicking.
4. **Context-Aware Telemetry Wiring**: `CaptureErrorContext(ctx, err)` automatically extracts W3C OpenTelemetry `trace_id`/`span_id` from `ctx` and applies context tag helpers (`WithUser`, `WithTenant`, `WithTag`).
5. **Logger Framework Sub-Packages**: Zero-code-change logger integration via `sentinelslog` (`slog.Handler`), `sentinelzerolog` (`zerolog.Hook`), and `sentinellog` (`io.Writer`).

**Tradeoffs**
- **Gained**: Sub-50µs execution overhead, 90% HTTP request reduction, automatic trace correlation.
- **Made harder**: Slightly higher memory usage for buffered channels (up to 10 MB per process).
- **Reconsider**: If client applications require disk-backed persistence during multi-day offline disconnects.

**Future mistake prevented**
Blocking application execution threads during error capture or leaking PII in metadata.

**Evidence**
- Specification: `docs/sdk-specification.md`, `specs/007-go-client-sdk/spec.md`
- Implementation: `packages/sdk-go/`, `apps/ingestor-go/main.go`

**Where to look next**
`docs/sdk-specification.md` and `packages/sdk-go/`.

---

### 2026-07-26 - Dual-Layer Multi-Tenant API Key Authentication with NATS Invalidation & Hierarchical Sliding-Window Rate Limiting

**Status**
Active

**Why this is durable**
Protects Sentinel ingestion hot path (< 1ms overhead) from authentication latency and denial-of-service memory amplification while maintaining immediate (< 100ms) cache invalidation upon key revocation across distributed ingestor nodes.

**Decision**
Store only SHA256 hashed API key digests (`key_hash`) in `project_api_keys` with raw secret tokens (`sent_live_...` or `sent_org_...`) displayed ONCE upon creation. Support both Project-scoped and Organization-wide API keys (`organization_id` bound, nullable `project_id`). Cache valid keys in Redis/in-memory LRU with a 60-second TTL, listening to NATS JetStream `api_key.invalidated` events for instant revocation cache purging. ~~Enforce hierarchical rate limits (per-key quota overrides project default 5,000 RPM)~~ **[false, see Correction below — rate limiting is per-key only]** via Redis sliding window counters wrapped inside authentication middleware.

**Tradeoffs**
- **Gained**: Sub-millisecond error ingestion auth, zero plaintext secret storage, instant cross-node revocation, and granular per-key rate limit overrides.
- **Made harder**: Standalone single-node deployments without Redis/NATS rely on a 60-second in-memory LRU TTL window for key revocation propagation.
- **Reconsider**: If standalone non-Redis deployments require instant revocation without NATS broker overhead.

**Future mistake prevented**
Nesting rate-limiting middleware outside authentication middleware (exposing rate limit caches to unauthenticated DoS amplification) or storing plaintext API key tokens in database tables.

**Correction (2026-07-29) — "hierarchical" was never true; record it as unimplemented**
There is no project rate-limit tier and no `projects.rate_limit_rpm` column. Verified: `rtk grep -rn
"rate_limit_rpm" apps/ingestor-go packages/db-migrations/migrations` finds exactly one column,
`project_api_keys.rate_limit_rpm INTEGER NOT NULL DEFAULT 5000`
(`packages/db-migrations/migrations/1722000000_add_api_key_management.sql:17`), read per-key in
`auth/apikey.go:143-148` and carried through `auth.WithIdentity` /
`auth.RateLimitRPMFromContext`. Rate limiting is **per-key only**; "overrides project default" describes
a tier that was never built. If org- or project-level quotas are wanted, they are new work, not a bug
fix.

**Correction (2026-07-29) — rate limiting was silently 100% bypassed until R1 (self-inflicted, same changeset) fixed it**
Introducing typed context keys in `auth/context.go` (replacing bare string keys, itself a good change —
Go vet flags string-literal context keys, and they collide silently across packages) broke
`middleware/ratelimit.go`, which still read the old bare string keys (`"api_key_hash"`,
`"rate_limit_rpm"`). `context.Value` compares the dynamic type as well as the value, so
`r.Context().Value("api_key_hash").(string)` always failed its type assertion and `ok` was always
`false` — every request took the `next.ServeHTTP(w, r); return` early-out and rate limiting did not run,
for any key, at any RPM, with no error or log. Three test files hand-injected the bare string keys
directly into `context.Background()`, matching the (broken) production reads, so nothing caught it.
Fixed by routing both middleware and tests through `auth.APIKeyHashFromContext` /
`auth.RateLimitRPMFromContext` / `auth.WithIdentity` (`apps/ingestor-go/middleware/ratelimit.go:33,39`).
**Proven**: 12 requests against a limit of 5 → `202 202 202 202 202 429 429 429 429 429 429 429`, with
`Retry-After: 60`, `X-RateLimit-Limit: 5`, `X-RateLimit-Remaining: 0`.

**Correction (2026-07-29) — org-wide key resolution order, previously undocumented**
For an organization-wide key (`project_api_keys.project_id IS NULL`), the target project is resolved in
this order, entirely inside the key's own organization (`apps/ingestor-go/main.go:317-376`,
`applyAuthenticatedScope`):
1. `X-Project-Key` header, resolved by the auth middleware. If present, it **is** the target; the
   body's `project_key` is never consulted, even if also present.
2. Body `project_key`, resolved via `auth.ResolveProjectInOrg`, only if the header was absent.
3. A name that does not resolve within the key's organization is `403`, not a cross-tenant write — this
   is what closed S6.

For a project-scoped key, the project is fixed by the credential; the body may omit `project_key` or
name the same project, but naming a **different** one is `403` (`main.go:365-368`).

**Known gaps not addressed by this correction** (see VERIFIED_STATE.md for detail, out of scope here):
S7 (`expires_at` never checked; rotation leaves `status='active'`) and S10 (rate limiting is still 4
unpipelined Redis round-trips — `ZRemRangeByScore`/`ZCard`/`ZAdd`/`Expire` are not pipelined — and
`middleware/ratelimit.go:44-47`'s `if rl.client == nil { next.ServeHTTP(...); return }` still fails
open with no rate limiting applied when Redis is unset) remain open and are unrelated to the corrections
above.

**Evidence**
- Implementation: `apps/ingestor-go/auth/apikey.go`, `apps/ingestor-go/auth/context.go`, `apps/ingestor-go/middleware/ratelimit.go`, `apps/ingestor-go/main.go`, `apps/dashboard-web/src/lib/db/queries/apikeys.ts`, `packages/db-migrations/migrations/1722000000_add_api_key_management.sql`
- Specification & Plan: `specs/008-api-key-management/spec.md`, `specs/008-api-key-management/plan.md`

**Where to look next**
`apps/ingestor-go/auth/apikey.go`, `apps/ingestor-go/auth/context.go`, `apps/ingestor-go/middleware/ratelimit.go`, `apps/ingestor-go/main.go` (`applyAuthenticatedScope`), `apps/dashboard-web/src/lib/db/queries/apikeys.ts`.


---

## D10 | Bounded-Retry NATS Delivery with Dead-Letter Capture

**Status**: active · **Recorded**: 2026-07-29 · **Tags**: nats,jetstream,reliability,delivery,dlq

**Context**
JetStream is an at-least-once transport: it redelivers until the consumer acks or terminates. The
processor previously did neither correctly — on any handler error it issued a bare `msg.Nak()` with no
delivery cap and no dead-letter path, and `scripts/nats-init.sh` created the consumer with `--defaults`
(unlimited redeliveries). A single permanently-unstorable message therefore redelivered forever and,
being pulled back into every Fetch batch ahead of newer messages, starved the whole pipeline. Measured
during the P2b recovery: ~510 unique events produced 5,874 processing attempts, and no newly-published
event was ever observed reaching Postgres (VERIFIED_STATE.md S13).

No decision record covered delivery semantics at all, so this behavior was invented during a bug fix.
This entry exists so it stops being accidental.

**Decision**
The pipeline offers **at-least-once delivery into the stream, bounded retry out of it, and never a
silent drop**. Concretely:

1. **Failures are classified.** `nats.Permanent(err)` marks a content-caused failure — malformed
   payload, check-constraint or foreign-key violation, a lookup that can never succeed. Everything else
   (connection refused, context deadline, partition) is retryable. Handlers MUST NOT mark infrastructure
   failures permanent: those are precisely what redelivery and the degradation buffer (D1) recover from.
2. **Retryable failures back off**: 1s, 5s, 15s, 30s, 60s via `NakWithDelay`, bounded by
   `MaxDeliver` (default 5). A bare Nak redelivers immediately and burns the entire budget in
   milliseconds, which would dead-letter an event long before a restarting database came back.
3. **Permanent failures skip the retry budget** and dead-letter on the first delivery.
4. **`Term()` is called ONLY after the event is captured somewhere durable.** Term is the application
   explicitly waiving JetStream's at-least-once guarantee for one message; waiving it before the event
   is stored is unrecoverable loss. If the DLQ publish fails, the message is Nak'd and left in the
   file-backed source stream instead. That cannot livelock: server-side `MaxDeliver` stops redelivery on
   its own, leaving the message parked-but-preserved.
5. **The DLQ is `<STREAM>_DLQ`, file-backed, unlimited retention**, carrying `X-Sentinel-Dlq-Reason`,
   `-Attempts` and `-Source-Subject` headers.

**Tradeoffs**
- **Gained**: no poison-message livelock; transient database faults survive a ~51s retry window; no
  event is dropped without being stored first.
- **Made harder**: a dead-lettered event is *preserved but unprocessed*. Durability moved from a stream
  with a live consumer to one with none.
- **Reconsider**: if the DLQ needs its own retention limits, or if per-subject delivery budgets diverge.

**Known gap — this decision is not fully honored yet**
**Nothing consumes the DLQ.** There is no drain, no replay, no alerting, and no dashboard surface, so a
dead-lettered event is invisible to the product. `Subscriber.DLQPublishFailures()` exists but is not
exported to any metric. Until a replay path exists, "no event loss" is true in the storage sense and
false in the useful sense — an error tracker that silently parks its own errors. Minimum work to close
it: (a) surface DLQ depth on `/health` or a metric, (b) a replay CLI that re-publishes onto
`error_events` once the root cause is fixed, (c) a "failed events" view in the dashboard.

**Future mistake prevented**
Reintroducing a bare `Nak()` (unbounded immediate redelivery → livelock), or terminating a message
whose DLQ publish failed (silent event loss). Also: classifying a database outage as `Permanent`, which
would dead-letter every in-flight event during a restart.

**Evidence**
- Implementation: `packages/shared-go/nats/subscriber.go` (`handleMessage`, `deadLetter`,
  `retryBackoff`, `ensureConsumer`), `apps/processor-go/service/errors.go`, `apps/processor-go/main.go`
- Runtime proof: after the fix, `ERROR_EVENTS` showed `Redelivered=0` across ~180 messages with
  `ERROR_EVENTS_DLQ` holding the unstorable ones, and events posted behind a poison message landed
  within ~2s (previously never).

**Where to look next**
`packages/shared-go/nats/subscriber.go`, `apps/processor-go/service/errors.go`, `scripts/nats-init.sh`
(whose unconditional `--defaults` consumer add can still race and revert `MaxDeliver`).

---

## D11 | APIKey/ProjectKey Split — a Project Name Is Never a Credential

**Status**: active · **Recorded**: 2026-07-29 · **Tags**: sdk,go,auth,multitenancy,contracts

**Context**
Before this fix, `packages/sdk-go`'s `Config` had a single field, `ProjectKey`, which callers were
expected to set to their API key, and which the SDK also sent as the wire body's `project_key`. The
ingestor resolves `project_key` against `projects.name` (`apps/ingestor-go/validation/validator.go:26`,
`apps/processor-go/store/store.go` project lookup) — a human-chosen, non-secret, per-organization-unique
name — never against a credential. So every event sent by every Go SDK user was accepted at the HTTP
layer (202), then the async processor's project-name lookup failed to find a project named "the
customer's actual API key string", and the event was permanently dead-lettered
(`classifyProjectLookupError`, this file's D10) — with no error ever surfaced to the caller, because
`sendBatch` did not inspect the response status at all (S4, VERIFIED_STATE.md). This is VERIFIED_STATE.md
S16: **the official SDK's happy path silently discarded 100% of events**, and nothing in the SDK's own
test suite caught it because no test round-tripped a real ingestor.

No decision record had ever stated, in one place, that a project name and an API key are different kinds
of value with different trust levels and different transport locations. That gap is what let one field
do both jobs.

**Decision**
A project's identity on the wire and a project's authorization credential are two different values that
MUST NOT share a field, in either the SDK or the server:
1. **`project_key` (wire body field, `projects.name`)** is a human-chosen, unique-per-organization
   *name* — an identifier, not a secret. It is safe to log, safe to put in a request body, and its only
   job is telling an organization-wide credential which of the org's projects an event belongs to.
2. **The API key (`sent_live_...` / `sent_org_...`)** is the secret credential. It travels ONLY in the
   `X-API-Key` header, NEVER in a body field, and is what D9's hashing/lookup/rate-limit machinery keys
   on.
3. In `packages/sdk-go`, these are separate `Config` fields: `APIKey` (`json:"-"`, never serialized) and
   `ProjectKey` (`json:"project_key"`). `Config.Validate()` additionally rejects a `ProjectKey` value that
   *looks like* an API key (`looksLikeSecret`, matching `sent_live_`/`sent_org_`/`pk_live_`/`sk_`
   prefixes) so a caller who swaps the two fields gets a loud config-time error instead of a silent
   100%-drop rate identical to S16's.
4. Server-side, tenant scope is still derived from the authenticated credential context, never trusted
   from the body outright — `project_key` in the body only *selects* which project inside the
   credential's own organization for an org-wide key, per D9's resolution order, and is rejected with 403
   on any mismatch for a project-scoped key. This is what B7 requires.

**Tradeoffs**
- **Gained**: an SDK config-time error (`Validate()`) instead of a runtime, per-event, silent data-loss
  failure mode; the field names now match what the server does with them, closing the semantic gap that
  caused S16.
- **Made harder**: a breaking SDK config change — any caller still setting only `ProjectKey` to their API
  key must migrate (the CHANGELOG documents this as a v0.2.0 breaking change).
- **Reconsider**: if a future auth scheme needs the project name to also carry authorization weight (it
  should not — that would resurrect this exact bug class).

**Future mistake prevented**
Naming a credential and a public identifier the same thing, or trusting either an SDK field name or a
request body field to imply where a value is safe to log or transmit.

**Evidence**
- Implementation: `packages/sdk-go/config.go` (`APIKey`, `ProjectKey`, `looksLikeSecret`, `Validate`),
  `apps/ingestor-go/main.go` (`applyAuthenticatedScope`, this file's D9)
- `packages/sdk-go/CHANGELOG.md` documents the breaking change
- Cross-reference: VERIFIED_STATE.md S16, S4; this file's B7 (`docs/memory/BUGS.md`), D9, D10

**Where to look next**
`packages/sdk-go/config.go`, `apps/ingestor-go/main.go:317-376` (`applyAuthenticatedScope`),
`apps/ingestor-go/validation/validator.go:26`.
