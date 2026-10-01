import { redirect } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';
import { updateUserLastActiveOrg } from '$lib/db/queries/organizations';
import { db } from '$lib/server/db';
import { users } from '$lib/db/schema';
import { eq } from 'drizzle-orm';

/**
 * Organization root route (/[orgSlug]).
 *
 * Redirects the user to the organization's projects directory (/[orgSlug]/projects),
 * updating their lastActiveOrganizationId preference if authenticated.
 */
export const load: PageServerLoad = async ({ params, locals }) => {
	const session = await locals.auth();
	if (session?.user?.email && locals.currentOrg?.id) {
		const userResult = await db
			.select({ id: users.id })
			.from(users)
			.where(eq(users.email, session.user.email))
			.limit(1);
		if (userResult.length > 0) {
			await updateUserLastActiveOrg(userResult[0].id, locals.currentOrg.id);
		}
	}
	throw redirect(303, `/${params.orgSlug}/projects`);
};
