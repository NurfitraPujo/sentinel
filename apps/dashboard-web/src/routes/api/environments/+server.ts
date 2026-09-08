import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { resolveEnvironmentUnion } from '$lib/server/environment-union';
import { requireOrgMembership } from '../organizations/[orgId]/keys/_shared';

// GET /api/environments — sentinel-project-environments §4.3a, the cross-project union.
//
// `/search` is not project-scoped and an org-wide alert config (projectId: null) spans projects;
// both need a list of environment names that is not one project's. This is that list.

/**
 * **This endpoint is a guard, not a convenience.**
 *
 * The union is built **per project, after the §2.2 scope resolution, and only from projects the
 * caller may see**. A union assembled from every project in the organization would hand a scoped
 * member the complete list of production environment names across the org, which is exactly the
 * leak decision 12 exists to close.
 *
 * Two rules that are easy to invert and are both load-bearing:
 *
 * - **`hidden` rows are excluded per project, BEFORE de-duplication (decision 43).** Two projects
 *   may both declare `canary` and hide it in only one of them; de-duplicating first would let the
 *   un-hidden one carry the hidden one into the union. There is no `includeHidden` parameter here
 *   and none is to be added — §4.1's requires `write`, which an org-wide alert author may not hold.
 * - **A project ABSENT from the scopes map is unrestricted, not denied** — `scopeForProject`, and
 *   the inverse reading would return an empty union to every unrestricted caller.
 *
 * With no `organizationId` the population spans ORGANIZATIONS, deliberately (decision 47):
 * `/search` is the caller that omits it and `/search` is not organization-scoped either. A name is
 * not a secret the way an issue is — every name here came from a project the caller may already
 * read, after the scope resolution and after the `hidden` exclusion.
 */
export const GET: RequestHandler = async ({ locals, url }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	const organizationId = url.searchParams.get('organizationId') ?? undefined;
	if (organizationId) {
		const membership = await requireOrgMembership(userId, organizationId);
		if (!membership) {
			throw error(403, 'Forbidden: not a member of this organization');
		}
	}

	// The union itself is $lib/server/environment-union.ts, because PUT /api/alerts's org-wide scope
	// guard is the SAME question and §4.7 requires it to be the same call rather than a second
	// derivation of the rule (decision 26). The two rules that are easy to invert — hidden excluded
	// per project before de-duplication, and absent-from-the-scopes-map means unrestricted — live
	// there with them.
	const names = await resolveEnvironmentUnion(userId, organizationId);

	// The raw name only. No id, no displayName, no isDefault, no timestamps — every one of those
	// belongs to a (project_id, name) row, and two projects' `production` are two different rows
	// with two different labels. Presenting one project's label for a union entry would be a lie
	// the API tells for free.
	return json({
		environments: names.map((name) => ({ name })),
	});
};
