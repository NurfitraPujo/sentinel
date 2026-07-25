# Feature Specification: Issue Lifecycle Management & Regression Tracking

**Feature Branch**: `feature/issue-lifecycle-management`  
**Created**: 2026-07-25  
**Status**: Draft  
**Input**: User description: "we need to have issue lifecycle management, read this document docs/todos/05-issue-lifecycle-management-and-regression-tracking.md and explore more on this topics ensuring we provide a robust and convenient issue trackings and lifecycles managements"

## Clarifications

### Session 2026-07-25
- Q: How should issue assignment and triaging support AI Agents? → A: Polymorphic Assignees & Agent Actors. Support assigning issues to both human organization members and AI agents (`assignee_type: "user" | "agent"`, `assignee_id`). Log AI agent actions (triage, auto-investigation, automated resolution) directly in `IssueActivity` timeline.
- Q: How should the issue schema and architecture be designed to accommodate future user/support manual issue reporting and AI linking? → A: Extensible Source & Linkage Data Foundation. Introduce `issue_type` (`system_error` | `user_report`) and `source_channel` (`ingestion_sdk`, `manual_support`, `api`) on `issues`, plus an asynchronous `issue_relations` junction table (`source_issue_id`, `target_issue_id`, `relation_type`) supporting Many-to-Many linking with zero performance impact on high-throughput error ingestion.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Comprehensive Issue Triage & Polymorphic Assignment Management (Priority: P1)

As an engineer, support team member, or AI triage agent monitoring Sentinel error dashboards, I want to transition error issues between well-defined lifecycle states (`Unresolved`, `Resolved`, `Ignored`) individually or in bulk, and assign ownership to human developers or AI agents (`assignee_type: "user" | "agent"`), so that our team and automated agents can maintain a clean, actionable issue backlog without manual clutter.

**Why this priority**: Core workflow foundation. Without clear status transitions and flexible assignment, team members and AI bots cannot triage incoming errors effectively.

**Independent Test**: Select 5 unresolved issues on the dashboard, perform a bulk state update to "Resolved", verify all 5 issues transition immediately to "Resolved" status and update their state history. Assign an issue to an AI Agent (`agent:auto-debugger`) and verify that the assignee badge displays the agent identity clearly.

**Acceptance Scenarios**:

1. **Given** an unresolved error issue, **When** a user or AI Agent changes its status to `Resolved` (optionally specifying the current release version in which it was fixed), **Then** the issue status transitions to `Resolved`, setting a `resolved_at` timestamp and recording the resolving actor type (`user` or `agent`), actor identity, and version.
2. **Given** an issue generating non-critical noise, **When** a user or AI Agent changes its status to `Ignored`, **Then** the issue is filtered out of default unresolved dashboard views but remains searchable under the `Ignored` tab.
3. **Given** a list of error issues, **When** a user or AI Agent selects multiple items and chooses "Bulk Assign" (selecting a human team member or AI Agent) or "Bulk Resolve", **Then** all selected issues update their status or assigned owner (`assignee_type`, `assignee_id`) in a single atomic operation.
4. **Given** an issue requiring assignment to an automated bot, **When** a user assigns the issue to an AI Agent (e.g. `agent:auto-debugger`), **Then** the system updates `assignee_type = 'agent'` and logs an `assigned` event in `IssueActivity` specifying `actor_type` and agent metadata.

---

### User Story 2 - Automated Regression Detection & Reopening (Priority: P1)

As an engineer who has resolved an error issue in a specific software version, I want Sentinel to automatically detect when the same error recurs in a newer release version or environment, mark the issue as `Regressed`, and reopen it, so that our team and AI triage agents are immediately alerted to broken fixes without manual monitoring.

**Why this priority**: Critical reliability guardrail. Prevents silent regressions from slipping through resolved states unnoticed.

**Independent Test**: Mark an issue as `Resolved` in version `v1.0.0`. Ingest a new error event with a matching fingerprint carrying `release_version: "v1.1.0"`. Verify that the issue automatically transitions to `Unresolved`, gains a `Regressed` flag/badge, increments its regression counter, and triggers an alert.

**Acceptance Scenarios**:

1. **Given** an issue marked as `Resolved` in version `v1.0.0`, **When** a new error occurrence is ingested with `release_version: "v1.1.0"` (or any version newer than/equal to the resolution release), **Then** Sentinel automatically reopens the issue to `Unresolved`, marks its regression status as `Regressed`, and records the regression event timestamp.
2. **Given** an issue marked as `Resolved`, **When** a new occurrence arrives from a legacy/older version than the resolution release (e.g., `v0.9.0`), **Then** Sentinel logs the occurrence without reopening the issue or marking it as regressed.
3. **Given** an issue marked as `Ignored`, **When** new error occurrences arrive, **Then** Sentinel increments the occurrence count silently without reopening the issue or triggering regression alerts, unless explicitly un-ignored by a user or AI agent.

---

### User Story 3 - Multi-Dimensional Filtering & Issue Relations Foundation (Priority: P2)

As a developer or AI agent investigating system errors, I want to filter and search issues by environment (`production`, `staging`, `development`), platform (`go`, `js`, `python`), release version, assignee type (`user` vs `agent`), assignee identity, and issue type (`system_error` vs `user_report`), as well as establish Many-to-Many relationships (`IssueRelation`) between linked issues, so that root causes and correlated incidents across services can be explored effortlessly.

**Why this priority**: High-value exploration tool for quick diagnosis during incident management and multi-service root cause analysis.

