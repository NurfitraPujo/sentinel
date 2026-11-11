# Memory Index

This is a compact routing map for durable project memory (`docs/memory/`). Keep it short. 

> [!NOTE]
> High-level project governance, constitution, and standards are stored in the **Governance Layer** at `.specify/memory/` and should be reviewed before technical planning.

## State (read first)
- S0 | Verified State of the Codebase — what actually runs, verified by execution; re-verify anything dated before your current HEAD | audit,build,tests,reality-check | [VERIFIED_STATE.md](VERIFIED_STATE.md) | active
- P0 | E2E Recovery Plan — phased plan to make every feature work end-to-end, keyed to S0's findings | plan,recovery,ci,e2e | [../plans/E2E_RECOVERY_PLAN.md](../plans/E2E_RECOVERY_PLAN.md) | active — **P7 complete 2026-07-30: all 32 matrix rows green** (56 tests, 0 skips, 124.8s, CI-gated)

> [!WARNING]
> `specs/*/spec.md` "Completed" and `WORKLOG.md` milestones record *merge events*, not verified behavior.
> Several features marked Completed are unreachable at runtime. Check [VERIFIED_STATE.md](VERIFIED_STATE.md)
> before building on any of them.

## Deferred work (read before assuming something is missing by accident)
- P9 | Deferred work, with the reason and the acceptance bar for each | backlog,deferred,process | [../plans/E2E_RECOVERY_PLAN.md](../plans/E2E_RECOVERY_PLAN.md) | active — the 4 items deferred 2026-07-30 (org-wide alert UI, observability, S16/S18 idempotency, invitation acceptance) are ALL DONE as of 2026-08-01; P9-1 and P9-4 shipped earlier but did not run until the UI parity remediation. Read P9-5 for what was deliberately NOT fixed

