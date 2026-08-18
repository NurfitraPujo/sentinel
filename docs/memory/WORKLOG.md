# Worklog

Use concise high-value entries only.
This is not a changelog. Do not record routine releases, version bumps, or implementation summaries.

---

### 2026-08-18 - N10 part 1: per-project agent settings + repo connections (server-side worker policy)

- **Why durable**: FIX authorization for the N8 sentinel-worker is now per-project, server-side,
  default OFF — `project_agent_settings` + `project_repo_connections` (PK-on-project_id = one repo
  per project v1), managed in the project settings UI under `manage_agents` RBAC with every
  mutation audited, and delivered to workers on `GET /api/agent/projects` as
  `agentSettings {fixEnabled, maxPrsPerDay, repo|null}`. Onboarding a repo never requires a worker
  redeploy; `WORKER_FIX_ENABLED` is only a kill switch. Proofs in VERIFIED_STATE.md "N10 part 1".
- **Future mistakes prevented**:
  - `test_cmd`/`agent_cmd` are server-stored commands the worker executes — a DELIBERATE,
    documented acceptance (DECISIONS.md D23: cloned-repo tests already run repo-controlled code;
    the fix-container sandbox is the boundary; RBAC + audit are the controls). Do not "fix" this
    by stripping them from the agent API.
  - The encrypted git-credentials store is a SEPARATE sibling schema — never add credential
    columns to `project_repo_connections`.
  - Cross-worktree podman compose `--force-recreate` can REMOVE dependent services and hang
    without recreating them (details in VERIFIED_STATE.md N10 entry); recover with `--no-build`
    plain `up -d` per service.

---

### 2026-08-14 - Agent-native layer (events feed, webhooks, batch, sentinel CLI) — provider-agnostic

- **Why durable**: Sentinel is now operable by ANY external AI agent (Claude/GPT/Gemini/scripts)
  end-to-end: discover work via `GET /api/agent/events` (seq cursor over `issue_activity`), read
  occurrences/reports, claim/comment/status/link/question, compose ops via `POST /api/agent/batch`,
  receive pushes via HMAC-signed webhooks, all through the org-scoped Bearer surface. Canonical
  docs live in `docs/agents/SENTINEL_AGENT_GUIDE.md` + `.agents/skills/sentinel-agent/SKILL.md`
  (`.claude` shim points there). Details + proofs: VERIFIED_STATE.md "Agent-native layer (N1–N5)".
- **Future mistakes prevented**:
  - `issue_activity.seq` is an IDENTITY column: gaps and brief commit-order inversion are normal.
    Every consumer (feed, dispatcher) MUST keep the 2s `created_at` lag guard; removing it silently
    loses events to cursor advancement. Contract is at-least-once; consumers dedupe by seq.
  - `agent_webhooks.secret` is plaintext BY DESIGN (server signs outbound HMAC); do not "fix" it to
    a hash — that breaks signing. Signature: `t=<unix>,v1=hmac-sha256(secret, t + "." + body)`.
  - Webhook cursor advance is CAS (`WHERE last_delivered_seq=$old`) so replicated processors never
    double-deliver; failure paths must never advance the cursor.
  - The dispatcher payload must stay field-for-field identical to `queries/events.ts` (B5 — no
    compiler crosses that boundary); `issues.message` maps to `issue.title`.
  - `tools/sentinel-cli` is deliberately NOT in `go.work` (stdlib-only, own CI job, `GOWORK=off`);
    adding it to the workspace would couple it to root-module hygiene for no benefit.
  - Batch has NO outer transaction on purpose (partial completion == N sequential calls); wrapping
    it in one would change claim-conflict semantics agents already rely on.
- Dispatcher ships gated OFF (`WEBHOOK_DISPATCH_ENABLED=false`), same posture as the DLQ drainer.

### 2026-08-13 - Production deployment artifacts (Helm chart, prod compose, migration image)

