import type { PageServerLoad } from './$types';
import { db } from '$lib/server/db';
import { projects, projectMembers, organizationMembers, organizations, alertConfigs } from '$lib/db/schema';
import { requireAuth } from '$lib/server/auth';
import { eq, and, or, inArray, isNull } from 'drizzle-orm';
import { hasPermission, type Role, type OrgRole } from '$lib/rbac';
import { channelTargetOf } from '$lib/alerts';

export const load: PageServerLoad = async ({ locals }) => {
	const user = await requireAuth({ locals } as any);

	const [userProjectMemberships, userOrgMemberships] = await Promise.all([
		db
			.select({
				projectId: projectMembers.projectId,
				role: projectMembers.role,
			})
			.from(projectMembers)
			.where(eq(projectMembers.userId, user.id)),
		db
			.select({
				organizationId: organizationMembers.organizationId,
				role: organizationMembers.role,
			})
			.from(organizationMembers)
			.where(eq(organizationMembers.userId, user.id)),
	]);

	const projectIds = userProjectMemberships.map((m) => m.projectId);
	const orgIds = userOrgMemberships.map((m) => m.organizationId);

	// Alert configs can be project-scoped (projectId matches user's projects) or
	// organization-wide (projectId IS NULL and organizationId matches user's orgs).
	const visibility = [];
	if (projectIds.length > 0) {
		visibility.push(inArray(alertConfigs.projectId, projectIds));
	}
	if (orgIds.length > 0) {
		visibility.push(and(isNull(alertConfigs.projectId), inArray(alertConfigs.organizationId, orgIds)));
	}

	const rawAlertConfigs =
		visibility.length > 0
			? await db
					.select({
						id: alertConfigs.id,
						organizationId: alertConfigs.organizationId,
						projectId: alertConfigs.projectId,
						channel: alertConfigs.channel,
						channelConfig: alertConfigs.channelConfig,
						frequencyThreshold: alertConfigs.frequencyThreshold,
						frequencyWindowSeconds: alertConfigs.frequencyWindowSeconds,
						enabled: alertConfigs.enabled,
						createdAt: alertConfigs.createdAt,
					})
					.from(alertConfigs)
					.where(visibility.length === 1 ? visibility[0] : or(...visibility))
			: [];

	const alertConfigsList = rawAlertConfigs.map((row) => {
		const channelConfig = (row.channelConfig ?? {}) as Record<string, unknown>;
		return {
			id: row.id,
			scope: (row.projectId === null ? 'organization' : 'project') as 'organization' | 'project',
			organizationId: row.organizationId,
			projectId: row.projectId,
			channel: row.channel,
			channelTarget: channelTargetOf(row.channel, channelConfig),
			frequencyThreshold: row.frequencyThreshold,
			windowSeconds: row.frequencyWindowSeconds,
			enabled: row.enabled,
			createdAt: row.createdAt,
		};
	});


	const userProjects =
		projectIds.length > 0
			? await db
					.select({
						id: projects.id,
						name: projects.name,
						organizationId: projects.organizationId,
					})
					.from(projects)
					.innerJoin(projectMembers, eq(projectMembers.projectId, projects.id))
					.where(eq(projectMembers.userId, user.id))
			: [];

	const projectRoleMap: Record<string, Role> = {};
	for (const membership of userProjectMemberships) {
		projectRoleMap[membership.projectId] = membership.role as Role;
	}

	const orgRoleMap: Record<string, OrgRole> = {};
	for (const membership of userOrgMemberships) {
		orgRoleMap[membership.organizationId] = membership.role as OrgRole;
	}

	// Check if user has manage_keys permission in any of their organizations
	const canManageOrgAlerts = userOrgMemberships.some((m) =>
		hasPermission(m.role as OrgRole, 'manage_keys')
	);

	// Editable configs are project-scoped configs where user has 'write' permission,
	// or org-wide configs where user has 'manage_keys' permission.
	const editableAlertConfigs = alertConfigsList.filter((config) => {
		if (config.projectId === null) {
			const orgRole = orgRoleMap[config.organizationId];
			return orgRole ? hasPermission(orgRole, 'manage_keys') : false;
		}
		const role = projectRoleMap[config.projectId];
		return role ? hasPermission(role, 'write') : false;
	});

	const userOrgsList =
		orgIds.length > 0
			? await db
					.select({
						id: organizations.id,
						name: organizations.name,
					})
					.from(organizations)
					.where(inArray(organizations.id, orgIds))
			: [];

	return {
		alertConfigs: alertConfigsList,
		editableAlertConfigs,
		projects: userProjects,
		projectRoles: projectRoleMap,
		orgRoles: orgRoleMap,
		canManageOrgAlerts,
		userOrganizations: userOrgsList,
	};
};