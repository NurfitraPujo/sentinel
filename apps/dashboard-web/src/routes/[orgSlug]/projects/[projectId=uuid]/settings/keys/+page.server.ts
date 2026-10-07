import { error } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';

export const load: PageServerLoad = async ({ params, fetch, locals }) => {
	const { orgSlug, projectId } = params;

	// D39: authenticate BEFORE any org/project lookup — see the sibling org-level loader
	// (settings/keys/+page.server.ts) for the full rationale. `orgHandle` does not gate anonymous
	// requests, so this loader must check itself.
	const session = await locals.auth();
	if (!session?.user?.email) {
		throw error(401, 'Unauthorized');
	}

	const currentOrg = locals.currentOrg;
	if (!currentOrg || currentOrg.slug !== orgSlug) {
		throw error(403, 'Forbidden: Unauthorized access to organization');
	}

	const org = { id: currentOrg.id, name: currentOrg.name };

	// Verify project exists in org
	const [project] = await db
		.select({ id: projects.id, name: projects.name })
		.from(projects)
		.where(and(eq(projects.id, projectId), eq(projects.organizationId, org.id)));

	if (!project) {
		throw error(404, 'Project not found in this organization');
	}

	// Fetch org keys via API
	const res = await fetch(`/api/organizations/${org.id}/keys`);
	if (!res.ok) {
		const err = await res.json().catch(() => ({ message: 'Failed to load keys' }));
		throw error(res.status, err.message || 'Failed to fetch API keys');
	}

	const { keys } = await res.json();

	// Server-side filter to return only keys targeted to this project
	// D37: attach the project NAME. The keys API returns only `projectId`, so without this every
	// row on this page rendered a raw UUID in the "Target" column (ApiKeyTable falls back to
	// `key.projectId` when `targetProject` is absent). The org-level loader already does this; the
	// project-scoped one was missed — and here the name is trivially known, since every key on this
	// page belongs to the project we just looked up.
	const projectKeys = (keys || [])
		.filter((k: any) => k.projectId === projectId)
		.map((k: any) => ({ ...k, targetProject: project.name }));

	return {
		orgId: org.id,
		orgSlug,
		projectId,
		projectName: project.name,
		keys: projectKeys,
		projects: [{ id: project.id, name: project.name }],
	};
};
