import type { PageServerLoad, Actions } from './$types';
import { db } from '$lib/server/db';
import { projects, projectMembers, alertConfigs } from '$lib/db/schema';
import { requireAuth } from '$lib/server/auth';
import { eq, and, inArray } from 'drizzle-orm';
import { hasPermission, type Role } from '$lib/rbac';

export const load: PageServerLoad = async ({ locals }) => {
	const user = await requireAuth({ locals } as any);

	const userProjectMemberships = await db
		.select({
			projectId: projectMembers.projectId,
			role: projectMembers.role,
		})
		.from(projectMembers)
		.where(eq(projectMembers.userId, user.id));

	const filteredAlertConfigs = await db
		.select({
			id: alertConfigs.id,
			projectId: alertConfigs.projectId,
			channel: alertConfigs.channel,
			channelTarget: alertConfigs.channelTarget,
			frequencyThreshold: alertConfigs.frequencyThreshold,
			windowSeconds: alertConfigs.windowSeconds,
			enabled: alertConfigs.enabled,
			createdAt: alertConfigs.createdAt,
		})
		.from(alertConfigs)
		.where(
			inArray(
				alertConfigs.projectId,
				userProjectMemberships.map((m) => m.projectId)
			)
		);

	const userProjects = await db
		.select({
			id: projects.id,
			name: projects.name,
		})
		.from(projects)
		.innerJoin(projectMembers, eq(projectMembers.projectId, projects.id))
		.where(eq(projectMembers.userId, user.id));

	const projectRoleMap: Record<string, Role> = {};
	for (const membership of userProjectMemberships) {
		projectRoleMap[membership.projectId] = membership.role as Role;
	}

	const editableAlertConfigs = filteredAlertConfigs.filter((config) => {
		const role = projectRoleMap[config.projectId];
		return role && hasPermission(role, 'write');
	});

	return {
		alertConfigs: filteredAlertConfigs,
		editableAlertConfigs,
		projects: userProjects,
		projectRoles: projectRoleMap,
	};
};