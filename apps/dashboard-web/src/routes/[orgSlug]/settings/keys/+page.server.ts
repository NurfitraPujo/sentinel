import { error } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';
import { db } from '$lib/server/db';
import { organizations, projects } from '$lib/db/schema';
import { eq } from 'drizzle-orm';

export const load: PageServerLoad = async ({ params, fetch }) => {
	const { orgSlug } = params;

	// Resolve organization ID from slug
	const [org] = await db
		.select({ id: organizations.id, name: organizations.name })
		.from(organizations)
		.where(eq(organizations.slug, orgSlug));

	if (!org) {
		throw error(404, 'Organization not found');
	}

	// Fetch projects for target project select dropdown
	const orgProjects = await db
		.select({ id: projects.id, name: projects.name })
		.from(projects)
		.where(eq(projects.organizationId, org.id));

	// Call backend API endpoint to list keys
	const res = await fetch(`/api/organizations/${org.id}/keys`);
	if (!res.ok) {
		const err = await res.json().catch(() => ({ message: 'Failed to load keys' }));
		throw error(res.status, err.message || 'Failed to fetch API keys');
	}

	const { keys } = await res.json();

	return {
		orgId: org.id,
		orgSlug,
		keys: keys || [],
		projects: orgProjects || [],
	};
};
