import { json } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { and, eq, inArray } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { organizationMembers, projectMembers, users } from '$lib/db/schema';
import { ISSUE_WRITE_ROLES } from '$lib/server/issue-access';
import { listProjectEnvironmentGrants } from '$lib/db/queries/environments';
import { MANAGE_MEMBERS_FORBIDDEN, requireProjectRoute } from '../../_shared';

// GET /api/organizations/[orgId]/projects/[projectId]/environment-grants
// sentinel-project-environments §4.10, decision 31.
//
// `manage_members`. **Not** environment-scope-guarded: granting is a management action, and §4.1's
// includeHidden=true rule already establishes that an environment's NAME is visible to whoever
// administers the project.

interface MemberRow {
	userId: string;
	displayName: string;
	projectRole: string | null;
	orgRole: string | null;
	environments: string[] | null;
}

/**
 * Returns **the project's population and their grants** — decision 31. An earlier revision of the
 * contract returned the grant rows alone, which is unbuildable as a screen: the section has to show
 * every member with a row saying "All environments", and a caller given only the narrowed ones has
 * no list to put them beside.
 *
 * The population is `project_members` ∪ `organization_members` in `ISSUE_WRITE_ROLES` — §4.3a's
 * rule, and the same one the `PUT` beside this resolves its `404` by, so the `GET` and the `PUT`
 * agree about who exists. Nothing in this application writes `project_members`, so on a real
 * deployment every row here comes from the org half.
 *
 * `environments: null` means **unrestricted** — no grant row. Deliberately `null` and not `[]`:
 * `[]` is a state the `PUT` refuses to store, and using it here would make "sees nothing" and
 * "sees everything" the same value on the wire.
 */
export const GET: RequestHandler = async ({ params, locals }) => {
	const { orgId, projectId } = await requireProjectRoute(
		locals,
		params,
		'manage_members',
		MANAGE_MEMBERS_FORBIDDEN
	);

	const grants = await listProjectEnvironmentGrants(projectId);
	const grantByUser = new Map(grants.map((g) => [g.userId, g.environments]));

	const orgRows = await db
		.select({ userId: organizationMembers.userId, role: organizationMembers.role })
		.from(organizationMembers)
		.where(
			and(
				eq(organizationMembers.organizationId, orgId),
				inArray(organizationMembers.role, ISSUE_WRITE_ROLES as unknown as string[])
			)
		);

	const projectRows = await db
		.select({ userId: projectMembers.userId, role: projectMembers.role })
		.from(projectMembers)
		.where(eq(projectMembers.projectId, projectId));

	const orgRoleByUser = new Map(orgRows.map((r) => [r.userId, r.role]));
	const projectRoleByUser = new Map(projectRows.map((r) => [r.userId, r.role]));
	const userIds = [...new Set([...orgRoleByUser.keys(), ...projectRoleByUser.keys()])];

	const userRows = userIds.length
		? await db
				.select({ id: users.id, name: users.name, email: users.email })
				.from(users)
				.where(inArray(users.id, userIds))
		: [];
	const userById = new Map(userRows.map((u) => [u.id, u]));

	const members: MemberRow[] = userIds
		.map((userId) => {
			const user = userById.get(userId);
			return {
				userId,
				// Falls back to the email local part, as the org members screen already does.
				displayName: user?.name ?? user?.email?.split('@')[0] ?? userId,
				projectRole: projectRoleByUser.get(userId) ?? null,
				orgRole: orgRoleByUser.get(userId) ?? null,
				environments: grantByUser.get(userId) ?? null,
			};
		})
		.sort((a, b) => a.displayName.localeCompare(b.displayName));

	return json({ grants, members });
};
