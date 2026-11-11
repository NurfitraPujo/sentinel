import { db } from '$lib/server/db';
import { projectMembers } from '$lib/db/schema';
import { eq } from 'drizzle-orm';
import type { Role, ProjectAccess } from '$lib/rbac';

export async function getUserProjectRoles(userId: string): Promise<ProjectAccess[]> {
	const rows = await db
		.select({
			projectId: projectMembers.projectId,
			role: projectMembers.role,
		})
		.from(projectMembers)
		.where(eq(projectMembers.userId, userId));

	return rows.map((row) => ({
		projectId: row.projectId,
		role: row.role as Role,
	}));
}