- **Why durable**: First production deployment surface for the repo — a Helm chart
  (`deploy/helm/sentinel`), a hardened `docker-compose.prod.yml`, and a dedicated migration image.
  The invariants encoded here outlive the artifacts: migrations run **direct-to-Postgres, never
  through PgBouncer** (DDL and transaction pooling don't mix), and presigned uploads need
  `S3_PUBLIC_ENDPOINT` set to a **browser-reachable** host because SigV4 signs the host into the URL
  (the in-cluster `minio:9000` the server uses is unreachable from the browser).
- **Future mistake prevented**: The migration image (`packages/db-migrations/Dockerfile`) reads its
  `.sql` files **from disk at runtime — they are NOT embedded in the binary**. The image therefore
  ships both the binary and `packages/db-migrations/migrations/`, laid out under WORKDIR `/app` so the
  CLI's default path resolution finds them; its build context is the module dir, not the repo root.
  A future dev adding a migration must **rebuild and republish this image** — a new `.sql` file that
  isn't in the image is invisible at deploy time.
- **Evidence**: PR #15 squash-merged to `main` as `908f491`. Migration image build-tested and
  run-tested against a throwaway Postgres (applies all migrations incl. M6 attachment status,
  idempotent across all three targets on the shared DB, redacts the DSN password in logs). Helm chart
  `helm lint` clean and server-side `kubectl --dry-run` validated for dev and prod value sets;
  `docker compose -f docker-compose.prod.yml config` parses. No application/runtime code changed.
- **Where to look**: `DEPLOYMENT.md` (runbook, incl. §0 build/publish and the two gotchas above),
  `deploy/helm/sentinel/`, `docker-compose.prod.yml`, `packages/db-migrations/Dockerfile`. TODO 03/07
  checklists reconciled against `VERIFIED_STATE.md`.

### 2026-08-12 - M6 presigned large uploads + toolbar editor (partial M6)

- **Why durable**: The presigned direct-to-bucket path establishes the pattern for "the server never
  sees the bytes at upload time" while keeping the magic-byte guarantee — validation moves to a
  `finalize` step that sniffs the stored object, and a `attachments.status` (`pending`→`ready`) gate
  makes an unvalidated object un-linkable. That gate (`claimDraftAttachmentsOnto` requires
  `status='ready'` in both pre-check and conditional UPDATE) is the load-bearing security invariant
  any future upload path must preserve.
- **Future mistake prevented**: Three real-MinIO signing failures that mocked unit tests cannot see,
  and that a future dev will hit again the moment they touch presigned S3: (1) **never sign
  Content-Type into a presigned PUT** and have the client send a **type-stripped Blob** — a signed
  Content-Type the client echoes, or any unsigned Content-Type, is a MinIO "headers present … which
  were not signed" rejection; (2) AWS SDK v3 ≥ ~3.729 **default integrity checksums** pollute
  presigned URLs — set `requestChecksumCalculation`/`responseChecksumValidation: 'WHEN_REQUIRED'`;
  (3) the SDK's **browser build mis-signs a ranged GetObject** against MinIO (vitest forces that
  build), so read the sniff bytes via a full GET whose stream you stop early, not `Range`. All three
  were caught only by the committed real-Postgres+MinIO integration flow, not by the (green) mocked
  unit tests — the same "mocks stay green while real infra breaks" lesson as M1's varchar(64) key.
- **Evidence**: branch `feat/manual-issues`; migration `1723100000_add_attachment_status.sql`;
  `reports.presign.flow.integration.test.ts` (`M6_PRESIGN_INTEGRATION_REQUIRED=1`) green; dashboard
  gates green (build, check 0/0, `pnpm test --sequence.shuffle` 531 passed), drift 26/26, e2e 76/0.
- **Where to look**: `docs/plans/M6_PRESIGNED_UPLOADS_AND_TOOLBAR_PLAN.md`,
  `docs/memory/VERIFIED_STATE.md` → "M6 (partial)", `src/lib/server/storage.ts` +
  `src/lib/server/upload-core.ts`, `src/lib/markdown-toolbar.ts`. Toolbar shipped dependency-free
  (Markdown-syntax buttons over the textarea); Tiptap WYSIWYG stays deferred by design.

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

### 2026-08-12 - Manual Issues M2–M5: the Whole Feature Shipped Phase-by-Phase, Each Phase Proven Against the Real Stack Before Commit

- **Why durable**: Four phases (attachments/MinIO, threads, notifications, agents) landed as four
  commits (`f3940f8`, `952d6b0`, `6abbf9e`, M5 next), each gated the same way: Sonnet implementors,
  Opus adversarial validators with fix loops, Fable holistic review, and a required-env integration
  flow test against the real compose stack before anything was committed. The pattern paid for
  itself repeatedly — every phase's only real bugs were ones ONLY the real stack could catch:
  M1's varchar(64) overflow, M3's app-clock-vs-DB-clock polling skew, M5's hardcoded release
  actorType. Mock-based suites stayed green through all of them.
