import { db } from './db';
import { projects, project_members } from '$lib/db/schema';
import type { RequestEvent } from '@sveltejs/kit';
import type { Role, ProjectAccess } from '$lib/rbac';
import { eq } from 'drizzle-orm';

export async function getUserProjectAccess(
	userId: string,
	projectId?: string
): Promise<ProjectAccess[]> {
	let query = db.select({
		projectId: project_members.projectId,
		role: project_members.role,
	}).from(project_members).where(eq(project_members.userId, userId));

	if (projectId) {
		query = query.where(eq(project_members.projectId, projectId)) as any;
	}

	const results = await query;
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