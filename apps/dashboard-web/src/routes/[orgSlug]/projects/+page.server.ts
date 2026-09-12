import { error, fail } from '@sveltejs/kit';
import { z } from 'zod';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';
import { hasPermission } from '$lib/rbac';
import { createProject } from '$lib/db/queries/organizations';
import { requireOrgMembership } from '../../api/organizations/[orgId]/keys/_shared';
import type { PageServerLoad, Actions } from './$types';

// Projects had no index either, so a project's own settings and API-key pages
// (/[orgSlug]/projects/[projectId]/settings/keys) could only be reached by someone who already
// knew the project's UUID.
//
// Project creation had two surfaces and neither reached an existing organization's members:
// POST /api/organizations/[orgId]/projects had no UI caller, and /onboarding creates for orgs[0]
// only and is reachable only while the user can see zero projects. This page's `create` action
// is the missing path for everyone else. Same validation and the same 'write' gate as the POST
// endpoint, so the two surfaces cannot drift on who may create or what a name may be.

const projectSchema = z.object({
	name: z.string().trim().min(1, 'Project name is required').max(255, 'Project name must be 255 characters or fewer'),
});

// D39: authenticate before any lookup that reveals whether an organization exists.
async function requireOrgContext(locals: App.Locals, orgSlug: string) {
	const session = await locals.auth();
	if (!session?.user?.email || !session.user.id) {
		error(401, 'Unauthorized');
	}

	const currentOrg = locals.currentOrg;
	if (!currentOrg || currentOrg.slug !== orgSlug) {
		error(403, 'Forbidden: Unauthorized access to organization');
	}

	return { userId: session.user.id, currentOrg };
}

export const load: PageServerLoad = async ({ params, locals }) => {
	const { userId, currentOrg } = await requireOrgContext(locals, params.orgSlug);

	const orgProjects = await db
		.select({ id: projects.id, name: projects.name, createdAt: projects.createdAt })
		.from(projects)
		.where(and(eq(projects.organizationId, currentOrg.id), eq(projects.isInbox, false)));

	// Role is read off the caller's own membership row, never off anything the client sent (B7).
	// `canCreate` only decides whether the form is rendered; the action re-checks regardless.
	const membership = await requireOrgMembership(userId, currentOrg.id);
	const canCreate = !!membership && hasPermission(membership.role, 'write');

	return {
		org: { name: currentOrg.name, slug: currentOrg.slug },
		projects: orgProjects,
		canCreate,
	};
};

export const actions: Actions = {
	create: async ({ params, request, locals }) => {
		const { userId, currentOrg } = await requireOrgContext(locals, params.orgSlug);

		// 'write' rather than 'manage_members', mirroring POST /api/organizations/[orgId]/projects:
		// owner, admin and engineer may create a project, a viewer or support member may not.
		const membership = await requireOrgMembership(userId, currentOrg.id);
		if (!membership || !hasPermission(membership.role, 'write')) {
			return fail(403, { error: 'You do not have permission to create projects in this organization' });
		}

		const form = Object.fromEntries(await request.formData());
		const parsed = projectSchema.safeParse(form);
		if (!parsed.success) {
			return fail(400, { error: parsed.error.issues[0].message, values: { name: String(form.name ?? '') } });
		}

		// Tenant scope is the org resolved from the caller's membership (locals.currentOrg), not a
		// body field — the form carries only the name.
		let project;
		try {
			project = await createProject(userId, currentOrg.id, { name: parsed.data.name });
		} catch (err) {
			// `projects` is UNIQUE (organization_id, name), and every org with a report inbox already
			// owns a hidden project named 'Triage'. A taken name is the one failure the user can act
			// on, so name it and echo the input back instead of letting the 23505 escape the action
			// (which would land on the generic error page and lose what they typed).
			const message = err instanceof Error ? err.message : '';
			if (/unique|duplicate/i.test(message)) {
				return fail(409, {
					error: `A project named '${parsed.data.name}' already exists in ${currentOrg.name}`,
					values: { name: parsed.data.name },
				});
			}
			throw err;
		}

		// `use:enhance` re-runs `load` on success, so the new row appears in the list without a
		// redirect; `created` lets the page confirm it by name.
		return { created: { id: project.id, name: project.name } };
	},
};
