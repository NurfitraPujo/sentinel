# Worklog

Use concise high-value entries only.
This is not a changelog. Do not record routine releases, version bumps, or implementation summaries.

---

### 2026-07-17 - Shared DB Migrations Foundation Shipped

- **Why durable**: Sentinel now has a single, enforced boundary for schema evolution across all apps. The architectural invariant (unified migrations directory, loud-failure policy, prod-safety guardrails) will outlive the feature that introduced it.
- **Future mistake prevented**: A future contributor adding per-app migration subdirectories, bypassing `goose` for ad-hoc SQL, or removing the `ENVIRONMENT=prod` guard from destructive Taskfile targets.
- **Evidence**: `a1255dc feat(db-migrations): complete 004-shared-db-migrations feature`. 28/28 tasks complete. Integration tests in `tests/integration/db_migrations_test.go` cover all targets.
- **Where to look**: `packages/db-migrations/`, `Taskfile.yml`, `specs/004-shared-db-migrations/architecture-migration-plan.md`, `docs/memory/ARCHITECTURE.md` → Database Schema Management.

### 2024-05-20 - Adopted CEL for Protobuf Validation

- **Why durable**: Validation logic traditionally drifted between the Go Ingestor and any potential future clients. CEL allows embedding validation rules directly in the schema.
- **Future mistake prevented**: Mismatched validation logic between producers and consumers.
- **Evidence**: `packages/proto/error_event.proto` uses `buf.validate.message` with CEL expressions.
- **Where to look**: `packages/proto/error_event.proto`

## Template

### YYYY-MM-DD - Summary

- why this is durable
- what future mistake it prevents
- evidence
- where future contributors should look

## Example

### 2026-03-15 - Pagination cursor must be opaque to clients

- **Why durable**: three features so far have tried to expose raw database offsets as pagination cursors, each time creating breaking changes when the underlying query changes
- **Future mistake prevented**: next time a feature adds pagination, the implementer will know to use opaque cursors from the start
- **Evidence**: specs 018, 024, and 031 all required pagination rework; see DECISIONS.md entry on API pagination
- **Where to look**: `src/api/pagination.ts`, `docs/memory/DECISIONS.md`

## Counter-Example (do not write entries like this)

> ### 2026-03-15 - Updated pagination
>
> - Changed pagination to use cursors
> - Deployed to staging

This is a changelog entry, not a durable lesson. It records what happened, not what was learned.

### 2026-07-24 - Shipped Organization Layer & Multi-Tenancy Support

- **Why durable**: Sentinel now supports multi-tenant organization hierarchy, server-side context routing, RBAC role inheritance (`owner`, `admin`, `engineer`, `support`, `viewer`), header org switcher, and project navigation components under `005-organization-layer`.
- **Future mistake prevented**: Building features without explicit multi-tenant organization boundaries or context scoping.
- **Evidence**: `specs/005-organization-layer/tasks.md` (9/9 tasks completed) and `specs/005-organization-layer/ripple-fixes.md`.
- **Where to look**: `apps/dashboard-web/src/lib/db/queries/organizations.ts`, `apps/dashboard-web/src/hooks.server.ts`, `packages/db-migrations/migrations/1721800000_add_organization_layer.sql`.

---

### 2026-07-25 - Shipped Issue Lifecycle Management & Regression Tracking

**Status**
Active

**Why this is durable**
Marks the completion of the active triage platform milestone (006-issue-lifecycle-management).

**Decision**
Shipped issue status triage, polymorphic AI/human assignees, bulk triage API, real-time Go regression detection, and Many-to-Many issue relations.

---

### 2026-07-25 - Shipped Official Go Client SDK (`packages/sdk-go`) and High-Throughput Ingestor Batch API

- **Why durable**: Sentinel now has an official Go Client SDK (`packages/sdk-go`) adhering to the Sentinel SDK Protocol Specification (`docs/sdk-specification.md`), with batch HTTP ingestion support (`POST /ingest/batch`) in `ingestor-go`.
- **Future mistake prevented**: Creating ad-hoc unstandardized error reporting clients or making blocking HTTP calls on caller threads during error capture.
- **Evidence**: `specs/007-go-client-sdk/tasks.md` (6/6 tasks completed) and `specs/007-go-client-sdk/ripple-report.md`.
- **Where to look**: `packages/sdk-go/`, `apps/ingestor-go/main.go`, `docs/sdk-specification.md`.
