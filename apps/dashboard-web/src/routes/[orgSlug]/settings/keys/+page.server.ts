import { error } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';
import { db } from '$lib/server/db';
import { organizations, projects } from '$lib/db/schema';
import { eq } from 'drizzle-orm';

export const load: PageServerLoad = async ({ params, fetch, locals }) => {
	const { orgSlug } = params;

	// D39: authenticate BEFORE any lookup that reveals whether an organization exists. This used
	// to query `organizations` by slug first, so an anonymous request got a 404 for a slug that
	// doesn't exist and a 200 (with keys, via the fetch below) for one that does — an
	// unauthenticated org-existence oracle. `orgHandle` (hooks.server.ts) only enforces membership
	// for a request that already carries a session; an anonymous request sails through it and
	// reaches this loader directly, so the check has to happen here too, same as
	// settings/members/+page.server.ts.
	const session = await locals.auth();
	if (!session?.user?.email) {
		throw error(401, 'Unauthorized');
	}

	const currentOrg = locals.currentOrg;
	if (!currentOrg || currentOrg.slug !== orgSlug) {
		throw error(403, 'Forbidden: Unauthorized access to organization');
	}

	const org = { id: currentOrg.id, name: currentOrg.name };

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

	// D37: the table showed the raw project UUID as "Target" for a project-scoped key even
	// though this loader already has every project's name in `orgProjects` — attach the name
	// here so the table never has to render a bare id.
	const projectNameById = new Map(orgProjects.map((p) => [p.id, p.name]));
	const keysWithTargetNames = (keys || []).map((k: any) =>
		k.projectId ? { ...k, targetProject: projectNameById.get(k.projectId) } : k
	);

	return {
		orgId: org.id,
		orgSlug,
		keys: keysWithTargetNames,
		projects: orgProjects || [],
	};
};