- **Future mistake prevented**: (1) New one-shot compose services silently fail
  `scripts/wait-healthy.sh` unless added to its `ONESHOT_SERVICES` list (bit us with `minio-init`).
  (2) vitest's `resolve.conditions: ['browser']` makes `@aws-sdk/client-s3` load its browser build,
  which needs `Blob.prototype.arrayBuffer` missing in jsdom — integration tests must swap in Node's
  Blob before import. (3) A `?after=` polling cutoff must come from the DB clock (`select now()`),
  never the app clock. (4) The ingestor's scope check is an allowlist, which is why adding the
  `'agent'` key scope required zero Go changes — allowlists age well, denylists don't.
- **Evidence**: end state 2026-08-12 — dashboard gates 441 tests shuffled / check 0-0 / build green;
  full-stack `SENTINEL_E2E=1 -tags=e2e` 76 passed 0 failed every phase; four required-env
  integration flow tests (M2 attachments byte-identity+reaper, M3 threads incl. waiting_on
  round-trip, M4 notification lifecycle, M5 agent work-loop over real HTTP incl. a concurrent
  double-claim resolving to exactly one 200 + one 409 and an agent-key ingest attempt rejected 403).
- **Where to look**: `docs/plans/MANUAL_ISSUES_DESIGN.md` (phase table now all DONE through M5),
  `VERIFIED_STATE.md` M2/M3/M4/M5 entries, `DECISIONS.md` D19.

---

### 2026-08-11 - Manual (User-Reported) Issues M1: the Schema Was Waiting, and the Only Real Bug Was One Only a Real Database Could Catch

- **Why durable**: The `issue_type='user_report'` / `source_channel` / `assignee_type` /
  `issue_relations` machinery added by `1721900000` sat unused for weeks; M1 is the first feature to
  exercise it, and it fit with zero schema rework — evidence that speculative-but-typed schema (CHECK
  discriminators over separate tables) pays off when the feature finally lands. The full design and its
  12-entry grilled decision register live in `docs/plans/MANUAL_ISSUES_DESIGN.md` §0; the load-bearing
  ones: reuse `issues` (not a parallel table), per-org lazily-provisioned `is_inbox` Triage project so
  `project_id` stays NOT NULL, atomic conditional-UPDATE claims, strict `system_error` filtering of the
  existing dashboards.
- **Future mistake prevented**: Trusting mock-based query tests for anything Postgres itself enforces.
  All 19 mocked unit tests were green while `findOrCreateTriageProject` generated a 70-char placeholder
  key into `varchar(64)` — every first-use Triage provisioning would have thrown in production. Only the
  integration flow test (`reports.e2e-flow.integration.test.ts`, disposable Postgres, red-first) caught
  it. Column limits, CHECKs, and FK cascades are invisible to chainable-mock tests — same class as S3/S14.
- **Evidence**: gates at time of writing (uncommitted branch `claude/fervent-kalam-27b223`): all three
  dashboard gates green (279 tests, shuffled), `go test ./tests/unit/...` 308 passed, migration replayed
  2× + down/up on disposable Postgres, full-stack `SENTINEL_E2E=1 -tags=e2e` **76 passed / 0 skipped**
  after the change. Orchestration: Sonnet implementors, Opus adversarial validators per stage (3-round
  fix loops), Fable holistic review — the validator loop passed 4/4 stages first-round; the e2e stage
  found the one real bug.
- **Where to look**: `docs/plans/MANUAL_ISSUES_DESIGN.md`, `docs/memory/VERIFIED_STATE.md` → "Manual
  (user-reported) issues — M1", `packages/db-migrations/migrations/1722600000_*.sql`,
  `apps/dashboard-web/src/lib/db/queries/reports.ts`, `src/lib/server/report-access.ts`,
  `src/routes/[orgSlug]/reports/`.

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

---

### 2026-08-14 - Agent-Automation Audit Found the Product Working But Undiscoverable: an Adversarially-Verified Blocker List, and Four Durable Lessons From Closing It

