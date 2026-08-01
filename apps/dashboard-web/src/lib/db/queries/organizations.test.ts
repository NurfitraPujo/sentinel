import { describe, it, expect, vi, beforeEach } from 'vitest';

// Real 'and'/'eq'/'lt'/'sql' from drizzle-orm build SQL AST objects that are painful to introspect
// generically. Since this file's schema mock (below, matching the convention in
// members.test.ts / issues.test.ts) uses plain strings as column identifiers, we replace
// drizzle-orm's condition builders with tiny descriptor objects keyed the same way, so the fake db
// below can evaluate a WHERE clause against a row by field name -- this lets the tests exercise the
// REAL production code paths in organizations.ts (not a re-implementation of them), including the
// exact WHERE conditions claimInvitation and revokeOrganizationInvitation build.
vi.mock('drizzle-orm', () => {
	const eq = (col: any, val: any) => ({ __op: 'eq', col, val });
	const lt = (col: any, val: any) => ({ __op: 'lt', col, val });
	const and = (...conds: any[]) => ({ __op: 'and', conds: conds.filter(Boolean) });
	const count = () => ({ __op: 'count' });
	function sql(strings: TemplateStringsArray, ...exprs: any[]) {
		return { __op: 'raw', text: strings.join('?'), exprs };
	}
	return { eq, lt, and, count, sql };
});

vi.mock('$lib/db/schema', () => ({
	organizations: { id: 'id', name: 'name', slug: 'slug' },
	organizationMembers: { id: 'id', organizationId: 'organizationId', userId: 'userId', role: 'role' },
	organizationInvitations: {
		id: 'id',
		organizationId: 'organizationId',
		email: 'email',
		role: 'role',
		tokenHash: 'tokenHash',
		status: 'status',
		expiresAt: 'expiresAt',
		createdAt: 'createdAt',
		acceptedAt: 'acceptedAt',
	},
	userSessionPreferences: { userId: 'userId', lastActiveOrganizationId: 'lastActiveOrganizationId' },
	projects: { id: 'id', organizationId: 'organizationId' },
	projectMembers: { id: 'id' },
}));

// --- A minimal in-memory fake db that actually evaluates the WHERE descriptors above against
// per-table row stores, so an UPDATE ... WHERE status='pending' really only matches pending rows.
// This is what lets the concurrency test below prove something about the ACTUAL claimInvitation
// implementation rather than about a hand-scripted mock response sequence.
type Row = Record<string, any>;

function evalCond(row: Row, cond: any): boolean {
	if (!cond) return true;
	if (cond.__op === 'and') return cond.conds.every((c: any) => evalCond(row, c));
	if (cond.__op === 'eq') return row[cond.col] === cond.val;
	if (cond.__op === 'lt') return row[cond.col] < cond.val;
	if (cond.__op === 'raw') {
		// The only raw sql`` usage in organizations.ts is `${expiresAt} > now()`.
		const field = cond.exprs[0];
		return row[field] instanceof Date ? row[field].getTime() > Date.now() : true;
	}
	return true;
}

function makeStore() {
	return {
		organizations: new Map<string, Row>(),
		organizationMembers: new Map<string, Row>(), // keyed `${organizationId}:${userId}`
		organizationInvitations: new Map<string, Row>(), // keyed by id
	};
}

function tableName(table: any): 'organizations' | 'organizationMembers' | 'organizationInvitations' {
	if (table.tokenHash) return 'organizationInvitations';
	if (table.userId && table.role) return 'organizationMembers';
	return 'organizations';
}

let idCounter = 0;

