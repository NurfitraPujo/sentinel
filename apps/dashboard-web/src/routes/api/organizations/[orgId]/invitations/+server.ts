import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { createOrganizationInvitation } from '$lib/db/queries/organizations';
import { db } from '$lib/db';
import { organizationMembers } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';
import crypto from 'crypto';

export const POST: RequestHandler = async ({ params, request, locals }) => {
  const session = await locals.getSession();
  if (!session?.user?.id) {
    throw error(401, 'Unauthorized');
  }

  const { orgId } = params;
  const { email, role } = await request.json();

  if (!email || !role) {
    throw error(400, 'Email and role are required');
  }

  // Check admin/owner permissions
  const [membership] = await db
    .select()
    .from(organizationMembers)
    .where(
      and(
        eq(organizationMembers.organizationId, orgId),
        eq(organizationMembers.userId, session.user.id)
      )
    );

  if (!membership || !['owner', 'admin'].includes(membership.role)) {
    throw error(403, 'Forbidden: Only owners and admins can issue invitations');
  }

  const token = crypto.randomBytes(32).toString('hex');
  const expiresAt = new Date(Date.now() + 7 * 24 * 60 * 60 * 1000); // 7 days

  const invite = await createOrganizationInvitation(orgId, email, role, token, expiresAt);
  return json(invite, { status: 201 });
};