## Architecture
- A1 | Unified Migration Directory Boundary | migrations,architecture,monorepo,postgres | [ARCHITECTURE.md](ARCHITECTURE.md) | active
- A2 | Three-Module Go Layout, Workspace Mode for Local/Contract Tests Only (GOWORK=off in CI's go-root) | go,modules,sdk,testing,gowork | [ARCHITECTURE.md](ARCHITECTURE.md) | active

## Bugs
- B1 | Data Loss on Database Outage — mitigation DELETED, replaced by D10 | db,reliability,processor | [BUGS.md](BUGS.md) | resolved-2026-07-29
- B2 | Reserved Path Collision Guard for Dynamic Slug Routing | routing,sveltekit,slug,guard | [BUGS.md](BUGS.md) | active
- B3 | "Shipped" Features That Are Never Invoked From main() | wiring,dead-code,process | [BUGS.md](BUGS.md) | active
- B4 | One Broken Test File Silently Disables an Entire Go Test Package | go,testing,coverage | [BUGS.md](BUGS.md) | active
- B5 | Cross-Boundary Payload Contracts Drift With No Test Spanning the Boundary | contracts,sdk,nats,json | [BUGS.md](BUGS.md) | active — recurred TWICE on 2026-07-30 (alert channel_config `{target}` vs `["to"]`; DLQ class `"unknown"` vs `"unclassified"` within an hour of the contract existing). Shared constants are the mitigation that works
- B6 | Normalization Destroys the Fields Read After It | processor,normalizer,regression | [BUGS.md](BUGS.md) | active
- B8 | A Framework Misconfiguration Breaks Every Route While All Gates Stay Green | auth,sveltekit,runtime,b3 | [BUGS.md](BUGS.md) | active — confirmed again 2026-07-30: /auth/signin looped forever, nobody could sign in
- B9 | A Deployment That Never Connects Two Correct Halves | deployment,config,nats,b3 | [BUGS.md](BUGS.md) | active — two instances fixed, cause structural
- B10 | Tests That Assert The Defect | testing,regression,process | [BUGS.md](BUGS.md) | active — four instances fixed 2026-07-30; **two more 2026-08-01**, incl. the vacuous-mock variant where deleting the guarded line left 29/29 green
- B11 | Instrumentation Whose Failure Mode Is Silence | observability,otel,tracing,guards | [BUGS.md](BUGS.md) | active — found and guarded 2026-07-31
- B12 | The Gate You Never Ran | build,ci,sveltekit,process | [BUGS.md](BUGS.md) | active — `pnpm check`+`test` green while `pnpm build` failed on SvelteKit's route-export allowlist; CI existed but had never run
- B13 | Tests That Pass For The Wrong Reason: Ambient State and Test Order | testing,flake,mocks,postgres | [BUGS.md](BUGS.md) | active — ambient DB rows, leaked `...Once` mock queues, module caches, missing testing-library cleanup
- B7 | Authenticated Identity Computed, Then Discarded | security,multitenancy,ingestor | [BUGS.md](BUGS.md) | resolved-2026-07-29

## Decisions
- D1 | Graceful Degradation via In-Memory Buffering | resilience,buffer,processor | [DECISIONS.md](DECISIONS.md) | superseded-by-D10 — the buffer was deleted, not repaired
- D2 | Magic Link Authentication via Auth.js Email Provider | auth,magic-link,rb | [DECISIONS.md](DECISIONS.md) | active
- D3 | Adopt Goose for All Database Migrations | migrations,tooling,goose,go | [DECISIONS.md](DECISIONS.md) | active — accepted risk: already-applied migrations edited in place, see entry
- D4 | Strict Loud-Failure Migration Policy | migrations,errors,concurrency,policy | [DECISIONS.md](DECISIONS.md) | active — tension: idempotency retrofit for multi-ledger hazard, see entry
- D5 | Production Safety Guardrails for Destructive Migration Tasks | migrations,security,ci,operations | [DECISIONS.md](DECISIONS.md) | active
- D6 | Organization-First Multi-Tenancy & Role Inheritance | multitenancy,organizations,rbac,routing | [DECISIONS.md](DECISIONS.md) | active
- D7 | Real-time Ingestion Regression Detection with Polymorphic Assignees & Async Relations | ingestion,regression,lifecycle,go,multitenancy | [DECISIONS.md](DECISIONS.md) | active — both firing bugs fixed, runtime-proven (U11); old_value still NULL, residual gap
- D8 | Non-Blocking Dual-Endpoint Client SDK Protocol with Auto-Initialization & Context-Aware Telemetry Correlation | sdk,go,concurrency,opentelemetry,batch | [DECISIONS.md](DECISIONS.md) | active
- D9 | Dual-Layer Multi-Tenant API Key Authentication with NATS Invalidation & Hierarchical Sliding-Window Rate Limiting | multitenancy,apikeys,auth,ratelimit,nats,go | [DECISIONS.md](DECISIONS.md) | active — "hierarchical" is false (per-key only, corrected); rate limiting was 100% bypassed until R1
- D10 | Bounded-Retry NATS Delivery with Dead-Letter Capture | nats,jetstream,reliability,delivery,dlq | [DECISIONS.md](DECISIONS.md) | active
- D11 | APIKey/ProjectKey Split — a Project Name Is Never a Credential | sdk,go,auth,multitenancy,contracts | [DECISIONS.md](DECISIONS.md) | active
- D12 | Two-Layer Alert Configs (organization-wide + project-scoped), UNION not override | alerts,multitenancy,rbac,postgres | [DECISIONS.md](DECISIONS.md) | active — the "org-wide is API-only" note is STALE: the UI shipped and, as of 2026-08-01, actually runs (the dispatcher kept only ONE config per key until finding D04 was fixed)
- D13 | Every JetStream Stream Is Bounded; Discard Policy Chosen Per Role | nats,jetstream,ops,outage | [DECISIONS.md](DECISIONS.md) | active
- D14 | Dead Letters Carry a Machine-Readable Class; Permanent Failures Never Auto-Replayed | nats,dlq,ops,contracts | [DECISIONS.md](DECISIONS.md) | active
- D15 | OTel Everywhere, the Trace ID Is the Correlation ID | observability,otel,tracing | [DECISIONS.md](DECISIONS.md) | active
- D16 | Exactly-Once Event Writes: `event_id` End to End, One Transaction | idempotency,postgres,nats,processor | [DECISIONS.md](DECISIONS.md) | active
- D17 | An Org Role Is an Org-Wide Grant; Project Membership Is the Alternative Path, Not an Extra Hurdle | rbac,multitenancy,issues,authz | [DECISIONS.md](DECISIONS.md) | active — e2e disproved the stricter AND model
- D18 | An Invitation's Authority Is Re-Validated at Redemption, Not Just at Issue | invitations,authz,transactions,security | [DECISIONS.md](DECISIONS.md) | active — early `return` inside `db.transaction` COMMITS; throw to roll back

## Workflow
- W1 | Adopted CEL for Protobuf Validation | protobuf,validation,buf | [WORKLOG.md](WORKLOG.md) | active
- W2 | Shared DB Migrations Foundation Shipped | milestone,migrations,architecture | [WORKLOG.md](WORKLOG.md) | active
- W3 | Shipped Organization Layer & Multi-Tenancy Support | milestone,organizations,multitenancy | [WORKLOG.md](WORKLOG.md) | active
- W4 | Shipped Issue Lifecycle Management & Regression Tracking | milestone,lifecycle,regression,triage | [WORKLOG.md](WORKLOG.md) | active
- W5 | Shipped Official Go Client SDK & Ingestor Batch API | milestone,sdk,go,ingestion,batch | [WORKLOG.md](WORKLOG.md) | active
- W6 | Shipped Multi-Tenant Auth & API Key Management | milestone,apikeys,auth,ratelimit,multitenancy | [WORKLOG.md](WORKLOG.md) | active
- W7 | A Feature Can Merge Green, Be Reviewed, and Still Never Run | process,testing,ci,dashboard,b3 | [WORKLOG.md](WORKLOG.md) | active — 2026-08-01; first green CI run on `main` (`b895df1`)
