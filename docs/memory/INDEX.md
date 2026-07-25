# Memory Index

This is a compact routing map for durable project memory (`docs/memory/`). Keep it short. 

> [!NOTE]
> High-level project governance, constitution, and standards are stored in the **Governance Layer** at `.specify/memory/` and should be reviewed before technical planning.

## Architecture
- A1 | Unified Migration Directory Boundary | migrations,architecture,monorepo,postgres | [ARCHITECTURE.md](ARCHITECTURE.md) | active

## Bugs
- B1 | Data Loss on Database Outage | db,reliability,processor | [BUGS.md](BUGS.md) | active
- B2 | Reserved Path Collision Guard for Dynamic Slug Routing | routing,sveltekit,slug,guard | [BUGS.md](BUGS.md) | active

## Decisions
- D1 | Graceful Degradation via In-Memory Buffering | resilience,buffer,processor | [DECISIONS.md](DECISIONS.md) | active
- D2 | Magic Link Authentication via Auth.js Email Provider | auth,magic-link,rb | [DECISIONS.md](DECISIONS.md) | active
- D3 | Adopt Goose for All Database Migrations | migrations,tooling,goose,go | [DECISIONS.md](DECISIONS.md) | active
- D4 | Strict Loud-Failure Migration Policy | migrations,errors,concurrency,policy | [DECISIONS.md](DECISIONS.md) | active
- D5 | Production Safety Guardrails for Destructive Migration Tasks | migrations,security,ci,operations | [DECISIONS.md](DECISIONS.md) | active
- D6 | Organization-First Multi-Tenancy & Role Inheritance | multitenancy,organizations,rbac,routing | [DECISIONS.md](DECISIONS.md) | active
- D7 | Real-time Ingestion Regression Detection with Polymorphic Assignees & Async Relations | ingestion,regression,lifecycle,go,multitenancy | [DECISIONS.md](DECISIONS.md) | active

## Workflow
- W1 | Adopted CEL for Protobuf Validation | protobuf,validation,buf | [WORKLOG.md](WORKLOG.md) | active
- W2 | Shared DB Migrations Foundation Shipped | milestone,migrations,architecture | [WORKLOG.md](WORKLOG.md) | active
- W3 | Shipped Organization Layer & Multi-Tenancy Support | milestone,organizations,multitenancy | [WORKLOG.md](WORKLOG.md) | active
- W4 | Shipped Issue Lifecycle Management & Regression Tracking | milestone,lifecycle,regression,triage | [WORKLOG.md](WORKLOG.md) | active