- **Why durable**: A 5-lens audit of the freshly-merged agent-native layer (N1–N6) found no single
  blocker, but two majors — no `created`/repeat-occurrence events (A01/A02) — combined into a
  near-blocker: brand-new service errors were silently invisible to event-driven discovery, the exact
  use case the layer exists for. The audit's own method (every raw finding adversarially
  re-verified by a separate reviewer instructed to refute it) downgraded 7 of 22 raw findings and
  recalibrated several severities — treat "blocker" claims in any future audit as provisional until
  a refuter has tried to kill them; this one records both the finding AND the refutation note for
  each surviving item, not just the conclusion.
- **Future mistake prevented — events-feed coverage must be re-audited whenever a new issue-mutating
  writer is added.** A01's root cause was structural, not a bug: `StoreEvent`'s new-issue branch
  simply never wrote an `issue_activity` row, and the value that would have driven it
  (`IssueOutcomeNew`) was already computed and explicitly documented as "consumed by nobody"
  (store.go:309) — a correct-looking, fully-tested function whose result nobody downstream reads is
  a standing invitation for exactly this gap. Any future writer that mutates `issues`/creates new
  ones outside `StoreEvent` (a bulk-import path, an admin merge tool, a future ingestion source)
  must be checked against the events feed the same way, not assumed to inherit coverage.
- **Future mistake prevented — "claim" is advisory, not a lock, and that is a deliberate, documented
  contract, not a bug to eventually fix.** A11 found no mutation handler checks `assignedTo` against
  the caller; N7 did not add that check on purpose (agent-ops.ts's own handlers now log a structured
  `agent.mutated_claimed_issue` warning instead) — enforcing it would silently change what "any org
  agent can act like any other" already means for human collaborators on the same issue, which this
  product never enforced either. If a future need for real mutual exclusion appears, the correct
  primitive is claim/release's atomic 409 (already race-safe), not retrofitting an ownership check
  onto every other mutation.
- **Future mistake prevented — the retention/reaper pattern for anything that can go stale
  unattended.** A03 (stuck claims) and A04 (retention deleting claimed manual issues) were the same
  underlying gap in two places: state that an unattended loop can abandon mid-flight with no
  built-in expiry. Both were fixed the same way — a scheduled sweep with an explicit, configurable
  grace window (`CLAIM_STALE_HOURS`, `MANUAL_ISSUE_RETENTION_DAYS`) that only acts on state with no
  recent activity, and that writes its own audit trail (`claim_released` with
  `reason:"stale"`) so the agent that comes back can tell what happened rather than just finding the
  issue mysteriously unclaimed. Any future "an agent holds X and might vanish" state should default
  to this pattern, not to "add a manual admin override and call it done" (which is what A03 had
  before N7c).
