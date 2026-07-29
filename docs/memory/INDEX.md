# Memory Index

This is a compact routing map for durable project memory (`docs/memory/`). Keep it short. 

> [!NOTE]
> High-level project governance, constitution, and standards are stored in the **Governance Layer** at `.specify/memory/` and should be reviewed before technical planning.

## State (read first)
- S0 | Verified State of the Codebase — what actually runs, verified by execution; re-verify anything dated before your current HEAD | audit,build,tests,reality-check | [VERIFIED_STATE.md](VERIFIED_STATE.md) | active
- P0 | E2E Recovery Plan — phased plan to make every feature work end-to-end, keyed to S0's findings | plan,recovery,ci,e2e | [../plans/E2E_RECOVERY_PLAN.md](../plans/E2E_RECOVERY_PLAN.md) | active

> [!WARNING]
> `specs/*/spec.md` "Completed" and `WORKLOG.md` milestones record *merge events*, not verified behavior.
> Several features marked Completed are unreachable at runtime. Check [VERIFIED_STATE.md](VERIFIED_STATE.md)
> before building on any of them.

## Architecture
- A1 | Unified Migration Directory Boundary | migrations,architecture,monorepo,postgres | [ARCHITECTURE.md](ARCHITECTURE.md) | active
- A2 | Three-Module Go Layout, Workspace Mode for Local/Contract Tests Only (GOWORK=off in CI's go-root) | go,modules,sdk,testing,gowork | [ARCHITECTURE.md](ARCHITECTURE.md) | active

## Bugs
- B1 | Data Loss on Database Outage — its stated mitigation (GracefulDegradation) is itself broken | db,reliability,processor | [BUGS.md](BUGS.md) | active
- B2 | Reserved Path Collision Guard for Dynamic Slug Routing | routing,sveltekit,slug,guard | [BUGS.md](BUGS.md) | active
- B3 | "Shipped" Features That Are Never Invoked From main() | wiring,dead-code,process | [BUGS.md](BUGS.md) | active
- B4 | One Broken Test File Silently Disables an Entire Go Test Package | go,testing,coverage | [BUGS.md](BUGS.md) | active
- B5 | Cross-Boundary Payload Contracts Drift With No Test Spanning the Boundary | contracts,sdk,nats,json | [BUGS.md](BUGS.md) | partial (SDK↔ingestor covered by tests/contract; dashboard↔NATS seam still open)
- B6 | Normalization Destroys the Fields Read After It | processor,normalizer,regression | [BUGS.md](BUGS.md) | active
- B7 | Authenticated Identity Computed, Then Discarded | security,multitenancy,ingestor | [BUGS.md](BUGS.md) | resolved-2026-07-29

## Decisions
- D1 | Graceful Degradation via In-Memory Buffering | resilience,buffer,processor | [DECISIONS.md](DECISIONS.md) | needs-review — buffer-full path still silently acks a dropped event (S9); see D10
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

## Workflow
- W1 | Adopted CEL for Protobuf Validation | protobuf,validation,buf | [WORKLOG.md](WORKLOG.md) | active
- W2 | Shared DB Migrations Foundation Shipped | milestone,migrations,architecture | [WORKLOG.md](WORKLOG.md) | active
- W3 | Shipped Organization Layer & Multi-Tenancy Support | milestone,organizations,multitenancy | [WORKLOG.md](WORKLOG.md) | active
- W4 | Shipped Issue Lifecycle Management & Regression Tracking | milestone,lifecycle,regression,triage | [WORKLOG.md](WORKLOG.md) | active
- W5 | Shipped Official Go Client SDK & Ingestor Batch API | milestone,sdk,go,ingestion,batch | [WORKLOG.md](WORKLOG.md) | active
- W6 | Shipped Multi-Tenant Auth & API Key Management | milestone,apikeys,auth,ratelimit,multitenancy | [WORKLOG.md](WORKLOG.md) | active
