# Memory Synthesis: Issue Lifecycle Management & Regression Tracking

**Feature**: `specs/006-issue-lifecycle-management`  
**Synthesized At**: 2026-07-25  

## Active Architecture Constraints & Decisions

1. **D6: Organization-First Multi-Tenancy & Role Inheritance**
   - All issues belong to a Project, which belongs to an Organization. Access controls for issue triage (`Resolve`, `Ignore`, `Assign`) MUST inherit the user's `organization_members.role` (`owner`, `admin`, `engineer`, `support`) unless overridden by `project_members`.
   - API endpoints and queries MUST enforce organization/project isolation.

2. **D3 & D4: Adopt Goose for Database Migrations & Loud-Failure Policy**
   - New database tables (`issue_relations`, `issue_activity`) and column additions MUST be implemented via static Goose SQL migrations under `packages/db-migrations/migrations/` using timestamp prefix `1721900000_*.sql`.

3. **SC-004 / Ingestion Decoupling Architecture Invariant**
   - High-throughput error event ingestion (`/api/v1/events` or processor worker) MUST NOT perform synchronous reads or locks on `issue_relations`. Regression detection logic MUST query `issues` table using existing indexes on `(project_id, fingerprint)`.

## Conflicts & Risk Assessment
- **None Detected**: The proposed Issue Lifecycle & Regression Tracking design aligns with Sentinel multi-tenancy, Goose migration rules, and decoupled ingestion performance invariants.
