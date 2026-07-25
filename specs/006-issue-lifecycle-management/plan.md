# Implementation Plan: Issue Lifecycle Management & Regression Tracking

**Feature Branch**: `feature/issue-lifecycle-management`  
**Created**: 2026-07-25  
**Status**: Revised Plan  
**Specification**: [specs/006-issue-lifecycle-management/spec.md](file:///home/fitrapujo/oss/sentinel/specs/006-issue-lifecycle-management/spec.md)  
**Memory Context**: [specs/006-issue-lifecycle-management/memory-synthesis.md](file:///home/fitrapujo/oss/sentinel/specs/006-issue-lifecycle-management/memory-synthesis.md)

---

## Executive Summary

The **Issue Lifecycle Management & Regression Tracking** feature upgrades Sentinel's error monitoring capabilities from passive ingestion listing to an active, automated triage platform. It introduces:
1. State management (`unresolved`, `resolved`, `ignored`) with regression badges and bulk action support.
2. Polymorphic issue assignees supporting both human developers and AI Agents (`assignee_type: "user" | "agent"`).
3. Automated version-aware regression detection inside **`apps/processor-go`** on the event ingestion pipeline, reopening resolved issues in <1 second upon recurrence in newer software releases.
4. An asynchronous Many-to-Many issue linkage foundation (`issue_relations`) with zero (0%) read/lock overhead on the high-throughput ingestion hot path.
5. Multi-dimensional filtering across environments, platforms, release versions, assignees, and issue types.

---

## Technical Architecture & Schema Design

### 1. Database Schema Updates (`packages/db-migrations` & Drizzle ORM)

#### Goose Migration: `1721900000_add_issue_lifecycle_and_relations.sql`

- **Update `issues` Table**:
  - `status`: VARCHAR(20) DEFAULT `'unresolved'` CHECK (`status` IN ('unresolved', 'resolved', 'ignored')).
  - `regression_status`: VARCHAR(20) DEFAULT `'none'` CHECK (`regression_status` IN ('none', 'regressed')).
  - `issue_type`: VARCHAR(50) DEFAULT `'system_error'` CHECK (`issue_type` IN ('system_error', 'user_report')).
  - `source_channel`: VARCHAR(50) DEFAULT `'ingestion_sdk'` CHECK (`source_channel` IN ('ingestion_sdk', 'manual_support', 'api')).
  - `assignee_type`: VARCHAR(20) DEFAULT NULL CHECK (`assignee_type` IN ('user', 'agent')).
  - `assigned_to`: VARCHAR(255) DEFAULT NULL.
  - `resolved_in_version`: VARCHAR(100) DEFAULT NULL.
  - `resolved_at`: TIMESTAMP DEFAULT NULL.
  - `resolved_by_type`: VARCHAR(20) DEFAULT NULL CHECK (`resolved_by_type` IN ('user', 'agent')).
  - `resolved_by`: VARCHAR(255) DEFAULT NULL.
  - `regression_count`: INTEGER DEFAULT 0.
  - `last_regressed_at`: TIMESTAMP DEFAULT NULL.
  - Indexes: Add composite index `idx_issues_project_status_regression` on `(project_id, status, regression_status)` and index on `(project_id, assignee_type, assigned_to)`.

- **Update `error_occurrences` Table**:
  - Add `release_version`: VARCHAR(100) DEFAULT NULL.
  - Add index `idx_occurrences_issue_release` on `(issue_id, release_version)`.

- **New Table: `issue_relations`**:
  - `id`: UUID PK DEFAULT gen_random_uuid().
  - `source_issue_id`: UUID NOT NULL REFERENCES `issues(id)` ON DELETE CASCADE.
  - `target_issue_id`: UUID NOT NULL REFERENCES `issues(id)` ON DELETE CASCADE.
  - `relation_type`: VARCHAR(50) NOT NULL CHECK (`relation_type` IN ('linked_to', 'caused_by', 'duplicate_of')).
  - `created_by_type`: VARCHAR(20) NOT NULL CHECK (`created_by_type` IN ('user', 'agent', 'system')).
  - `created_by`: VARCHAR(255) NOT NULL.
  - `created_at`: TIMESTAMP DEFAULT NOW().
  - Unique Constraint: `(source_issue_id, target_issue_id, relation_type)`.
  - Indexes: B-tree index on `source_issue_id` and `target_issue_id`.

- **New Table: `issue_activity`**:
  - `id`: UUID PK DEFAULT gen_random_uuid().
  - `issue_id`: UUID NOT NULL REFERENCES `issues(id)` ON DELETE CASCADE.
  - `actor_type`: VARCHAR(20) NOT NULL CHECK (`actor_type` IN ('user', 'agent', 'system')).
  - `actor_id`: VARCHAR(255) NOT NULL.
  - `event_type`: VARCHAR(50) NOT NULL CHECK (`event_type` IN ('status_changed', 'assigned', 'unassigned', 'regressed', 'ai_analysis', `linked`)).
  - `old_value`: JSONB DEFAULT NULL.
  - `new_value`: JSONB DEFAULT NULL.
  - `created_at`: TIMESTAMP DEFAULT NOW().
  - Index: `idx_issue_activity_issue_id` on `(issue_id, created_at DESC)`.

---

## Service & Middleware Integration

### 1. Ingestion Pipeline & Automated Regression Detection (`apps/processor-go`)
- Update Go structs in `apps/processor-go/store/store.go` to include `ReleaseVersion` on `ErrorOccurrence`.
- Update `UpsertIssue` in `apps/processor-go/store/store.go`:
  1. On `ON CONFLICT (project_id, fingerprint) DO UPDATE`:
     - Inspect `issues.status` and `issues.resolved_in_version`.
     - If `status == 'resolved'` and incoming `release_version` >= `resolved_in_version`:
       - Update: `status = 'unresolved'`, `regression_status = 'regressed'`, `regression_count = regression_count + 1`, `last_regressed_at = NOW()`, `resolved_in_version = NULL`, `resolved_at = NULL`.
  2. Insert corresponding `regressed` event into `issue_activity` inside the same Go database transaction.
  3. **Zero Lock Invariant**: Confirm zero queries or locks against `issue_relations` during ingestion.

### 2. Dashboard Web API & SvelteKit Backend (`apps/dashboard-web`)

- `POST /api/projects/[projectId]/issues/batch`: Bulk status updates (`resolve`, `ignore`, `unresolve`) and bulk assignment updates (`user` or `agent`) calling atomic helper `batchUpdateIssues` in `queries/issues.ts`.
- `GET /api/projects/[projectId]/issues`: Multi-dimensional issue listing supporting filters: `status`, `regression_status`, `environment`, `platform`, `release_version`, `assignee_type`, `assigned_to`, `issue_type`.
- `GET /api/issues/[issueId]/activity`: Fetch issue activity timeline.
- `POST /api/issues/[issueId]/relations`: Create/delete Many-to-Many issue relations (`source_issue_id`, `target_issue_id`, `relation_type`).

---

## Verification & Testing Strategy

1. **Migration Verification**: Run `pressly/goose` migration `1721900000_add_issue_lifecycle_and_relations.sql` against Postgres test database.
2. **Go Ingestion Regression Detection Test**: Test event processing in `apps/processor-go`: ingest event for resolved issue with newer release version and verify immediate status transition to `unresolved` & `regressed` in `issues` and `issue_activity`.
3. **Polymorphic Assignee Test**: Test assigning issues to `user:123` vs `agent:auto-debugger`, checking `IssueActivity` logs.
4. **Performance Benchmark**: Execute 10,000 synthetic event ingestion requests through `apps/processor-go` while `issue_relations` table contains 50,000 entries. Assert zero latency penalty on event ingestion hot path.
