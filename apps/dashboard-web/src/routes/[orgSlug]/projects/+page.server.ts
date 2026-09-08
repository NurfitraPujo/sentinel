import { error } from '@sveltejs/kit';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';
import type { PageServerLoad } from './$types';

// Projects had no index either, so a project's own settings and API-key pages
// (/[orgSlug]/projects/[projectId]/settings/keys) could only be reached by someone who already
// knew the project's UUID.
export const load: PageServerLoad = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.email) {
		error(401, 'Unauthorized');
	}

	const currentOrg = locals.currentOrg;
	if (!currentOrg || currentOrg.slug !== params.orgSlug) {
		error(403, 'Forbidden: Unauthorized access to organization');
	}

	const orgProjects = await db
		.select({ id: projects.id, name: projects.name, createdAt: projects.createdAt })
		.from(projects)
		.where(and(eq(projects.organizationId, currentOrg.id), eq(projects.isInbox, false)));

	return {
		org: { name: currentOrg.name, slug: currentOrg.slug },
		projects: orgProjects,
	};
};
