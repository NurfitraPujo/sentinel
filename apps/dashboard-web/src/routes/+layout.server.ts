import type { LayoutServerLoad } from './$types';

// Manual Issues M1 (docs/plans/MANUAL_ISSUES_DESIGN.md §10): the root layout's nav needs an
// org slug to link to the org-scoped `/[orgSlug]/reports` route. `locals.currentOrg` is already
// resolved per-request by hooks.server.ts's orgHandle (from the URL's leading path segment, or
// the user's last-active org) -- just pass its slug through, nothing new to compute.
export const load: LayoutServerLoad = async ({ locals }) => {
	const session = await locals.auth();
	return {
		session,
		orgSlug: locals.currentOrg?.slug ?? null,
	};
};
