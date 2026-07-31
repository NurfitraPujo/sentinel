# TODO 10: Invitation Acceptance Route & Flow (P9-4)

## Priority: High
## Status: Completed

### Overview
`POST /api/organizations/[orgId]/invitations` creates valid `organization_invitations` database records with unique tokens. However, nothing in the dashboard consumes or redeems invitation tokens. Any invited user receiving an invitation link hits a 404 error.

### Requirements
1. **Accept Invitation Endpoint & Route**:
   - Create route `/auth/accept-invite` or `/invitations/[token]`.
   - Implement `+page.server.ts` to validate token existence, check expiration timestamp, and verify the token has not already been accepted.
2. **Organization Membership Provisioning**:
   - Upon valid token redemption, create the `organization_members` record with the invited role.
   - Mark the invitation token as redeemed (`accepted_at = NOW()`).
   - Redirect the user to the active organization dashboard (`/[orgSlug]`).

### Affected Files
- `apps/dashboard-web/src/routes/auth/accept-invite/+page.server.ts` (New)
- `apps/dashboard-web/src/routes/auth/accept-invite/+page.svelte` (New)

### Acceptance Criteria
- An invited user clicking an invitation link can redeem their token.
- Invalid, expired, or previously redeemed tokens show an explicit error message.
- Successful redemption provisions the user with the target role and redirects to the organization workspace.
