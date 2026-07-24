# TODO 05: Issue Lifecycle Management & Regression Tracking

## Priority: Important
## Status: Pending

### Overview
`apps/dashboard-web` lists ingested issues, but lacks a full issue triage lifecycle workflow and automated regression detection.

### Requirements
1. **Issue Status Workflow**:
   - Statuses: `Unresolved`, `Resolved`, `Ignored`.
   - UI actions for bulk resolution and assignment.

2. **Automated Regression Detection**:
   - Track `release_version` or `environment` in error event payloads.
   - If an event arrives matching a `Resolved` issue from a newer `release_version`, automatically transition status back to `Unresolved` and flag it as a Regression.

3. **Advanced Filtering**:
   - Filter issues by environment (`production`, `staging`, `development`), platform (`go`, `js`, `python`), release version, or date range.

### Acceptance Criteria
- Resolved issues automatically reopen upon new occurrences in subsequent releases.
- Users can filter and triage issues seamlessly in `apps/dashboard-web`.
