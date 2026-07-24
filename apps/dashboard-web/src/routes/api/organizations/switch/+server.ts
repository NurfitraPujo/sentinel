import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { updateUserLastActiveOrg } from '$lib/db/queries/organizations';
import { db } from '$lib/db';
import { organizations, organizationMembers } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';

export const POST: RequestHandler = async ({ request, locals }) => {
  const session = await locals.getSession();
  if (!session?.user?.id) {
    throw error(401, 'Unauthorized');
  }

  const { organizationId } = await request.json();
  if (!organizationId) {
    throw error(400, 'organizationId is required');
  }

  const userId = session.user.id;

  // Verify membership in target organization
  const [membership] = await db
    .select()
    .from(organizationMembers)
    .where(
      and(
        eq(organizationMembers.organizationId, organizationId),
        eq(organizationMembers.userId, userId)
      )
    );

  if (!membership) {
    throw error(403, 'Forbidden: Not a member of target organization');
  }

  const [org] = await db
    .select()
    .from(organizations)
    .where(eq(organizations.id, organizationId));

  await updateUserLastActiveOrg(userId, organizationId);

  return json({
    success: true,
    activeOrganization: org,
    redirectUrl: `/${org.slug}/projects`,
  });
};