**Independent Test**: Apply filters for `environment = production`, `platform = go`, and `assignee_type = agent`. Create an `IssueRelation` linking issue A to issue B (`relation_type = 'caused_by'`), and verify that querying relations for issue A returns issue B without impacting ingestion performance.

**Acceptance Scenarios**:

1. **Given** a project issue list, **When** a user applies filter combinations (e.g., Environment + Platform + Release Version + Assigned User/Agent + Issue Type), **Then** the issue table updates immediately to display only matching issues.
2. **Given** a specific release deployment, **When** an engineer or AI agent filters issues by `release_version: "v2.4.0"`, **Then** Sentinel lists all new and regressed issues introduced in that release version.
3. **Given** two related issues (e.g., an upstream service error and a downstream consumer failure), **When** a user or AI Agent creates a relationship link via `IssueRelation` (`relation_type = 'caused_by'`), **Then** the Many-to-Many link is persisted asynchronously without acquiring locks on the error occurrence ingestion path.

---

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The system MUST support explicit issue lifecycle statuses: `Unresolved`, `Resolved`, and `Ignored`.
- **FR-002**: The system MUST support polymorphic issue assignment to either human organization members or AI agents (`assignee_type: "user" | "agent"`, `assignee_id`) and track assignment history.
- **FR-003**: The system MUST support bulk operations for updating issue statuses (`Resolve`, `Ignore`, `Unresolve`) and assigning owners (human users or AI agents) across multiple selected issues simultaneously.
- **FR-004**: The system MUST capture `release_version` and `environment` metadata in error occurrence payloads.
- **FR-005**: The system MUST automatically compare the incoming error's `release_version` against a `Resolved` issue's resolution release version.
- **FR-006**: If an occurrence arrives for a `Resolved` issue with a `release_version` >= resolution release (or if release versioning is absent, any new occurrence in production), the system MUST automatically transition the issue status to `Unresolved` and mark it with a prominent `Regressed` state flag.
- **FR-007**: The system MUST provide multi-dimensional filtering across issue status, regression state, environment, platform/SDK, release version, assignee type (user vs agent), assignee identity, issue type, and date range.
- **FR-008**: The system MUST log an audit/activity timeline (`IssueActivity`) for each issue, recording state transitions (Resolved, Ignored, Reopened, Regressed), assignee updates (human or AI agent), and resolution version milestones.
- **FR-009**: The database schema and domain entities MUST include extensible foundational fields (`issue_type`, `source_channel`) on `issues` and create a dedicated `issue_relations` junction table (`source_issue_id`, `target_issue_id`, `relation_type` [`linked_to`, `caused_by`, `duplicate_of`]) supporting Many-to-Many linking.
- **FR-010**: The ingestion hot path (`/api/v1/events`) MUST NOT perform reads or locks on `issue_relations`, ensuring 0% performance degradation during high-throughput error ingestion.

### Success Criteria

- **SC-001**: **Automated Regression Detection Latency**: 100% of newly ingested error occurrences matching a resolved issue trigger automatic reopening and regression flagging within 1 second of ingestion.
- **SC-002**: **Bulk Triage Efficiency**: Performing bulk status updates or assignments on up to 100 issues completes in under 500 milliseconds.
- **SC-003**: **Filter & Search Latency**: Multi-dimensional filtering across 50,000 issues returns updated dashboard results in under 300 milliseconds.
- **SC-004**: **Zero Ingestion Overhead**: Issue linking schema structures introduce zero (0%) read/lock overhead on the high-throughput error occurrence ingestion hot path.
- **SC-005**: **Zero False Reopens**: 0% of occurrences from older/legacy software releases (prior to the recorded resolution version) cause valid resolved issues to reopen prematurely.

## Key Entities *(mandatory)*

- **Issue**: Updated to track `status` (`unresolved`, `resolved`, `ignored`), `regression_status` (`none`, `regressed`), `issue_type` (`system_error`, `user_report` [foundation]), `source_channel` (`ingestion_sdk`, `manual_support`, `api`), `assignee_type` (`user`, `agent`), `assigned_to` (`user_id` or `agent_id`), `resolved_in_version`, `resolved_at`, `resolved_by_type`, `resolved_by`, `regression_count`, `last_regressed_at`.
- **IssueRelation**: Dedicated Many-to-Many junction table (`id`, `source_issue_id`, `target_issue_id`, `relation_type` [`linked_to`, `caused_by`, `duplicate_of`], `created_by_type`, `created_by`, `created_at`).
- **IssueActivity**: Activity log table capturing timeline events (`id`, `issue_id`, `actor_type` [`user`, `agent`, `system`], `actor_id`, `event_type` [e.g., `status_changed`, `assigned`, `regressed`, `ai_analysis`, `linked`], `old_value`, `new_value`, `created_at`).
- **ErrorOccurrence**: Error occurrence event payload containing `release_version`, `environment`, `platform`, `metadata`.

## Assumptions

- Software releases follow semantic versioning (`vX.Y.Z`) or sequential build numbers to enable reliable version comparison.
- When an issue is resolved without specifying a release version, any subsequent error occurrence in the same environment automatically triggers a regression reopen.
- Access to issue triage actions (resolve, ignore, assign) is governed by organization roles (`owner`, `admin`, `engineer`, `support`) or authorized AI agent API keys.
- Manual issue reporting UI and AI auto-linking pipelines are out of scope for this initial release, but the `issue_relations` Many-to-Many data foundation is explicitly established.
