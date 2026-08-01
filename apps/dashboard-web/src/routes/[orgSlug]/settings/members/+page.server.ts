import { error } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';
import { db } from '$lib/server/db';
import { organizationMembers, users } from '$lib/db/schema';
import { eq } from 'drizzle-orm';

export const load: PageServerLoad = async ({ locals, params }) => {
  const session = await locals.auth();
  if (!session?.user?.email) {
    throw error(401, 'Unauthorized session');
  }

  const currentOrg = locals.currentOrg;
  if (!currentOrg || currentOrg.slug !== params.orgSlug) {
    throw error(403, 'Forbidden: Unauthorized access to organization');
  }

  // Fetch organization members with user details for active organization
  const members = await db
    .select({
      id: organizationMembers.id,
      role: organizationMembers.role,
      user: {
        id: users.id,
        name: users.name,
        email: users.email,
      },
    })
    .from(organizationMembers)
    .innerJoin(users, eq(organizationMembers.userId, users.id))
    .where(eq(organizationMembers.organizationId, currentOrg.id));

  return {
    orgId: currentOrg.id,
    orgSlug: currentOrg.slug,
    members: members.map((m) => ({
      id: m.id,
      userId: m.user.id,
      user: {
        name: m.user.name ?? m.user.email,
        email: m.user.email,
      },
      role: m.role,
    })),
  };
};
