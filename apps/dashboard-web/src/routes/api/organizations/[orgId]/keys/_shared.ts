// Shared helpers for the API key management routes in this directory. Not a route itself
// (SvelteKit only treats +page/+layout/+server-named files as routes), just a co-located module.
import { db } from '$lib/server/db';
import { organizationMembers, projects } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';
import type { OrgRole } from '$lib/rbac';

// requireOrgMembership looks up the CALLER's own membership row for orgId, keyed off the
// authenticated session's user id — never off anything in the request body or URL beyond orgId
// itself. This is the check VERIFIED_STATE.md S6 warns is easy to skip: authority must come from
// who the caller is (session.user.id), not from what they claim (a body field), and the org scope
// must come from the membership row, not be assumed from the URL.
export async function requireOrgMembership(
	userId: string,
	orgId: string
): Promise<{ role: OrgRole } | undefined> {
	const [membership] = await db
		.select({ role: organizationMembers.role })
		.from(organizationMembers)
		.where(and(eq(organizationMembers.organizationId, orgId), eq(organizationMembers.userId, userId)));

	if (!membership) return undefined;
	return { role: membership.role as OrgRole };
}

// resolveProjectInOrg maps a project id OR name to its id, scoped to organizationId. A project
// belonging to a different organization is treated identically to "does not exist" (undefined) —
// deliberately indistinguishable, so this cannot be used to enumerate another tenant's projects
// (same reasoning as apps/ingestor-go/auth/apikey.go's ResolveProjectInOrg, mirrored here for the
// dashboard's own project-scoped key creation).
export async function resolveProjectInOrg(
	organizationId: string,
	projectIdOrName: string
): Promise<string | undefined> {
	const [row] = await db
		.select({ id: projects.id })
		.from(projects)
		.where(
			and(
				eq(projects.organizationId, organizationId),
				// projectIdOrName may be a UUID (id) or a human-entered project name; either way the
				// match MUST also be scoped to this organization.
				projectIdOrName.length === 36
					? eq(projects.id, projectIdOrName)
					: eq(projects.name, projectIdOrName)
			)
		);
	return row?.id;
}
