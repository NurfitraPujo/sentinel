import type { PageServerLoad, Actions } from './$types';
import { db } from '$lib/server/db';
import { projects, projectMembers, alertConfigs } from '$lib/db/schema';
import { requireAuth } from '$lib/server/auth';
import { eq, and, inArray, isNotNull } from 'drizzle-orm';
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

	// alert_configs.channel_config is JSONB, not a flat channel_target column — see schema.ts and
	// src/routes/api/alerts/+server.ts for the full note. Mapped to the same channelTarget/windowSeconds
	// shape the page already renders, so +page.svelte needs no change.
	const rawAlertConfigs = await db
		.select({
			id: alertConfigs.id,
			projectId: alertConfigs.projectId,
			channel: alertConfigs.channel,
			channelConfig: alertConfigs.channelConfig,
			frequencyThreshold: alertConfigs.frequencyThreshold,
			frequencyWindowSeconds: alertConfigs.frequencyWindowSeconds,
			enabled: alertConfigs.enabled,
			createdAt: alertConfigs.createdAt,
		})
		.from(alertConfigs)
		.where(
			and(
				// alert_configs.project_id is now nullable (1722100000_add_alert_config_org_layer.sql):
				// NULL means an organization-wide config, which this project-scoped settings page does not
				// yet render (see src/routes/api/alerts/+server.ts for the org-wide layer). Excluding NULL
				// explicitly here is what lets the `row.projectId as string` cast below stay honest — the
				// inArray predicate alone would already never match a NULL row, but wouldn't say so to the
				// type checker.
				isNotNull(alertConfigs.projectId),
				inArray(
					alertConfigs.projectId,
					userProjectMemberships.map((m) => m.projectId)
				)
			)
		);

	const filteredAlertConfigs = rawAlertConfigs.map((row) => {
		const channelConfig = (row.channelConfig ?? {}) as Record<string, unknown>;
		return {
			id: row.id,
			projectId: row.projectId as string,
			channel: row.channel,
			channelTarget: typeof channelConfig.target === 'string' ? channelConfig.target : '',
			frequencyThreshold: row.frequencyThreshold,
			windowSeconds: row.frequencyWindowSeconds,
			enabled: row.enabled,
			createdAt: row.createdAt,
		};
	});

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