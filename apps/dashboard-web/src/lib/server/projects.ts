import { db } from './db';
import { projects, projectMembers } from '$lib/db/schema';
import type { Role, ProjectAccess } from '$lib/rbac';
import { eq, and } from 'drizzle-orm';

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