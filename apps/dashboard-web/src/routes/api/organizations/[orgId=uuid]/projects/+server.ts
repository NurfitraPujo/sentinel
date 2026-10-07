import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';
import { eq, and, inArray } from 'drizzle-orm';
import { hasPermission } from '$lib/rbac';
import { createProject } from '$lib/db/queries/organizations';
import { getAccessibleProjects } from '$lib/server/issue-access';
import { requireOrgMembership } from '../keys/_shared';

// Project creation had no endpoint at all: the only project ever created at runtime was the
// auto-provisioned Triage inbox (lib/db/queries/reports.ts), which is deliberately unusable for
// ingestion. A real project could only be made by writing to the database by hand.
//
// Authority comes from the caller's own membership row, keyed off session.user.id — never from
// anything in the body or the URL beyond orgId. Same rule the key routes follow.
//
// GET is also sentinel-project-environments §4.10, decision 31: §4.10a's invitation control is PER
// PROJECT, and this is the one endpoint that lists an organization's projects for a caller. It has
// two consumers now — the invite modal's environment-scope picker and the agent credential-scope UI
// at /[orgSlug]/settings/agents — and one derivation of who may see what serves both.

/**
 * `read` on the organization, and the list is **the same union** every other decision-31 surface
 * uses — the projects the caller reaches by `project_members` **or** by an `organization_members`
 * role in `ISSUE_WRITE_ROLES`. It is deliberately **not** an org-wide project listing: a caller sees
 * the projects they can already reach. For an owner, admin or engineer that is every project in the
 * organization, so the credential-scope UI sees exactly what it saw before; it narrows only for an
 * org `viewer` or `support` member, who reaches a project solely through a `project_members` row.
 *
 * Inbox projects stay excluded, as they were when this listed the table directly: they are an
 * internal landing place for unassigned reports, not something anyone chose to create, and they are
 * excluded from the error dashboard and alert evaluation for the same reason.
 *
 * `{ id, name }` and nothing else, sorted by name — decision 31's shape, which both consumers (the
 * invite modal and /[orgSlug]/settings/agents) type as exactly that. The 2026-09-10 merge of #8 into
 * the environments branch (678613b) re-selected the rows from `projects` to drop inboxes and in doing
 * so put #8's `createdAt` back and lost getAccessibleProjects' name order (no ORDER BY): the inbox
 * filter below only narrows that list, so the order and shape are the resolver's.
 */
export const GET: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		error(401, 'Unauthorized');
	}

	const { orgId } = params;
	if (!orgId) {
		error(400, 'Missing organizationId');
	}

	const membership = await requireOrgMembership(session.user.id, orgId);
	if (!membership) {
		error(403, 'Forbidden: not a member of this organization');
	}
	if (!hasPermission(membership.role, 'read')) {
		error(403, 'Forbidden: insufficient permissions');
	}

	const accessible = await getAccessibleProjects(session.user.id, orgId);
	if (accessible.length === 0) {
		return json({ projects: [] });
	}

	const visible = await db
		.select({ id: projects.id })
		.from(projects)
		.where(
			and(
				inArray(
					projects.id,
					accessible.map((p) => p.id)
				),
				eq(projects.isInbox, false)
			)
		);
	const visibleIds = new Set(visible.map((r) => r.id));

	return json({
		projects: accessible
			.filter((p) => visibleIds.has(p.id))
			.map((p) => ({ id: p.id, name: p.name })),
	});
};

export const POST: RequestHandler = async ({ params, request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		error(401, 'Unauthorized');
	}

	const { orgId } = params;
	const membership = await requireOrgMembership(session.user.id, orgId!);
	if (!membership) {
		error(403, 'Forbidden: not a member of this organization');
	}
	// 'write' rather than 'manage_members': owner, admin and engineer may create a project, a
	// viewer or support member may not. Raising this to an admin-only gate is a policy change, not
	// a code one — it is this single call.
	if (!hasPermission(membership.role, 'write')) {
		error(403, 'Forbidden: insufficient permissions');
	}

	const body = await request.json().catch(() => ({}) as { name?: unknown });
	const name = typeof body?.name === 'string' ? body.name.trim() : '';
	if (!name) {
		error(400, 'name is required');
	}
	if (name.length > 255) {
		error(400, 'name must be 255 characters or fewer');
	}

	const project = await createProject(session.user.id, orgId!, { name });
	return json({ id: project.id, name: project.name }, { status: 201 });
};
