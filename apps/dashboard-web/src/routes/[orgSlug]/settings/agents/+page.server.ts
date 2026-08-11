import { error } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';

// Manual Issues M5 §7/§9: mirrors settings/keys/+page.server.ts's shape -- authenticate BEFORE
// any lookup that would reveal whether an organization exists (D39), then delegate the actual
// list to the already-guarded API route so there is exactly one place ('manage_agents') that
// decides who may see the roster.
export const load: PageServerLoad = async ({ params, fetch, locals }) => {
	const { orgSlug } = params;

	const session = await locals.auth();
	if (!session?.user?.email) {
		throw error(401, 'Unauthorized');
	}

	const currentOrg = locals.currentOrg;
	if (!currentOrg || currentOrg.slug !== orgSlug) {
		throw error(403, 'Forbidden: Unauthorized access to organization');
	}

	const org = { id: currentOrg.id, name: currentOrg.name };

	const res = await fetch(`/api/organizations/${org.id}/agents`);
	if (!res.ok) {
		const err = await res.json().catch(() => ({ message: 'Failed to load agents' }));
		throw error(res.status, err.message || 'Failed to fetch agents');
	}
	const { agents } = await res.json();

	return {
		orgId: org.id,
		orgSlug,
		agents: agents || [],
	};
};
