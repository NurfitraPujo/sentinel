# Ripple Fixes — Session 2026-07-25

## R-001: Bulk Action Status Discrepancy ('open' vs 'unresolved') [WARNING]

**Strategy**: Option A — Update `batch/+server.ts` line 57 to set `updateData.status = 'unresolved'`

**Files to modify**:
- `apps/dashboard-web/src/routes/api/projects/[projectId]/issues/batch/+server.ts` — Change line 57 `updateData.status = 'open'` to `updateData.status = 'unresolved'`

**Key steps**:
1. Open `apps/dashboard-web/src/routes/api/projects/[projectId]/issues/batch/+server.ts`
2. Change `case 'unresolve': updateData.status = 'open'` to `updateData.status = 'unresolved'`
3. Verify that bulk unresolve action succeeds without Postgres check constraint error.

**Verification**: Send bulk unresolve API request and assert status is `'unresolved'`.

---

## R-002: Missing Activity Audit Events in Bulk Operations [WARNING]

**Strategy**: Option B — Create a dedicated `batchUpdateIssues` helper function in `queries/issues.ts` and call it from `batch/+server.ts`

**Files to modify**:
- `apps/dashboard-web/src/lib/db/queries/issues.ts` — Implement `batchUpdateIssues(projectId, action, issueIds, resolvedInVersion, assigneeType, assignedTo, actorType, actorId)`
- `apps/dashboard-web/src/routes/api/projects/[projectId]/issues/batch/+server.ts` — Delegate bulk processing to `batchUpdateIssues` query helper

**Key steps**:
1. Add `batchUpdateIssues` to `queries/issues.ts` wrapping `issues` update and `issueActivity` batch insertion in a `db.transaction()`.
2. Refactor `batch/+server.ts` to call `batchUpdateIssues`.
3. Assert both `issues` status/assignee updates and `issueActivity` logs are produced during bulk operations.

**Verification**: Test bulk action API endpoint and verify `issue_activity` contains corresponding events.

---

## R-003: Simple String Fallback Comparison in Semver Regression Detector [INFO]

**Strategy**: Option B — Adopt a dedicated `semver` library parser in `queries/issues.ts`

**Files to modify**:
- `apps/dashboard-web/src/lib/db/queries/issues.ts` — Update `isRegression` helper to use a standard semver comparison parser (`semver.gte` or robust semver comparison fallback)

**Key steps**:
1. Import/utilize semver parsing in `isRegression` in `apps/dashboard-web/src/lib/db/queries/issues.ts`.
2. Compare `releaseVersion` and `resolvedInVersion` using `semver.gte` (handling `v` prefix strip and pre-release tags cleanly).
3. Fallback gracefully to integer/localeCompare if non-semver strings are provided.

**Verification**: Unit test `isRegression` with complex semver strings like `v1.2.0-rc.1` vs `v1.2.0`.

---
