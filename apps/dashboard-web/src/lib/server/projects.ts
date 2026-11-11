import { db } from './db';
import { projects, projectMembers } from '$lib/db/schema';
import type { Role, ProjectAccess } from '$lib/rbac';
import { eq, and } from 'drizzle-orm';

/**
 * Project access as `project_members` records it, and **nothing in this application writes that
 * table** — its only two writers in the whole repository are `tests/e2e/alerting_test.go` and
 * `tests/e2e/dashboard_test.go`.
 *
 * sentinel-project-environments decision 35 therefore moved `/`'s project derivation OFF this
 * function and onto `issue-access.ts: getAccessibleProjects`, which resolves the wide population
 * (`project_members` ∪ `organization_members` in `ISSUE_WRITE_ROLES`). Do not route a new
 * authorisation derivation through here without knowing that: built on this table alone it is empty
 * for every user in any real deployment, which fails open or closed depending on how the caller
 * consumes it — `/` returned every issue in the database, `/search` returned none, and the two
 * issue-detail loaders `403`'d everybody, all three from this one fact.
 */
export async function getUserProjectAccess(
	userId: string,
	projectId?: string
): Promise<ProjectAccess[]> {
	const conditions = [eq(projectMembers.userId, userId)];
	
	if (projectId) {
		conditions.push(eq(projectMembers.projectId, projectId));
	}

	const results = await db.select({
		projectId: projectMembers.projectId,
		role: projectMembers.role,
	})
	.from(projectMembers)
	.where(conditions.length > 1 ? and(...conditions) : conditions[0]);

	return results.map(r => ({
		projectId: r.projectId,
		role: r.role as Role,
	}));
}

/**
 * sentinel-project-environments decision 46: the two issue-detail loaders that were this function's
 * only production callers now call `issue-access.ts: requireProjectAccess(userId, projectId,
 * 'read')` instead. The swap, rather than widening the comparison below, is deliberate:
 * `roleHierarchy` is over PROJECT roles (`admin | developer | viewer`) while organization roles are
 * `owner | admin | engineer | support | viewer`, so `roleHierarchy['engineer']` is `undefined` and
 * `undefined >= 1` is false — widening in place would have been a silent no-op that kept `403`-ing
 * every org-role user, with no error anywhere.
 */
export async function checkProjectAccess(
	userId: string,
	projectId: string,
	requiredRole: Role
): Promise<boolean> {
	const access = await getUserProjectAccess(userId, projectId);
	const projectAccess = access.find(a => a.projectId === projectId);

	if (!projectAccess) {
		return false;
	}

	const roleHierarchy: Record<Role, number> = {
		admin: 3,
		developer: 2,
		viewer: 1,
	};

	return roleHierarchy[projectAccess.role] >= roleHierarchy[requiredRole];
}