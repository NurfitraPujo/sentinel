import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { getUserOrganizations, createOrganization, getUserLastActiveOrg } from '$lib/db/queries/organizations';

export const GET: RequestHandler = async ({ locals }) => {
  const session = await locals.auth();
  if (!session?.user?.id) {
    throw error(401, 'Unauthorized');
  }

  const userId = session.user.id;
  const userOrgs = await getUserOrganizations(userId);
  const activeOrganizationId = await getUserLastActiveOrg(userId);

  return json({
    activeOrganizationId,
    organizations: userOrgs,
  });
};

export const POST: RequestHandler = async ({ request, locals }) => {
  const session = await locals.auth();
  if (!session?.user?.id) {
    throw error(401, 'Unauthorized');
  }

  const body = await request.json();
  const { name, slug, avatarUrl } = body;

  if (!name || !slug) {
    throw error(400, 'Name and slug are required');
  }

  const newOrg = await createOrganization(session.user.id, { name, slug, avatarUrl });
  return json(newOrg, { status: 201 });
};
