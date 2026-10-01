import type { LayoutServerLoad } from './$types';
import { getUserOrganizations } from '$lib/db/queries/organizations';
import { db } from '$lib/server/db';
import { users } from '$lib/db/schema';
import { eq } from 'drizzle-orm';

import type { Session } from '@auth/core/types';

// Manual Issues M1 (docs/plans/MANUAL_ISSUES_DESIGN.md §10): the root layout's nav needs an
// org slug to link to the org-scoped `/[orgSlug]/reports` route. `locals.currentOrg` is already
// resolved per-request by hooks.server.ts's orgHandle (from the URL's leading path segment, or
// the user's last-active org) -- just pass its slug through, nothing new to compute.
export const load: LayoutServerLoad = async ({ locals }): Promise<{
	session: Session | null;
	orgSlug: string | null;
	activeOrg?: { id: string; name: string; slug: string; avatarUrl?: string | null } | null;
	organizations?: Array<{ id: string; name: string; slug: string; avatarUrl?: string | null; role?: string }>;
}> => {
	const session = await locals.auth();
	let organizations: Array<{ id: string; name: string; slug: string; avatarUrl?: string | null; role?: string }> = [];

	if (session?.user?.email) {
		const userResult = await db
			.select({ id: users.id })
			.from(users)
			.where(eq(users.email, session.user.email))
			.limit(1);

		if (userResult.length > 0) {
			organizations = await getUserOrganizations(userResult[0].id);
		}
	}

	return {
		session,
		orgSlug: locals.currentOrg?.slug ?? null,
		activeOrg: locals.currentOrg ?? null,
		organizations,
	};
};