function makeExecutor(store: ReturnType<typeof makeStore>) {
	function rowsFor(name: keyof ReturnType<typeof makeStore>): Row[] {
		return Array.from((store[name] as Map<string, Row>).values());
	}

	return {
		select: (_cols?: any) => ({
			from: (table: any) => {
				const name = tableName(table);
				const where = (cond: any) => {
					const matched = rowsFor(name).filter((r) => evalCond(r, cond));
					return Promise.resolve(matched.map((r) => ({ ...r })));
				};
				// select().from(table) with no .where() (used by listOrganizationInvitations via
				// .where always in this file, but keep this for safety / future callers).
				const thenable: any = Promise.resolve(rowsFor(name).map((r) => ({ ...r })));
				thenable.where = where;
				return thenable;
			},
		}),
		insert: (table: any) => ({
			values: (vals: Row) => {
				const name = tableName(table);
				const insertNow = () => {
					const row: Row = { id: vals.id ?? `${name}-${++idCounter}`, ...vals };
					const key =
						name === 'organizationMembers' ? `${row.organizationId}:${row.userId}` : row.id;
					store[name].set(key, row);
					return row;
				};
				return {
					returning: async () => [insertNow()],
					onConflictDoUpdate: (opts: { set: Row }) => ({
						returning: async () => {
							const name2 = tableName(table);
							const key =
								name2 === 'organizationMembers' ? `${vals.organizationId}:${vals.userId}` : vals.id;
							const existing = store[name2].get(key);
							if (existing) {
								Object.assign(existing, opts.set);
								return [{ ...existing }];
							}
							return [insertNow()];
						},
					}),
				};
			},
		}),
		update: (table: any) => ({
			set: (patch: Row) => ({
				where: (cond: any) => ({
					returning: async () => {
						const name = tableName(table);
						const matched = rowsFor(name).filter((r) => evalCond(r, cond));
						for (const r of matched) Object.assign(r, patch);
						return matched.map((r) => ({ ...r }));
					},
				}),
			}),
		}),
		delete: (table: any) => ({
			where: async (cond: any) => {
				const name = tableName(table);
				for (const [key, row] of (store[name] as Map<string, Row>).entries()) {
					if (evalCond(row, cond)) store[name].delete(key);
				}
			},
		}),
	};
}

let store: ReturnType<typeof makeStore>;

const dbMock: any = {};

vi.mock('$lib/server/db', () => ({
	db: {
		select: (...args: any[]) => dbMock.select(...args),
		insert: (...args: any[]) => dbMock.insert(...args),
		update: (...args: any[]) => dbMock.update(...args),
		delete: (...args: any[]) => dbMock.delete(...args),
		transaction: async (cb: any) => dbMock.transaction(cb),
	},
}));

function resetDb() {
	store = makeStore();
	const executor = makeExecutor(store);
	Object.assign(dbMock, executor);
	dbMock.transaction = async (cb: any) => cb(makeExecutor(store));
}

const {
	claimInvitation,
	upsertOrganizationMember,
	revokeOrganizationInvitation,
	hashInvitationToken,
	outranks,
} = await import('./organizations');

beforeEach(() => {
	resetDb();
	idCounter = 0;
});

function seedInvitation(overrides: Partial<Row> = {}) {
	const token = overrides.__rawToken ?? 'raw-token-abc';
	const tokenHash = hashInvitationToken(token);
	const row: Row = {
		id: 'inv-1',
		organizationId: 'org-1',
		email: 'invitee@example.com',
		role: 'viewer',
		tokenHash,
		status: 'pending',
		expiresAt: new Date(Date.now() + 60_000),
		createdAt: new Date(),
		acceptedAt: null,
		...overrides,
	};
	delete row.__rawToken;
	store.organizationInvitations.set(row.id, row);
	store.organizations.set('org-1', { id: 'org-1', name: 'Acme', slug: 'acme' });
	return { token, row };
}

