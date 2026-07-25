# Tasks: Issue Lifecycle Management & Regression Tracking

**Feature Branch**: `feature/issue-lifecycle-management`  
**Status**: Completed  
**Specification**: [specs/006-issue-lifecycle-management/spec.md](file:///home/fitrapujo/oss/sentinel/specs/006-issue-lifecycle-management/spec.md)  
**Implementation Plan**: [specs/006-issue-lifecycle-management/plan.md](file:///home/fitrapujo/oss/sentinel/specs/006-issue-lifecycle-management/plan.md)  
**Memory Context**: [specs/006-issue-lifecycle-management/memory-synthesis.md](file:///home/fitrapujo/oss/sentinel/specs/006-issue-lifecycle-management/memory-synthesis.md)  
**Security Constraints**: [specs/006-issue-lifecycle-management/security-constraints.md](file:///home/fitrapujo/oss/sentinel/specs/006-issue-lifecycle-management/security-constraints.md)

---

## Phase 1: Database Migration & Schema Foundations (Setup & Data Layer)

- [x] **T-001: Goose Database Migration SQL for Issue Lifecycle & Relations**
  - **File**: `packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql`
  - **Description**: Create Goose SQL migration adding `status`, `regression_status`, `issue_type`, `source_channel`, `assignee_type`, `assigned_to`, `resolved_in_version`, `resolved_at`, `resolved_by_type`, `resolved_by`, `regression_count`, `last_regressed_at` to `issues`. Add `release_version` to `error_occurrences`. Create `issue_relations` and `issue_activity` tables with indexes.
  - **Verification**: SQL migration created with Goose syntax and default constraints.

- [x] **T-002: Drizzle ORM Schema & Relationship Definitions**
  - **File**: `apps/dashboard-web/src/lib/db/schema.ts`
  - **Description**: Export updated `issues` and `errorOccurrences` table definitions in Drizzle. Export new `issueRelations` and `issueActivity` schema tables with FK relations and indexes.
  - **Verification**: Drizzle schema exports match Goose migration structure.

- [x] **T-003: Core Database Queries & Activity Timeline Helpers**
  - **File**: `apps/dashboard-web/src/lib/db/queries/issues.ts`
  - **Description**: Implement query helper functions for issue state updates (`updateIssueStatus`), batch updates (`batchUpdateIssues`), polymorphic assignment (`assignIssue`), activity logging (`logIssueActivity`), multi-dimensional issue filtering, and relation creation (`createIssueRelation`).
  - **Verification**: Exported functions provide type-safe operations for issue management.

---

## Phase 2: Ingestion & Automated Regression Detection (Core Logic)

- [x] **T-004: Go Ingestion Automated Version-Aware Regression Detector**
  - **File**: `apps/processor-go/store/store.go`, `apps/processor-go/service/processor_service.go`, `apps/processor-go/event/event.go`
  - **Description**: Update `ErrorOccurrence` struct and SQL query in `apps/processor-go/store/store.go` to capture `release_version`. In `UpsertIssue`, evaluate incoming `release_version` against `issues.resolved_in_version`. If `status == 'resolved'` and `release_version` >= `resolved_in_version`, automatically update status to `unresolved`, set `regression_status = 'regressed'`, increment `regression_count`, and insert a `regressed` record in `issue_activity`. Ensure 0% read/lock overhead on `issue_relations`.
  - **Verification**: Go processor evaluates release version during event ingestion, reopening regressed issues in real-time.

- [x] **T-005: Multi-Tenant Scoped Issue & Relation API Endpoints**
  - **File**: `apps/dashboard-web/src/routes/api/projects/[projectId]/issues/+server.ts`, `apps/dashboard-web/src/routes/api/projects/[projectId]/issues/batch/+server.ts`, `apps/dashboard-web/src/routes/api/issues/[issueId]/relations/+server.ts`
  - **Description**: Implement REST API endpoints for issue listing with multi-dimensional filters, bulk status/assignment updates, and Many-to-Many issue relation creation. Enforce Organization RBAC and validate agent credentials when `assignee_type = 'agent'`.
  - **Verification**: Endpoints enforce tenant isolation and RBAC role checks.

---

## Phase 3: Dashboard Web UI & Triage Components (User Interface)

- [x] **T-006: Issue Status & Polymorphic Assignee Badge Components**
  - **File**: `apps/dashboard-web/src/lib/components/issues/IssueStatusBadge.svelte`, `apps/dashboard-web/src/lib/components/issues/IssueAssigneePicker.svelte`
  - **Description**: Build Svelte components rendering issue status badges (`Unresolved`, `Resolved`, `Ignored`, `Regressed`) and a polymorphic assignee picker supporting human organization members and AI Agents (`user` vs `agent`).
  - **Verification**: Components render status badges and assignee picker.

- [x] **T-007: Bulk Issue Triage Bar & Multi-Dimensional Filter Bar**
  - **File**: `apps/dashboard-web/src/lib/components/issues/IssueFilterBar.svelte`, `apps/dashboard-web/src/lib/components/issues/BulkTriageBar.svelte`
  - **Description**: Build interactive filter bar (status, regression, environment, platform, release version, assignee type, issue type) and bulk action bar allowing multi-select resolution and assignment.
  - **Verification**: Bulk action bar and filters trigger multi-dimensional search updates.

- [x] **T-008: Issue Details View with Activity Timeline & Related Issues**
  - **File**: `apps/dashboard-web/src/routes/[orgSlug]/projects/[projectId]/issues/[issueId]/+page.svelte`
  - **Description**: Build issue detail view rendering error stacktrace, regression history, audit activity timeline (`IssueActivity`), and linked related issues (`IssueRelation`).
  - **Verification**: Page displays issue details, activity timeline, and related issues.

---

## Phase 4: Integration Verification & Regression Tests (QA)

- [x] **T-009: Automated Regression & Multi-Tenant Isolation Tests**
  - **File**: `apps/dashboard-web/tests/issue-lifecycle-regression.test.ts`
  - **Description**: Write Vitest integration tests covering:
    1. Automated reopening of resolved issues upon newer release occurrence ingestion.
    2. Zero reopening on older/legacy release occurrences.
    3. Bulk triage state updates and polymorphic assignee changes (`user` vs `agent`).
    4. Tenant isolation on `IssueRelation` links.
  - **Verification**: Automated test suite passes cleanly.
