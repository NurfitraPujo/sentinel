import { describe, it, expect } from 'vitest';
import { hasPermission, type OrgRole, type Role } from '$lib/rbac';

describe('Alerts Settings Server RBAC Loader logic', () => {
	it('correctly calculates editability for project-scoped configs based on write permission', () => {
		const projectRoleMap: Record<string, Role> = {
			'proj-1': 'admin',
			'proj-2': 'developer',
			'proj-3': 'viewer',
		};

		const configs = [
			{ id: '1', projectId: 'proj-1', organizationId: 'org-1' },
			{ id: '2', projectId: 'proj-2', organizationId: 'org-1' },
			{ id: '3', projectId: 'proj-3', organizationId: 'org-1' },
		];

		const editable = configs.filter((config) => {
			const role = projectRoleMap[config.projectId];
			return role ? hasPermission(role, 'write') : false;
		});

		expect(editable.map((c) => c.id)).toEqual(['1', '2']);
	});

	it('correctly calculates editability for organization-wide configs based on manage_keys permission', () => {
		const orgRoleMap: Record<string, OrgRole> = {
			'org-owner': 'owner',
			'org-admin': 'admin',
			'org-engineer': 'engineer',
			'org-support': 'support',
			'org-viewer': 'viewer',
		};

		const orgWideConfigs = [
			{ id: 'org-cfg-1', projectId: null, organizationId: 'org-owner' },
			{ id: 'org-cfg-2', projectId: null, organizationId: 'org-admin' },
			{ id: 'org-cfg-3', projectId: null, organizationId: 'org-engineer' },
			{ id: 'org-cfg-4', projectId: null, organizationId: 'org-support' },
			{ id: 'org-cfg-5', projectId: null, organizationId: 'org-viewer' },
		];

		const editable = orgWideConfigs.filter((config) => {
			const orgRole = orgRoleMap[config.organizationId];
			return orgRole ? hasPermission(orgRole, 'manage_keys') : false;
		});

		expect(editable.map((c) => c.id)).toEqual(['org-cfg-1', 'org-cfg-2', 'org-cfg-3']);
	});
});
