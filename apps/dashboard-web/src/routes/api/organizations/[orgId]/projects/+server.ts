import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { hasPermission } from '$lib/rbac';
import { getAccessibleProjects } from '$lib/server/issue-access';
import { requireOrgMembership } from '../keys/_shared';

// GET /api/organizations/[orgId]/projects — sentinel-project-environments §4.10, decision 31.
//
// New because §4.10a's invitation control is PER PROJECT and nothing in this application lists an
// organization's projects for a caller. The directory has existed (for [projectId]/agent-settings
// and [projectId]/repo-connection) with no +server.ts of its own.

/**
 * `read` on the organization, and the list is **the same union** every other decision-31 surface
 * uses — the projects the caller reaches by `project_members` **or** by an `organization_members`
 * role in `ISSUE_WRITE_ROLES`. It is not an org-wide project listing: a caller sees the projects
 * they can already reach.
 *
 * `{ id, name }` and **nothing else** — this feeds a picker, and a project's settings, keys and
 * counts are other endpoints' business.
 */
export const GET: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId } = params;
	if (!orgId) {
		throw error(400, 'Missing organizationId');
	}

	const membership = await requireOrgMembership(session.user.id, orgId);
	if (!membership) {
		throw error(403, 'Forbidden: not a member of this organization');
	}
	if (!hasPermission(membership.role, 'read')) {
		throw error(403, 'Forbidden: not a member of this organization');
	}

	const projects = await getAccessibleProjects(session.user.id, orgId);

	return json({ projects: projects.map((p) => ({ id: p.id, name: p.name })) });
};
