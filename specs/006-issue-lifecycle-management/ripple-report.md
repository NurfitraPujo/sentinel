# Ripple Report: Issue Lifecycle Management & Regression Tracking

**Branch**: `feature/issue-lifecycle-management` | **Scanned**: 2026-07-25T10:55:00Z
**Baseline**: `f0927d6` (branch point from main)
**Change Set**: 11 files changed | **Blast Radius**: 4 dependent modules checked
**Findings**: 0 critical, 2 warning, 1 info

## Summary

The ripple scan evaluated the blast radius of adding Issue Lifecycle Management, Automated Regression Detection, Polymorphic Assignees (`user` vs `agent`), and Many-to-Many Issue Relations (`issue_relations`). No critical production outages or data loss risks were detected. Two warning items were identified: (1) Bulk action updates in `batch/+server.ts` set status to `'open'` instead of canonical `'unresolved'`, and (2) Bulk action updates do not emit corresponding `issueActivity` timeline events.

## Findings

---

### WARNING

#### R-001: Bulk Action Status Discrepancy ('open' vs 'unresolved')

- **Category**: Data Flow / Interface Contract
- **Cause**: In `apps/dashboard-web/src/routes/api/projects/[projectId]/issues/batch/+server.ts` (line 57), `case 'unresolve'` sets `updateData.status = 'open'`.
- **Affected**: `apps/dashboard-web/src/routes/api/projects/[projectId]/issues/batch/+server.ts` (line 57)
- **Blast Radius**: `apps/dashboard-web/src/lib/db/queries/issues.ts`, `apps/dashboard-web/src/lib/components/issues/IssueStatusBadge.svelte`
- **Before**: Standard issue status column check constraint enforces `'unresolved'`, `'resolved'`, `'ignored'`.
- **After**: Bulk unresolve action updates status to `'open'`, which fails check constraint `issues_status_check` or breaks status filter UI expecting `'unresolved'`.
- **Why Tests Miss It**: Unit tests for status updates targeted query helpers (`updateIssueStatus`) rather than the API route handler JSON payload parsing.
- **Recommendation**: Update `batch/+server.ts` line 57 to set `updateData.status = 'unresolved'`.
- **Status**: RESOLUTION_PLANNED
- **Resolution Strategy**: Option A: Update batch/+server.ts line 57 to set updateData.status = 'unresolved' — chosen on 2026-07-25

---

#### R-002: Missing Activity Audit Events in Bulk Operations

- **Category**: Observability / Data Flow
- **Cause**: `batch/+server.ts` performs direct `db.update(issues)` across an array of `issueIds` without writing corresponding records to `issue_activity`.
- **Affected**: `apps/dashboard-web/src/routes/api/projects/[projectId]/issues/batch/+server.ts` (lines 71-79)
- **Blast Radius**: `apps/dashboard-web/src/routes/[orgSlug]/projects/[projectId]/issues/[issueId]/+page.svelte` (`getIssueActivity`)
- **Before**: Individual status updates (`updateIssueStatus`) and assignments (`assignIssue`) logged `issueActivity` timeline events.
- **After**: Bulk status updates and assignments bypass `issueActivity` insertion, creating silent gaps in the issue activity timeline.
- **Why Tests Miss It**: Single-issue state transition tests verified `issueActivity` insertions, but bulk API route tests checked return JSON counts only.
- **Recommendation**: Wrap bulk updates in a transaction that inserts `issueActivity` rows for all modified `issueIds`.
- **Status**: RESOLUTION_PLANNED
- **Resolution Strategy**: Option B: Create dedicated batchUpdateIssues query helper in queries/issues.ts with issueActivity audit transaction — chosen on 2026-07-25

---

### INFO

#### R-003: Simple String Fallback Comparison in Semver Regression Detector

- **Category**: Resource & Performance / Error Propagation
- **Cause**: `isRegression` in `apps/dashboard-web/src/lib/db/queries/issues.ts` (lines 9-22) uses integer parsing with string fallback `localeCompare`.
- **Affected**: `apps/dashboard-web/src/lib/db/queries/issues.ts` (lines 9-22)
- **Blast Radius**: Automated regression detection ingestion hot path
- **Before**: Issues had no version-aware regression detection.
- **After**: Complex non-semver build tags (e.g. `v1.2.0-rc.1` vs `v1.2.0`) fall back to `localeCompare`, which handles standard semver accurately but may treat prerelease suffixes strictly by ASCII order.
- **Why Tests Miss It**: Standard semver string formats (`v1.0.0` -> `v1.1.0`) pass cleanly.
- **Recommendation**: Adopt robust semver library parser.
- **Status**: RESOLUTION_PLANNED
- **Resolution Strategy**: Option B: Adopt semver library parser in queries/issues.ts — chosen on 2026-07-25

---

## Coverage Gap Matrix

| Category | Critical | Warning | Info | Not Applicable |
|----------|----------|---------|------|----------------|
| Data Flow | 0 | 1 | 0 | |
| State & Lifecycle | 0 | 0 | 0 | |
| Interface Contract | 0 | 1 | 0 | |
| Resource & Performance | 0 | 0 | 1 | |
| Concurrency | 0 | 0 | 0 | |
| Distributed Coordination | 0 | 0 | 0 | N/A — single monorepo service |
| Configuration & Environment | 0 | 0 | 0 | |
| Error Propagation | 0 | 0 | 0 | |
| Observability | 0 | 1 | 0 | |

---

## Resolution History

| Date | Scope | Resolved | Accepted Risk | Skipped | Still Open |
|------|-------|----------|---------------|---------|------------|
| 2026-07-25T11:01:00Z | all | 3 | 0 | 0 | 0 |

### Session detail (2026-07-25)
- **R-001**: Option A (Update `updateData.status = 'unresolved'` in `batch/+server.ts`)
- **R-002**: Option B (Create reusable `batchUpdateIssues` query helper with `issueActivity` logging transaction)
- **R-003**: Option B (Adopt `semver` library comparison logic in `queries/issues.ts`)

---

## Next Steps

- [x] Address WARNING `R-001` (Planned)
- [x] Address WARNING `R-002` (Planned)
- [x] Address INFO `R-003` (Planned)
- [ ] Implement resolution plans from `specs/006-issue-lifecycle-management/ripple-fixes.md`
