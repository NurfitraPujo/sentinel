#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.specify/extensions/memory-md"

# ---------- A1 ----------
npx --no-install speckit-memory register-memory \
  --id A1 \
  --title "Unified Migration Directory Boundary" \
  --tags "migrations,architecture,monorepo,postgres" \
  --file ARCHITECTURE.md \
  --status active \
  --content "$(cat <<'A1_EOF'
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
A1_EOF
)"

# ---------- D3 ----------
npx --no-install speckit-memory register-memory \
  --id D3 \
  --title "Adopt Goose for All Database Migrations" \
  --tags "migrations,tooling,goose,go" \
  --file DECISIONS.md \
  --status active \
  --content "$(cat <<'D3_EOF'
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
D3_EOF
)"

# ---------- D4 ----------
npx --no-install speckit-memory register-memory \
  --id D4 \
  --title "Strict Loud-Failure Migration Policy" \
  --tags "migrations,errors,concurrency,policy" \
  --file DECISIONS.md \
  --status active \
  --content "$(cat <<'D4_EOF'
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
D4_EOF
)"

# ---------- D5 ----------
npx --no-install speckit-memory register-memory \
  --id D5 \
  --title "Production Safety Guardrails for Destructive Migration Tasks" \
  --tags "migrations,security,ci,operations" \
  --file DECISIONS.md \
  --status active \
  --content "$(cat <<'D5_EOF'
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
D5_EOF
)"

# ---------- W2 (prepend) ----------
npx --no-install speckit-memory register-memory \
  --id W2 \
  --title "Shared DB Migrations Foundation Shipped" \
  --tags "milestone,migrations,architecture" \
  --file WORKLOG.md \
  --status active \
  --prepend \
  --content "$(cat <<'W2_EOF'
### 2026-07-17 - Shared DB Migrations Foundation Shipped

- **Why durable**: Sentinel now has a single, enforced boundary for schema evolution across all apps. The architectural invariant (unified migrations directory, loud-failure policy, prod-safety guardrails) will outlive the feature that introduced it.
- **Future mistake prevented**: A future contributor adding per-app migration subdirectories, bypassing `goose` for ad-hoc SQL, or removing the `ENVIRONMENT=prod` guard from destructive Taskfile targets.
- **Evidence**: `a1255dc feat(db-migrations): complete 004-shared-db-migrations feature`. 28/28 tasks complete. Integration tests in `tests/integration/db_migrations_test.go` cover all targets.
- **Where to look**: `packages/db-migrations/`, `Taskfile.yml`, `specs/004-shared-db-migrations/architecture-migration-plan.md`, `docs/memory/ARCHITECTURE.md` → Database Schema Management.
W2_EOF
)"

echo "OK: 5 register-memory calls completed"
