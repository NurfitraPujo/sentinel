import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { revokeOrganizationInvitation } from '$lib/db/queries/organizations';
import { requireOrgMembership } from '../../keys/_shared';

// D07: the only way to kill a leaked-but-unused invitation token used to be re-inviting the same
// address (which merely raced the original link, still valid until it expired). This endpoint gives
// an owner/admin an explicit revoke, gated the same way invitation creation is.
export const DELETE: RequestHandler = async ({ params, locals }) => {
  const session = await locals.auth();
  if (!session?.user?.id) {
    throw error(401, 'Unauthorized');
  }

  const { orgId, id } = params;
  if (!orgId || !id) {
    throw error(400, 'Missing organizationId or invitationId');
  }

  const membership = await requireOrgMembership(session.user.id, orgId);
  if (!membership || !['owner', 'admin'].includes(membership.role)) {
    throw error(403, 'Forbidden: Only owners and admins can revoke invitations');
  }

  const revoked = await revokeOrganizationInvitation(orgId, id);
  if (!revoked) {
    throw error(404, 'Invitation not found or already resolved');
  }

  return json({ success: true, id: revoked.id, status: revoked.status });
};
