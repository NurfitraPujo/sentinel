import { describe, it, expect } from 'vitest';
import {
	hasPermission,
	isKnownRole,
	DB_PROJECT_MEMBER_ROLES,
	DB_ORGANIZATION_MEMBER_ROLES,
	type AnyRole,
} from './rbac';

// Every role value either members table's CHECK constraint permits. See:
//   packages/db-migrations/migrations/1716550000_add_project_members.sql (project_members.role)
//   packages/db-migrations/migrations/1721800000_add_organization_layer.sql (organization_members.role)
const ALL_DB_ROLES: readonly AnyRole[] = Array.from(
	new Set([...DB_PROJECT_MEMBER_ROLES, ...DB_ORGANIZATION_MEMBER_ROLES])
);

describe('rbac role reconciliation', () => {
	it.each(ALL_DB_ROLES.map((role) => [role]))('%s is a role rbac.ts recognizes', (role) => {
		expect(isKnownRole(role)).toBe(true);
	});

	it('DB_PROJECT_MEMBER_ROLES matches project_members.role CHECK constraint exactly', () => {
		expect(new Set(DB_PROJECT_MEMBER_ROLES)).toEqual(new Set(['admin', 'developer', 'viewer', 'support']));
	});

	it('DB_ORGANIZATION_MEMBER_ROLES matches organization_members.role CHECK constraint exactly', () => {
		expect(new Set(DB_ORGANIZATION_MEMBER_ROLES)).toEqual(
			new Set(['owner', 'admin', 'engineer', 'support', 'viewer'])
		);
	});

	// Regression guard for the specific bug this reconciliation fixes: an organization owner was
	// denied every permission because 'owner' was not a member of the (project-only) `Role` type
	// hasPermission checked against.
	it('organization owner is not denied API key management', () => {
		expect(hasPermission('owner', 'manage_keys')).toBe(true);
	});

	it.each([
		['owner', 'manage_keys', true],
		['admin', 'manage_keys', true],
		['engineer', 'manage_keys', true],
		['support', 'manage_keys', false],
		['viewer', 'manage_keys', false],
	] as const)('org role %s manage_keys -> %s', (role, permission, expected) => {
		expect(hasPermission(role, permission)).toBe(expected);
	});
});
