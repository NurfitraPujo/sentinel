// Shared helpers for the project-scoped environment routes in this directory tree
// (sentinel-project-environments §4.1, §4.2, §4.3, §4.10). Not a route itself — SvelteKit only
// treats `+page`/`+layout`/`+server`-named files as routes — just a co-located module, the same
// shape `../keys/_shared.ts` already uses.
import { error } from '@sveltejs/kit';
import { and, eq } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { organizationMembers, projectMembers, projects } from '$lib/db/schema';
import { hasPermission, type OrgRole } from '$lib/rbac';
import { requireOrgMembership } from '../keys/_shared';
import { listProjectEnvironments } from '$lib/db/queries/environments';
import { ISSUE_WRITE_ROLES } from '$lib/server/issue-access';

export interface ProjectRouteContext {
	userId: string;
	orgId: string;
	projectId: string;
	role: OrgRole;
}

/**
 * §2.1's bar, in §2.1's order, transcribed from
 * `projects/[projectId]/agent-settings/+server.ts`: `locals.auth()` → `requireOrgMembership` →
 * `hasPermission` → project-in-org. Authority is read from the CALLER's own membership row and
 * never from the URL or the body (B7, VERIFIED_STATE.md S6).
 *
 * The project check is LAST deliberately: a caller who is not a member of the organization must not
 * be able to tell an existing project from an absent one, so the `403` precedes the `404`.
 */
export async function requireProjectRoute(
	locals: App.Locals,
	params: { orgId?: string; projectId?: string },
	permission: 'read' | 'write' | 'manage_members',
	forbiddenMessage: string
): Promise<ProjectRouteContext> {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId, projectId } = params;
	if (!orgId || !projectId) {
		throw error(400, 'Missing organizationId or projectId');
	}

	const membership = await requireOrgMembership(session.user.id, orgId);
	if (!membership) {
		throw error(403, 'Forbidden: not a member of this organization');
	}
	if (!hasPermission(membership.role, permission)) {
		throw error(403, forbiddenMessage);
	}

	const [project] = await db
		.select({ id: projects.id })
		.from(projects)
		.where(and(eq(projects.id, projectId), eq(projects.organizationId, orgId)));
	if (!project) {
		throw error(404, 'Project not found in this organization');
	}

	return { userId: session.user.id, orgId, projectId, role: membership.role };
}

/** §7's exact strings, so the three routes that raise them cannot drift apart. */
export const WRITE_FORBIDDEN = 'Forbidden: write permission required to manage environments';
export const MANAGE_MEMBERS_FORBIDDEN =
	'Forbidden: manage_members permission required to manage environment access';

/**
 * §4.1's `includeHidden` rule, and it is deliberately not a boolean parse.
 *
 * **Only the literal string `true` counts.** `1`, `yes`, `TRUE`, an empty value and anything else
 * are all `false` and are **not** an error: a caller who mistypes the parameter gets the ordinary
 * scoped list rather than a rejection they cannot act on.
 */
export function wantsHidden(url: URL): boolean {
	return url.searchParams.get('includeHidden') === 'true';
}

/**
 * §4.10 and §4.10a: validate one `environments` array for a grant.
 *
 * Non-empty, and every name present in that project's registry — **every row of it, `hidden`
 * included (decision 49)**. It reads `listProjectEnvironments`, which is §4.1's `includeHidden=true`
 * view, NOT §4.1's default filtered one: validating through the default view would make a hidden
 * environment ungrantable by accident rather than by decision. A member holding a grant for an
 * environment that appears in no selector is the point, not a cost — a grant is the boundary, not
 * the menu.
 *
 * Throws §7's rows: `400 environments must contain at least one environment`,
 * `400 Unknown environment: <name>`.
 */
export async function validateGrantEnvironments(
	projectId: string,
	value: unknown
): Promise<string[]> {
	if (!Array.isArray(value) || value.length === 0 || !value.every((n) => typeof n === 'string')) {
		throw error(400, 'environments must contain at least one environment');
	}

	const registry = new Set((await listProjectEnvironments(projectId)).map((row) => row.name));
	for (const name of value as string[]) {
		if (!registry.has(name)) {
			throw error(400, `Unknown environment: ${name}`);
		}
	}

	// De-duplicated: `["staging","staging"]` is one grant, and the column is a set in intent even
	// though TEXT[] does not enforce it.
	return [...new Set(value as string[])];
}

/**
 * §2.2 and §4.10's `422`: an organization `owner` cannot be narrowed. The person who can undo a
 * narrowing must never be the person narrowed out — the same class of lockout the org members
 * route's sole-owner guard already refuses.
 */
export function refuseOwner(role: string | undefined): void {
	if (role === 'owner') {
		throw error(422, 'An organization owner cannot be restricted to specific environments');
	}
}

/**
 * §4.10's `404` row: who can reach this project by ANY path — `project_members`, or an
 * `organization_members` role in `ISSUE_WRITE_ROLES`. The `GET` and the `PUT` on that endpoint
 * agree about who exists because both resolve through this rule (decision 31).
 *
 * Returns the target's `organization_members.role` when they hold one, so the caller can apply
 * §4.10's owner `422` without a second query. `hasProjectAccess: false` is §4.10's `404`.
 */
export async function resolveGrantTarget(
	orgId: string,
	projectId: string,
	targetUserId: string
): Promise<{ hasProjectAccess: boolean; orgRole: string | null; projectRole: string | null }> {
	const [orgRow] = await db
		.select({ role: organizationMembers.role })
		.from(organizationMembers)
		.where(
			and(
				eq(organizationMembers.organizationId, orgId),
				eq(organizationMembers.userId, targetUserId)
			)
		);

	const projectRole = await resolveProjectRole(projectId, targetUserId);
	const orgRole = orgRow?.role ?? null;
	const reachesByOrgRole =
		orgRole !== null && (ISSUE_WRITE_ROLES as readonly string[]).includes(orgRole);

	return {
		hasProjectAccess: reachesByOrgRole || projectRole !== null,
		orgRole,
		projectRole,
	};
}

async function resolveProjectRole(projectId: string, userId: string): Promise<string | null> {
	const [row] = await db
		.select({ role: projectMembers.role })
		.from(projectMembers)
		.where(and(eq(projectMembers.projectId, projectId), eq(projectMembers.userId, userId)));
	return row?.role ?? null;
}
