import { error } from '@sveltejs/kit';
import { eq } from 'drizzle-orm';
import type { PageServerLoad } from './$types';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';

// Manual Issues M1 (design §2, §10, Q12): optional project picker -- "Not sure? Leave blank →
// Triage" hint. Excludes existing Triage inbox projects from the picker (isInbox=true) since
// the whole point of leaving the field blank is to land there; offering it as a normal choice
// too would be redundant and confusing.
export const load: PageServerLoad = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const currentOrg = locals.currentOrg;
	if (!currentOrg || currentOrg.slug !== params.orgSlug) {
		throw error(403, 'Forbidden: Unauthorized access to organization');
	}

	const orgProjects = await db
		.select({ id: projects.id, name: projects.name, isInbox: projects.isInbox })
		.from(projects)
		.where(eq(projects.organizationId, currentOrg.id));

	return {
		orgId: currentOrg.id,
		orgSlug: currentOrg.slug,
		projects: orgProjects.filter((p) => !p.isInbox),
	};
};
