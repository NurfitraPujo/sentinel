export type Role = 'admin' | 'developer' | 'viewer';

export interface ProjectAccess {
	projectId: string;
	role: Role;
}

export interface UserPermissions {
	projects: ProjectAccess[];
}

const ROLE_PERMISSIONS: Record<Role, string[]> = {
	admin: ['read', 'write', 'delete', 'manage_members'],
	developer: ['read', 'write'],
	viewer: ['read'],
};

export function hasPermission(role: Role, permission: string): boolean {
	return ROLE_PERMISSIONS[role]?.includes(permission) ?? false;
}

export function requirePermission(role: Role, permission: string): void {
	if (!hasPermission(role, permission)) {
		throw new Error(`Insufficient permissions: ${permission} requires ${role} role`);
	}
}

export function getHighestRole(projects: ProjectAccess[]): Role {
	if (projects.some(p => p.role === 'admin')) return 'admin';
	if (projects.some(p => p.role === 'developer')) return 'developer';
	return 'viewer';
}