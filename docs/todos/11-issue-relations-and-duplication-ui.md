# TODO 11: Issue Relations and Duplication UI Component

## Priority: Medium
## Status: Completed

### Overview
The backend API `/api/issues/[issueId]/relations` (`GET`, `POST`, `DELETE`) fully supports linking issues as `parent_of`, `child_of`, or `duplicate_of`. However, no component or view in the frontend dashboard imports or renders issue relations.

### Requirements
1. **Issue Relations UI Component**:
   - Build a reusable `IssueRelations.svelte` component to display related, parent/child, and duplicate issues on the issue detail page.
2. **Interactive Linking Controls**:
   - Allow users to search for another issue by ID/fingerprint/message and link it with a chosen relation type (`parent_of`, `child_of`, `duplicate_of`).
   - Provide an unlink button to delete an existing issue relationship via `DELETE /api/issues/[issueId]/relations`.

### Affected Files
- `apps/dashboard-web/src/lib/components/issues/IssueRelations.svelte` (New)
- `apps/dashboard-web/src/routes/issues/[id]/+page.svelte`
- `apps/dashboard-web/src/routes/[orgSlug]/projects/[projectId]/issues/[issueId]/+page.svelte`

### Acceptance Criteria
- Users can view all linked parent, child, and duplicate issues on the issue detail page.
- Users can search and link a related issue via the UI component.
- Unlinking an issue updates the database and removes the relation card.
