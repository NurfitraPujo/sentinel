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
Active

**Why this is durable**
Sentinel is an observability platform. Losing events during a temporary database outage defeats the purpose of the platform. This decision ensures that short-term infrastructure issues don't lead to permanent data loss.

**Decision**
When the PostgreSQL database is unavailable, the Processor service MUST buffer incoming events in memory up to a limit (MaxBufferSize = 10,000 events). These events MUST be flushed to the database automatically once connection is restored.

**Tradeoffs**
- **Gained**: High availability and data persistence during temporary outages.
- **Made harder**: Memory management in the Processor service. A long-term outage could lead to OOM if the buffer is too large or if backpressure is not applied.
- **Reconsider**: If Sentinel moves to a multi-tenant model where memory limits must be strictly partitioned, or if the buffer size needs to be dynamic.

**Future mistake prevented**
Directly failing or dropping events when the database is down.

**Evidence**
Implementation in `apps/processor-go/degradation/buffer.go`.

**Where to look next**
`apps/processor-go/processor.go` and `apps/processor-go/degradation/buffer.go`.

---

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

**Evidence**
- `specs/004-shared-db-migrations/spec.md` → Edge Cases
- `specs/004-shared-db-migrations/plan.md` → Technical Context Constraints
- `specs/004-shared-db-migrations/security-constraints.md` → Confirmed Secure Patterns (transactional DDL, concurrency locking)

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

**Evidence**
- Implementation: `apps/processor-go/store/store.go`, `apps/dashboard-web/src/lib/db/queries/issues.ts`, `packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql`

**Where to look next**
`apps/processor-go/store/store.go` and `apps/dashboard-web/src/lib/db/queries/issues.ts`.
