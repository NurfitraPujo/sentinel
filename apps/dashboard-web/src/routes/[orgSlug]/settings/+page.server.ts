import { error } from '@sveltejs/kit';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';
import type { PageServerLoad } from './$types';

// The index this directory never had. Every section under it — keys, members, agents, on-call,
// observability — was reachable only by typing its URL: the header's "Settings" points at the
// GLOBAL /settings (alerts, auth, observability), and nothing anywhere linked to the org-scoped
// pages. A user who finished onboarding therefore had a project and no way to find the API key
// screen that makes it useful.
//
// Authentication happens BEFORE any lookup that reveals whether an organization exists, for the
// same reason settings/keys/+page.server.ts does it (D39): orgHandle only enforces membership for
// a request that already carries a session, so an anonymous request reaches this loader directly
// and must not be able to tell a real slug from a made-up one.
export const load: PageServerLoad = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.email) {
		error(401, 'Unauthorized');
	}

	const currentOrg = locals.currentOrg;
	if (!currentOrg || currentOrg.slug !== params.orgSlug) {
		error(403, 'Forbidden: Unauthorized access to organization');
	}

	// Inbox projects are excluded, as they are from the dashboard and alert evaluation: they hold
	// unassigned reports and have no key of their own to manage.
	const orgProjects = await db
		.select({ id: projects.id, name: projects.name })
		.from(projects)
		.where(and(eq(projects.organizationId, currentOrg.id), eq(projects.isInbox, false)));

	return {
		org: { name: currentOrg.name, slug: currentOrg.slug },
		projects: orgProjects,
	};
};
