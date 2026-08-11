/**
 * Project-level role, as defined by `project_members.role` (see
 * packages/db-migrations/migrations/1716550000_add_project_members.sql):
 *   CHECK (role IN ('admin', 'developer', 'viewer', 'support'))
 *
 * Kept at its historical 3 values ('support' intentionally NOT added here) so
 * `Record<Role, number>` in src/lib/server/projects.ts (checkProjectAccess's
 * roleHierarchy) and the project-scoped checks in
 * src/routes/api/alerts/+server.ts keep compiling unchanged — those files are
 * outside this change's scope. A project member whose role is 'support' is
 * therefore still unrecognized by THIS narrower type (falls through
 * checkProjectAccess's lookup to `undefined`, which fails closed/denied, not
 * open). Reconciling that requires updating projects.ts's roleHierarchy too;
 * flagged as a follow-up rather than done here to avoid an out-of-scope edit.
 * `AnyRole` below is what actually needs to (and does) recognize 'support'.
 */
export type Role = 'admin' | 'developer' | 'viewer';

/**
 * Organization-level role, as defined by `organization_members.role` (see
 * packages/db-migrations/migrations/1721800000_add_organization_layer.sql):
 *   CHECK (role IN ('owner', 'admin', 'engineer', 'support', 'viewer'))
 *
 * This is the type API-key-management routes (organization-scoped) must
 * check against. Before this type existed, hasPermission('owner', ...)
 * returned false unconditionally (owner wasn't a member of `Role` at all),
 * denying organization owners every permission.
 */
export type OrgRole = 'owner' | 'admin' | 'engineer' | 'support' | 'viewer';

/**
 * Every role value permitted by either members table. A `Role` (project scope)
 * is always assignable here, so existing callers passing a `Role` to
 * `hasPermission`/`requirePermission` keep working unchanged; new
 * organization-scoped callers pass an `OrgRole` instead.
 */
export type AnyRole = Role | OrgRole;

export interface ProjectAccess {
	projectId: string;
	role: Role;
}

export interface UserPermissions {
	projects: ProjectAccess[];
}

// All role values `project_members.role`'s CHECK constraint permits (including
// 'support', which the narrower `Role` type above deliberately omits).
export const DB_PROJECT_MEMBER_ROLES: readonly AnyRole[] = ['admin', 'developer', 'viewer', 'support'];

// All role values `organization_members.role`'s CHECK constraint permits.
export const DB_ORGANIZATION_MEMBER_ROLES: readonly AnyRole[] = ['owner', 'admin', 'engineer', 'support', 'viewer'];

const ROLE_PERMISSIONS: Record<AnyRole, string[]> = {
	// Project-scoped roles (project_members.role)
	admin: ['read', 'write', 'delete', 'manage_members', 'manage_keys'],
	developer: ['read', 'write'],
	viewer: ['read'],
	// Organization-scoped roles (organization_members.role) not already covered above.
	// 'manage_keys' gates API key lifecycle actions (create/rotate/revoke) per spec 008 FR-007
	// ("owner, admin, engineer"); 'support' and 'viewer' are read-only for key management.
	// 'manage_agents' gates agent identity + agent-key issuance/revocation per
	// docs/plans/MANUAL_ISSUES_DESIGN.md §9's permission matrix ("Manage agents + agent keys" ->
	// owner, admin) -- deliberately narrower than 'manage_keys': 'engineer' can manage ordinary
	// ingest/read/admin keys but not agent identities.
	owner: ['read', 'write', 'delete', 'manage_members', 'manage_keys', 'manage_agents'],
	engineer: ['read', 'write', 'manage_keys'],
	support: ['read'],
};

// NOTE: 'admin' is a single object key shared by BOTH the project-scoped Role and the
// org-scoped OrgRole (both types include the literal 'admin') -- this was already true before
// this change (both roles' admins already shared 'manage_keys'). Granting 'manage_agents' here
// therefore also reaches project-scoped admin callers, same as every other permission on this
// key; nothing in this codebase currently checks 'manage_agents' from a project-scoped context,
// so this is consistent with the existing conflation rather than a new one.
ROLE_PERMISSIONS.admin = [...ROLE_PERMISSIONS.admin, 'manage_agents'];

export function hasPermission(role: AnyRole, permission: string): boolean {
	return ROLE_PERMISSIONS[role]?.includes(permission) ?? false;
}

export function requirePermission(role: AnyRole, permission: string): void {
	if (!hasPermission(role, permission)) {
		throw new Error(`Insufficient permissions: ${permission} requires ${role} role`);
	}
}

// True iff `role` is a role value this module has a permission table entry for. Use this to
// detect a role string read from the database that ROLE_PERMISSIONS doesn't recognize (silently
// falling through hasPermission's `?? false` denies-by-default, which is safe, but is easy to
// mistake for "this role legitimately has no permissions" instead of "this role is unknown to
// rbac.ts" — the exact class of bug this file's reconciliation is meant to close).
export function isKnownRole(role: string): role is AnyRole {
	return Object.prototype.hasOwnProperty.call(ROLE_PERMISSIONS, role);
}

export function getHighestRole(projects: ProjectAccess[]): Role {
	if (projects.some((p) => p.role === 'admin')) return 'admin';
	if (projects.some((p) => p.role === 'developer')) return 'developer';
	return 'viewer';
}