- **Evidence**: `docs/audits/AGENT_AUTOMATION_AUDIT_2026-08-14.md` (15 confirmed / 7 refuted, now with
  a remediation-status table mapping every finding to its N7 outcome), `feat/agent-remediation`
  commits `ebe6be8`..`faf6f34` (phases N7a–f), `docs/memory/VERIFIED_STATE.md` → "Agent-automation
  remediation (N7)". `rtk go build ./... && rtk go vet ./...` clean and `rtk go test
  ./tests/unit/...` 308 passed re-run at close-out 2026-08-14; per-phase gates (guard-deletion
  red-proofs, migration replays, dedupe boundary tests) recorded in each phase's own commit message.
- **Where to look**: `docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md`,
  `docs/audits/AGENT_AUTOMATION_AUDIT_2026-08-14.md`, `docs/memory/VERIFIED_STATE.md` → "Agent-automation
  remediation (N7)".

## 2026-08-18 — N10 part 2: encrypted server-side git-credentials store (`feat/n10-repo-credentials`)

- **What**: `repo_credentials` table (AES-256-GCM under `SENTINEL_ENCRYPTION_KEY`, per-row nonce,
  `key_version`, org-id AAD; migration `1723900000`, replay-proven), write-only management routes +
  Settings → Agents UI (add/replace/revoke, `manage_agents` RBAC, audited), admin-set
  `agents.can_access_repo_credentials` flag with a per-agent toggle, and the flag-gated delivery
  endpoint `GET /api/agent/repo-credentials` (403 without the flag, 503 without the master key,
  every served credential audited). OpenAPI spec + agent guide + DEPLOYMENT/helm/compose/.env
  wiring updated. Design recorded as **D22**.
- **Security proofs**: no-plaintext-at-rest asserted against raw rows (mock-captured inserts AND a
  real-Postgres integration test), decrypt round-trip, revoked-not-served with ciphertext
  destruction, and three guard mutations run red-first (flag check, audit write, encryption).
- **Systemic lesson**: `--sequence.shuffle` on the dashboard suite fails on a CLEAN tree — B13's
  "order-independence decays silently" is no longer hypothetical. Filed as its own task; plain
  `pnpm test` remains green.
- **Where to look**: `apps/dashboard-web/src/lib/server/repo-credential-crypto.ts`,
  `src/lib/db/queries/repo-credentials.ts`, `src/routes/api/agent/repo-credentials/`,
  `docs/memory/DECISIONS.md` D22, `docs/memory/VERIFIED_STATE.md` → "N10 part 2".

## 2026-08-18 — N8a: sentinel-worker skeleton (branch feat/agent-worker)

Built `tools/sentinel-worker` (4th independent Go module, stdlib-only, GOWORK=off) per
docs/plans/AGENT_WORKER_PLAN.md rev 5 §9 N8a: config+gates (WORKER_ENABLED/WORKER_EXECUTE;
EXECUTE=true rejected until N8d ships the Actor), sentinel client + two-level retry/circuits +
IdempotencyKey derivation, atomic cursor + journal (10 states incl. `advised`, fsync'd
compaction, O(1) jobId index), poll loop (drain-before-sleep, ctx-aware waits) + C9 bootstrap
(no history replay; delta bounded by headCapturedAt), dispatcher (§3 table + issue_deleted,
echo suppression, coalescing w/ superseded, per-issue serial queues, shutdown drain) + runner
(preconditions, ensure-claimed C1, dry-run, panic recovery, transient⇒terminal-failed), health
server (/healthz /readyz /metrics), CI job (build/vet/gofmt/test -race). Orchestration: Sonnet
implementors + Opus adversarial validators, 20+ validation rounds total; two transient-infra
aborts recovered out-of-band. Notable catches: readyz wiring unasserted (B3), Brain→Advisor
rename before "brained" became a persisted string, same-seq redelivery self-supersede event
loss, flaky panic-victim test, dashboard assignIssue claim_released lacking previousAssignee
(B5 — fixed server-side + tested red-first). Proofs: module gate green incl. -race; dashboard
build/check/test(shuffled) green; live boot vs compose stack (healthz 200, readyz 503 honest,
recovery-before-identity, 401 backoff, SIGTERM exit 0). Unwired-by-design seams documented in
plan §9 note (guard→N8c, keyguard→N8e, batch writers→N8d, runner circuits→N8e).

## 2026-08-18 — N8b: llm package (branch feat/agent-worker)

`tools/sentinel-worker/llm`: provider-neutral Chat interface + factory, tool loop (turn/token/
wall-clock caps, defensive MaxTurns default, UTF-8-safe truncation, validate-and-re-ask ≤2 then
Permanent, StopError short-circuit before re-asks, SyncBreaker + fallback-provider handoff,
per-turn Provider label), DailyBudget/volume counters (UTC resets, race-tested), and three
adapters — openai (primary: base-URL normalization, json_schema + schema-ignored fallback),
anthropic (forced-tool-call structured decisions, pinned anthropic-version), gemini
(thoughtsTokenCount summed into OutputTokens — thinking tokens bill as output but are excluded
from candidatesTokenCount; missing it under-enforced budgets). Cross-adapter parity table
(llm/parity_test.go: 11 scenarios × 3 adapters over real wire fixtures) unified: empty-content
⇒ malformed-retry-then-Permanent everywhere, 3xx ⇒ Transient, ctx-cancel ⇒ wrapped
context.Canceled, JSONSchemaName normalized neutrally (raw names 400 on two providers).
Validator catches of note: anthropic/gemini "goldens" originally round-tripped through their own
json tags (two request-breaking mutations stayed green — rewritten to raw-JSON goldens);
malformedJSONError hardcoded "llm/openai:" for all three providers. llm remains a documented
unwired seam until N8d. Gate green incl. -race + gofmt. Orchestration: Sonnet impl + Opus
validators (loop now always ENDS on a validation), integration parity validator, Fable holistic.
