# TODO 12: Organization Member Management Actions & Invitation Modal

## Priority: Medium
## Status: Pending

### Overview
`[orgSlug]/settings/members/+page.svelte` loads and renders organization members from the database. However, the action handlers for changing roles (`handleRoleChange`) and revoking access (`handleRevokeAccess`) contain `// TODO` logging stubs, and the "Invite Member" button lacks a modal trigger.

### Requirements
1. **Role Update & Member Revocation API Handlers**:
   - Wire `handleRoleChange` to invoke role update mutation endpoint (`PATCH /api/organizations/[orgId]/members/[memberId]`).
   - Wire `handleRevokeAccess` to invoke deletion endpoint (`DELETE /api/organizations/[orgId]/members/[memberId]`).
2. **Invite Member Modal & Flow**:
   - Build `InviteMemberModal.svelte` to input invitee email and target role (`owner`, `admin`, `engineer`, `support`, `viewer`).
   - Submit form to `POST /api/organizations/[orgId]/invitations` and render the generated invitation link/token.

### Affected Files
- `apps/dashboard-web/src/routes/[orgSlug]/settings/members/+page.svelte`
- `apps/dashboard-web/src/lib/components/members/InviteMemberModal.svelte` (New)

### Acceptance Criteria
- Changing a member's role in the dropdown persists to `organization_members`.
- Revoking access removes the member from the list and deletes their membership record.
- Clicking "Invite Member" opens a modal that creates an invitation token via `POST /api/organizations/[orgId]/invitations`.
