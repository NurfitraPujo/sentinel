# Security Constraints: Issue Lifecycle Management & Regression Tracking

**Feature**: `specs/006-issue-lifecycle-management`  
**Reviewed At**: 2026-07-25  

## Trust Boundaries & Access Controls

1. **Multi-Tenant Scoped Issue Mutations**:
   - All REST API endpoints modifying issue statuses (`POST /api/projects/[projectId]/issues/batch`) or assigning issues MUST validate that the authenticated user/agent possesses an authorized Organization role (`owner`, `admin`, `engineer`, `support`) within the parent Organization of `projectId`.

2. **Polymorphic Agent Identity Validation**:
   - When `assignee_type = 'agent'` or `actor_type = 'agent'`, the agent identity (`agent_id`) MUST be validated against registered workspace AI Agent credentials or signed API tokens to prevent impersonation of system bots.

3. **Asynchronous Relation Manipulation**:
   - `IssueRelation` creation MUST enforce that both `source_issue_id` and `target_issue_id` belong to projects within the same Organization boundary, preventing cross-tenant graph linkage leaks.
