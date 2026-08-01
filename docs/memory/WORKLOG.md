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

---

### 2026-07-26 - Shipped Multi-Tenant Auth & API Key Management

- **Why durable**: Sentinel now features full multi-tenant organization API key management, dual context creation UI (Org Settings vs. Project Settings), instant NATS JetStream key revocation, and hierarchical sliding-window rate limiting.
- **Future mistake prevented**: Nesting rate-limiting middleware outside authentication middleware (exposing rate limit caches to unauthenticated DoS amplification) or storing plaintext API key tokens in database tables.
- **Evidence**: `specs/008-api-key-management/tasks.md` (8/8 tasks completed) and `specs/008-api-key-management/ripple-report.md`.
- **Where to look**: `apps/ingestor-go/auth/apikey.go`, `apps/ingestor-go/middleware/ratelimit.go`, `apps/dashboard-web/src/lib/db/queries/apikeys.ts`, `packages/db-migrations/migrations/1722000000_add_api_key_management.sql`.

---

### 2026-07-28/29 - Recovery Work (P0, P1, P2, P2b): the Pipeline Persisted Data for the First Time

**Status**
Active — commit `cd84d17` (P0+P1) is on `main`; P2/P2b are staged, uncommitted as of this entry (36 files,
+2501/-254). This entry is written from that staged state; re-verify after it merges.

**Why this is durable**
Every milestone above this one in this file recorded a merge event, not verified behavior. The honest count:
`issues` and `error_occurrences` had **zero rows, ever**, for the life of the project, through 006 (issue
lifecycle), 007 (Go SDK), and 008 (API key management) all being marked Completed. This body of work is the
first time a POSTed event has been independently verified to reach Postgres as a row — checked live against
`sentinel-postgres` (`select count(*) from issues` / `error_occurrences` → non-zero) — and is written up
here without softening that this is a recovery from a broken baseline, not a new feature.

**What actually shipped (verified, not claimed)**
- **S1** `tests/unit` (11 files, ~2,800 lines, previously 0 tests run) now builds and passes: 241 assertions.
- **S2** `apps/dashboard-web` now builds; `pnpm check` 707 files / 0 errors; `pnpm test` 5 files / 19 tests.
- **S3** `/ingest` accepts well-formed events instead of rejecting 100% of them — fixed by *deleting* the
  redundant field-level `string.len` rule, not by switching to `max_len`; two overlapping validators with
  different semantics is how the bug hid for as long as it did.
- **S4/S16** the Go SDK and the ingestor now agree on wire field names and semantics; the SDK's `Config` now
  separates the secret (`APIKey`) from the project selector (`ProjectKey`) — previously one field did both
  jobs, so every real SDK event was accepted (202) and then silently dead-lettered.
- **S5** `release_version` survives normalization (moved to a first-class proto field, read before
  `Normalize` runs instead of out of the rewritten metadata map) — regression tracking (006's headline
  feature) can now actually fire; verified end to end (`regression_status='regressed'`, an `issue_activity`
  row with old/new release versions).
- **S6** cross-tenant writes closed: tenancy now derives from the authenticated credential
  (`applyAuthenticatedScope`), never the request body; verified 403 on both a project-scoped key naming
  another project and an org-wide key naming a project outside its own org.
- **S11** fingerprinting no longer collapses every error of a class in a project into one issue when no
  stack frame is marked in-app; the SDK now sets `in_app` and the processor falls back to the top 3 frames
  when none is set.
- **S12 (new defect, found and fixed this round)** two contradictory `CHECK` constraints on `issues.status`
  — one from the initial schema, never dropped, one from feature 006 — made every insert with the
  application's only status value (`'unresolved'`) fail. **This is the reason the database had zero rows for
  the life of the project**, and it was invisible until S3 stopped blocking the front door.
- **S13 (new defect, found and fixed this round)** a poison message with no `MaxDeliver` cap redelivered
  forever and starved the whole pipeline (~510 unique events produced 5,874 processing attempts). Now
  bounded retry + dead-letter queue (see `docs/memory/DECISIONS.md` D10).
- **S14 (new defect, found and fixed this round)** a second instance of the S3 bug class: several proto
  fields (`platform`, `environment`, `error_class`, `trace_id`, `span_id`, `release_version`) had no CEL
  length rule matching their bounded Postgres columns, so oversized values were accepted with 202 and then
  failed to store.
- **S17 (new defect, found and fixed this round)** the ingestor was hot-spinning at 55.8% CPU idle
  (a deadline-less `Fetch` returning instantly in a tight loop), flooding logs hard enough to suppress the
  whole pod's output. Now 0.08% idle.
