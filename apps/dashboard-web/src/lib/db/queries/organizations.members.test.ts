import { describe, it, expect, vi, beforeEach } from 'vitest';

// G3a (sentinel-occurrence-detail-panels spec §12.3, T30, O8): listOrganizationMembers used
// `users` without importing it, so GET /api/agent/members threw `ReferenceError: users is not
// defined` at runtime. Its route test mocks the function, so nothing caught it. This test calls
// the REAL function with the db mocked by a chain that records its calls.

// vi.mock factories and the import below are hoisted above ordinary consts, so the tables and the
// call log the factories close over are created in vi.hoisted.
const { usersTable, membersTable, calls } = vi.hoisted(() => ({
	usersTable: { __table: 'users', id: 'users.id', name: 'users.name', email: 'users.email' },
	membersTable: {
		__table: 'organization_members',
		id: 'om.id',
		organizationId: 'om.organizationId',
		userId: 'om.userId',
		role: 'om.role',
	},
	calls: [] as { method: string; args: unknown[] }[],
}));

vi.mock('drizzle-orm', () => {
	const eq = (col: unknown, val: unknown) => ({ __op: 'eq', col, val });
	const lt = (col: unknown, val: unknown) => ({ __op: 'lt', col, val });
	const and = (...conds: unknown[]) => ({ __op: 'and', conds });
	const count = () => ({ __op: 'count' });
	const sql = (strings: TemplateStringsArray, ...exprs: unknown[]) => ({ __op: 'raw', strings, exprs });
	return { eq, lt, and, count, sql };
});

vi.mock('$lib/db/schema', () => ({
	organizations: { id: 'id' },
	organizationMembers: membersTable,
	organizationInvitations: { id: 'id' },
	userSessionPreferences: { userId: 'userId' },
	projects: { id: 'id' },
	projectMembers: { id: 'id' },
	projectEnvironmentGrants: { id: 'id' },
	users: usersTable,
}));

vi.mock('$lib/server/db', () => {
	const rows = [{ id: 'm1', role: 'admin', user: { id: 'u1', name: null, email: 'a@x' } }];
	const chain = {
		select: (...args: unknown[]) => (calls.push({ method: 'select', args }), chain),
		from: (...args: unknown[]) => (calls.push({ method: 'from', args }), chain),
		innerJoin: (...args: unknown[]) => (calls.push({ method: 'innerJoin', args }), chain),
		where: (...args: unknown[]) => {
			calls.push({ method: 'where', args });
			return Promise.resolve(rows);
		},
	};
	return { db: chain };
});

import { listOrganizationMembers } from './organizations';

describe('listOrganizationMembers (G3a)', () => {
	beforeEach(() => {
		calls.length = 0;
	});

	it('joins the users table and maps a member with a null name to its email', async () => {
		const result = await listOrganizationMembers('org-1');

		expect(result).toEqual([{ id: 'm1', userId: 'u1', user: { name: 'a@x', email: 'a@x' }, role: 'admin' }]);

		const join = calls.find((c) => c.method === 'innerJoin');
		expect(join).toBeDefined();
		expect(join!.args[0]).toBe(usersTable);
		expect(calls.map((c) => c.method)).toEqual(['select', 'from', 'innerJoin', 'where']);
	});
});
