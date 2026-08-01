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
		invitedBy: 'invitedBy',
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

// Deep-copies every row so a restore doesn't share references with rows a transaction may have
// mutated in place (the fake's `update` handler does `Object.assign(row, patch)`).
function snapshotStore(s: typeof store) {
	return {
		organizations: new Map(Array.from(s.organizations, ([k, v]) => [k, { ...v }])),
		organizationMembers: new Map(Array.from(s.organizationMembers, ([k, v]) => [k, { ...v }])),
		organizationInvitations: new Map(Array.from(s.organizationInvitations, ([k, v]) => [k, { ...v }])),
	};
}

function resetDb() {
	store = makeStore();
	const executor = makeExecutor(store);
	Object.assign(dbMock, executor);
	// D31 (residual): claimInvitation throws INSIDE db.transaction to roll back its own earlier
	// UPDATE when the inviter's authority check fails, so the invitation stays 'pending' rather than
	// being burned on a refused redemption -- without this, the fake would apply every write a
	// callback made regardless of whether it threw, and a test asserting "still pending after
	// refusal" would pass even against a claimInvitation that never rolled back anything for real.
	dbMock.transaction = async (cb: any) => {
		const snapshot = snapshotStore(store);
		try {
			return await cb(makeExecutor(store));
		} catch (err) {
			store.organizations = snapshot.organizations;
			store.organizationMembers = snapshot.organizationMembers;
			store.organizationInvitations = snapshot.organizationInvitations;
			throw err;
		}
	};
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

// D31 (residual): defaults `invitedBy` to a seeded, currently-authorized owner ('inviter-1') so
// every EXISTING test below -- written before the inviter-authority check existed -- keeps passing
// unchanged. Tests that specifically exercise the new check pass `invitedBy: null` or seed a
// DIFFERENT inviter membership (or none) to defeat it deliberately.
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
		invitedBy: 'inviter-1',
		...overrides,
	};
	delete row.__rawToken;
	store.organizationInvitations.set(row.id, row);
	store.organizations.set('org-1', { id: 'org-1', name: 'Acme', slug: 'acme' });
	if (row.invitedBy && !store.organizationMembers.has(`org-1:${row.invitedBy}`)) {
		store.organizationMembers.set(`org-1:${row.invitedBy}`, {
			id: `mem-${row.invitedBy}`,
			organizationId: 'org-1',
			userId: row.invitedBy,
			role: 'owner',
		});
	}
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
		// Not .size === 1: seedInvitation now also seeds the inviter's own membership row by
		// default (D31 residual), so asserting the SPECIFIC accepting-user row is what this test
		// actually means to check.
		expect(store.organizationMembers.get('org-1:user-1')?.role).toBe('viewer');
	});

	it('a second redemption of the same token after success returns already_used, with no second membership write', async () => {
		const { token } = seedInvitation();

		await claimInvitation(token, 'user-1');
		const second = await claimInvitation(token, 'user-1');

		expect(second).toEqual({ ok: false, reason: 'already_used' });
		expect(store.organizationMembers.get('org-1:user-1')?.role).toBe('viewer');
	});

	it('refuses an expired token without provisioning membership', async () => {
		const { token } = seedInvitation({ expiresAt: new Date(Date.now() - 1000) });

		const result = await claimInvitation(token, 'user-1');

		expect(result).toEqual({ ok: false, reason: 'expired' });
		expect(store.organizationMembers.has('org-1:user-1')).toBe(false);
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
		expect(store.organizationMembers.get('org-1:user-1')?.role).toBe('viewer');
	});

	it('D31: refuses to grant a role that is not on the allowlist, even though it was already stored', async () => {
		seedInvitation({ role: 'superadmin' as any });
		const token = 'raw-token-abc';

		await expect(claimInvitation(token, 'user-1')).rejects.toThrow(/unrecognized role/i);
		expect(store.organizationMembers.has('org-1:user-1')).toBe(false);
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

	// D31 residual: a pending invitation's role was validated against the allowlist at redemption,
	// but nothing checked that the INVITER still had authority to grant it. These four tests cover
	// the cases that check closes, plus that a refusal rolls back cleanly rather than burning the
	// token.
	describe('inviter authority re-checked at redemption (D31 residual)', () => {
		it('refuses when the invitation has no recorded inviter (invitedBy is null)', async () => {
			const { token } = seedInvitation({ invitedBy: null });

			const result = await claimInvitation(token, 'user-1');

			expect(result).toEqual({ ok: false, reason: 'inviter_no_longer_authorized' });
			expect(store.organizationMembers.has('org-1:user-1')).toBe(false);
		});

		it('refuses when the inviter is no longer a member of the organization', async () => {
			// seedInvitation's default auto-seeds 'inviter-1' as owner; remove that membership to
			// simulate the inviter having been removed from the org after sending the invite.
			const { token } = seedInvitation();
			store.organizationMembers.delete('org-1:inviter-1');

			const result = await claimInvitation(token, 'user-1');

			expect(result).toEqual({ ok: false, reason: 'inviter_no_longer_authorized' });
			expect(store.organizationMembers.has('org-1:user-1')).toBe(false);
		});

		it('refuses when the inviter has been demoted below the authority to grant the invited role', async () => {
			// An 'owner' invite, but the inviter (who WAS owner when they sent it) has since been
			// demoted to 'admin' -- an admin cannot grant owner (mirrors the creation-time rule in
			// routes/api/organizations/[orgId]/invitations/+server.ts).
			const { token } = seedInvitation({ role: 'owner' });
			store.organizationMembers.set('org-1:inviter-1', {
				id: 'mem-inviter-1',
				organizationId: 'org-1',
				userId: 'inviter-1',
				role: 'admin',
			});

			const result = await claimInvitation(token, 'user-1');

			expect(result).toEqual({ ok: false, reason: 'inviter_no_longer_authorized' });
		});

		it('refuses when the inviter has been demoted to a role that cannot invite at all', async () => {
			const { token } = seedInvitation();
			store.organizationMembers.set('org-1:inviter-1', {
				id: 'mem-inviter-1',
				organizationId: 'org-1',
				userId: 'inviter-1',
				role: 'viewer',
			});

			const result = await claimInvitation(token, 'user-1');

			expect(result).toEqual({ ok: false, reason: 'inviter_no_longer_authorized' });
		});

		it('succeeds when the inviter is demoted but their CURRENT role still authorizes the grant', async () => {
			// Invited as 'engineer' by an owner; owner is later demoted to 'admin' -- admin can still
			// grant non-owner roles, so this must succeed. Proves the check is not simply "any change
			// is refused" but specifically "does the CURRENT role still authorize THIS grant".
			const { token } = seedInvitation({ role: 'engineer' });
			store.organizationMembers.set('org-1:inviter-1', {
				id: 'mem-inviter-1',
				organizationId: 'org-1',
				userId: 'inviter-1',
				role: 'admin',
			});

			const result = await claimInvitation(token, 'user-1');

			expect(result.ok).toBe(true);
			expect(store.organizationMembers.get('org-1:user-1')?.role).toBe('engineer');
		});

		// The regression fence for the transaction-rollback design: a refusal must not consume the
		// token. If claimInvitation returned early from inside db.transaction instead of throwing,
		// the fake's rollback (added specifically to catch this) would not fire, and this row would
		// incorrectly show 'accepted' here.
		it('leaves the invitation status as pending (not accepted) after a refused redemption, so it is not burned', async () => {
			const { token, row } = seedInvitation({ invitedBy: null });

			const result = await claimInvitation(token, 'user-1');

			expect(result.ok).toBe(false);
			expect(store.organizationInvitations.get(row.id)?.status).toBe('pending');
			expect(store.organizationInvitations.get(row.id)?.acceptedAt).toBeNull();

			// And because it is still pending, restoring the inviter's authority lets the SAME token
			// succeed on a later attempt -- confirming the token was never consumed.
			store.organizationMembers.set('org-1:inviter-1', {
				id: 'mem-inviter-1',
				organizationId: 'org-1',
				userId: 'inviter-1',
				role: 'owner',
			});
			store.organizationInvitations.get(row.id)!.invitedBy = 'inviter-1';
			const retry = await claimInvitation(token, 'user-1');
			expect(retry.ok).toBe(true);
		});
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