- **R1 (self-inflicted by this round's own S6/B7 fix, caught before landing)** introducing typed context
  keys silently disabled rate limiting on 100% of requests, because a sibling middleware file still read the
  old bare-string keys and `context.Value` compares dynamic type. Caught and fixed within this round; see
  `docs/memory/BUGS.md`.
- Also landed: PII scrubbing gaps closed on both the SDK and server side (substring match instead of exact
  key match, so `user_password`/`x-api-key`-style keys are actually caught); a `packages/sdk-go` full
  contract test (`tests/contract/sdk_ingestor_test.go`, P2-4) that decodes the SDK's real wire output with
  the ingestor's real decoder; a `UNIQUE (organization_id, name)` constraint on `projects`.

**Future mistake prevented**
Trusting a `specs/*/spec.md` "Status: Completed" line, or an earlier entry in *this file*, as evidence of
runtime behavior — in both directions. **S12 was introduced by the 2026-07-25 "Issue Lifecycle Management &
Regression Tracking" entry above** (its migration added a second, conflicting `status` constraint without
dropping the first) and sat undetected for the entries after it, including 007 and 008 being marked
Completed. S13 is an older, pre-existing defect in the base NATS subscriber (a bare `Nak()` with no
`MaxDeliver`/DLQ, already noted as a lower-severity item in `VERIFIED_STATE.md` before this round) that no
earlier milestone's acceptance tests exercised. S17's origin is unverified — it surfaced in the same file
this round rewrote for S13/D10, and this entry does not claim to know whether it predates that rewrite or
was introduced by it. And this recovery is not exempt from the same failure mode either way: **S14
(part) and R1 were introduced by earlier phases of this very round** — S14 by this round's own P2-1
field-length change, R1 by this round's own S6/B7 tenancy fix — and were only caught because this round
happened to include end-to-end verification steps that earlier milestones' package-level tests never ran. A
green package-test suite proved nothing about whether a byte ever reached Postgres, then or now.

**Still open — not fixed by this round, do not imply otherwise**
S7 (key revocation/expiry), S8 (alerts/notifiers never constructed), S9 (degradation buffer's `CheckAndBuffer`
returns `true` for two different reasons — see `docs/memory/BUGS.md`), S10 (rate limiting still 4
unpipelined Redis calls; nil-client fail-open path still exists even though Redis is now in compose), S15
(SDK partial-batch handling — unverified, the agent that may have shipped it did not report), the
dashboard↔ingestor NATS key-invalidation contract (the other half of B5), `issue_activity.old_value` left
NULL, `scripts/db/init.sql` still a third hand-maintained schema frozen pre-S12, and the DLQ D10 introduced
has no consumer, drain, or dashboard surface yet.

**Evidence**
`docs/memory/VERIFIED_STATE.md` (S1–S17, R1 — the command that proved each one), `docs/memory/BUGS.md`,
`docs/memory/ARCHITECTURE.md` (wire contract and tenancy resolution entries), `docs/memory/DECISIONS.md` D10,
`docs/plans/E2E_RECOVERY_PLAN.md`. Commit `cd84d17` (P0+P1); P2/P2b staged at time of writing.

**Where to look**
`apps/ingestor-go/main.go`, `apps/ingestor-go/auth/`, `apps/processor-go/service/`,
`packages/shared-go/nats/subscriber.go`, `packages/sdk-go/`, `tests/contract/sdk_ingestor_test.go`,
`packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql`.


---

### 2026-08-01 - A Feature Can Merge Green, Be Reviewed, and Still Never Run — Now With a Green CI to Prove Otherwise

- **Why durable**: Five features closed the dashboard↔backend parity gaps, each merging with its own
  tests passing. **Three of the five did not execute at runtime**: an emailed invite link 403'd for
  signed-in users (a missing entry in `reservedRoutes`), issue search 500'd on every query
  (`uuid ILIKE`, no cast), and only one alert rule per org/project ever fired (a `map[string]*Config`
  where the schema permits many). This is B3 relocated from the pipeline to the dashboard, and it means
  "reviewed and merged" carries no information about whether code runs. The durable output is not the 47
  fixes — it is the set of gates and habits that would have caught them: run **every** gate CI runs
  (B12), make each fix fail before it passes, and never trust a suite you have not shuffled (B13).
- **Future mistake prevented**: Declaring a dashboard feature complete on the strength of `pnpm check` +
  `pnpm test`. Neither sees SvelteKit's route-export rule (only `pnpm build` does), neither notices a
  test passing on ambient data, and neither can tell a real assertion from a vacuous one — deleting all
  three `.for('update')` calls from the sole-owner guard left 29/29 tests green. Also prevents trusting
  a CI config that has never actually executed: this repo's CI existed for days before its first real
  run, and that first run was red.
- **Evidence**: PR #11 → merge commit `b895df1`, the **first green CI run on `main`** (`gh run list
  --branch main` records the pre-work baseline `b9e2018` as `failure`). Gates at that commit:
  `pnpm check` 1024 files / 0 errors / 0 warnings, `pnpm build` pass, `pnpm test` 251 passed and
  order-independent across 8 shuffle seeds, `go test ./tests/unit/...` 308 passed,
  `SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/` 76 passed, 9/9 CI check runs green (8 jobs). The remediation
  introduced three defects of its own (a 500 behind a type cast, a broken `pnpm build`, an over-strict
  access model) — each caught by a gate rather than by review, which is the point.
- **Where to look**: `docs/plans/UI_PARITY_REMEDIATION_PLAN.md` (the 47-finding register, D01–D47 —
  note these are *findings*, unrelated to `DECISIONS.md`'s D1–D18 *decisions*),
  `docs/memory/VERIFIED_STATE.md` → "UI parity remediation", `docs/memory/BUGS.md` B10 addendum, B12,
  B13, `docs/memory/DECISIONS.md` D17 and D18.