describe('claimInvitation (D06, D07, D31)', () => {
	it('the stored tokenHash is never equal to the raw emailed token', () => {
		const { row } = seedInvitation();
		expect(row.tokenHash).not.toBe('raw-token-abc');
		expect(row.tokenHash).toMatch(/^[0-9a-f]{64}$/);
	});

	it('claims a pending, unexpired invitation exactly once and provisions membership', async () => {
		const { token } = seedInvitation();

		const result = await claimInvitation(token, 'user-1');

		expect(result.ok).toBe(true);
		if (result.ok) {
			expect(result.invitation.status).toBe('accepted');
			expect(result.invitation.acceptedAt).toBeInstanceOf(Date);
			expect(result.member.role).toBe('viewer');
		}
		expect(store.organizationMembers.size).toBe(1);
	});

	it('a second redemption of the same token after success returns already_used, with no second membership write', async () => {
		const { token } = seedInvitation();

		await claimInvitation(token, 'user-1');
		const second = await claimInvitation(token, 'user-1');

		expect(second).toEqual({ ok: false, reason: 'already_used' });
		expect(store.organizationMembers.size).toBe(1);
	});

	it('refuses an expired token without provisioning membership', async () => {
		const { token } = seedInvitation({ expiresAt: new Date(Date.now() - 1000) });

		const result = await claimInvitation(token, 'user-1');

		expect(result).toEqual({ ok: false, reason: 'expired' });
		expect(store.organizationMembers.size).toBe(0);
	});

	it('refuses an unknown token', async () => {
		seedInvitation();
		const result = await claimInvitation('a-token-that-was-never-issued', 'user-1');
		expect(result).toEqual({ ok: false, reason: 'not_found' });
	});

	// D07's core acceptance requirement: two concurrent redemptions of the SAME token must result in
	// exactly one membership write. This exercises the real claimInvitation implementation's single
	// conditional UPDATE ... WHERE status='pending' -- the fake db's `update` handler above filters
	// candidate rows through the SAME WHERE-condition evaluator used for every other query in this
	// file, so a regression back to check-then-act (a plain SELECT followed by an unconditional
	// write, with no WHERE guard on the UPDATE) would let both branches pass the filter and this
	// test would start failing.
	it('two simultaneous redemptions of one token result in exactly one membership write', async () => {
		const { token } = seedInvitation();

		const [a, b] = await Promise.all([
			claimInvitation(token, 'user-1'),
			claimInvitation(token, 'user-1'),
		]);

		const outcomes = [a, b];
		const successes = outcomes.filter((r) => r.ok);
		const failures = outcomes.filter((r) => !r.ok);

		expect(successes).toHaveLength(1);
		expect(failures).toHaveLength(1);
		expect((failures[0] as any).reason).toBe('already_used');
		expect(store.organizationMembers.size).toBe(1);
	});

	it('D31: refuses to grant a role that is not on the allowlist, even though it was already stored', async () => {
		seedInvitation({ role: 'superadmin' as any });
		const token = 'raw-token-abc';

		await expect(claimInvitation(token, 'user-1')).rejects.toThrow(/unrecognized role/i);
		expect(store.organizationMembers.size).toBe(0);
	});

	it('D08: an owner accepting a viewer invitation to their own org remains owner', async () => {
		const { token } = seedInvitation({ role: 'viewer' });
		store.organizationMembers.set('org-1:user-1', {
			id: 'mem-1',
			organizationId: 'org-1',
			userId: 'user-1',
			role: 'owner',
		});

		const result = await claimInvitation(token, 'user-1');

		expect(result.ok).toBe(true);
		expect(store.organizationMembers.get('org-1:user-1')?.role).toBe('owner');
	});
});

describe('upsertOrganizationMember (D08)', () => {
	it('never downgrades an existing higher role', async () => {
		await upsertOrganizationMember('org-1', 'user-1', 'owner');
		const member = await upsertOrganizationMember('org-1', 'user-1', 'viewer');

		expect(member.role).toBe('owner');
	});

	it('does upgrade an existing lower role', async () => {
		await upsertOrganizationMember('org-1', 'user-1', 'viewer');
		const member = await upsertOrganizationMember('org-1', 'user-1', 'admin');

		expect(member.role).toBe('admin');
	});

	it('outranks() ranks owner above every other role', () => {
		expect(outranks('owner', 'admin')).toBe(true);
		expect(outranks('viewer', 'owner')).toBe(false);
		expect(outranks('viewer', 'viewer')).toBe(false);
	});
});

describe('revokeOrganizationInvitation (D07)', () => {
	it('revokes a pending invitation', async () => {
		seedInvitation();
		const revoked = await revokeOrganizationInvitation('org-1', 'inv-1');
		expect(revoked?.status).toBe('revoked');
		expect(store.organizationInvitations.get('inv-1')?.status).toBe('revoked');
	});

	it('no-ops on an invitation that is not pending', async () => {
		seedInvitation({ status: 'accepted' });
		const revoked = await revokeOrganizationInvitation('org-1', 'inv-1');
		expect(revoked).toBeUndefined();
	});

	it('a revoked token can no longer be claimed', async () => {
		const { token } = seedInvitation();
		await revokeOrganizationInvitation('org-1', 'inv-1');

		const result = await claimInvitation(token, 'user-1');
		expect(result).toEqual({ ok: false, reason: 'already_used' });
	});
});
