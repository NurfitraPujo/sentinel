import { describe, it, expect, vi, beforeEach } from 'vitest';

// The scope predicate is the security boundary this migration exists for, so it is asserted
// directly: which rows the delivery query is willing to decrypt, for an agent with and without
// grants. Everything else about the query (decryption, last_fetched_at) is covered elsewhere.

const captured: { where: unknown[] } = { where: [] };

const grantRows: { projectId: string }[] = [];
const credentialRows: any[] = [];

const dbMock: any = {
	select: vi.fn(),
	from: vi.fn(),
	where: vi.fn(),
	orderBy: vi.fn(),
	update: vi.fn(),
	set: vi.fn(),
};
vi.mock('$lib/server/db', () => ({ db: dbMock }));
vi.mock('$lib/db/schema', () => ({
	repoCredentials: {
		id: 'id',
		organizationId: 'organizationId',
		projectId: 'projectId',
		status: 'status',
		createdAt: 'createdAt',
	},
	auditLogs: {},
	agentRepoCredentialGrants: { agentId: 'agentId', projectId: 'projectId' },
}));
vi.mock('$lib/server/repo-credential-crypto', () => ({
	decryptRepoCredentialSecret: () => ({ token: 'ghp_x' }),
	encryptRepoCredentialSecret: () => ({ encryptedSecret: '', nonce: '', keyVersion: 1 }),
	isEncryptionKeyAvailable: () => true,
	EncryptionKeyUnavailableError: class extends Error {},
}));

// `or(...)` / `isNull(...)` are recorded rather than executed, so the assertion is about which
// predicate shape was built — an org-wide-only clause, or org-wide OR the granted projects.
vi.mock('drizzle-orm', () => ({
	eq: (a: unknown, b: unknown) => ({ op: 'eq', a, b }),
	and: (...parts: unknown[]) => ({ op: 'and', parts }),
	or: (...parts: unknown[]) => ({ op: 'or', parts }),
	isNull: (col: unknown) => ({ op: 'isNull', col }),
	inArray: (col: unknown, values: unknown) => ({ op: 'inArray', col, values }),
	desc: (col: unknown) => col,
}));

const { fetchDecryptedCredentialsForAgent } = await import('./repo-credentials');

beforeEach(() => {
	vi.clearAllMocks();
	captured.where = [];
	grantRows.length = 0;
	credentialRows.length = 0;

	let call = 0;
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.orderBy.mockImplementation(() => Promise.resolve(credentialRows));
	dbMock.update.mockReturnValue(dbMock);
	dbMock.set.mockReturnValue(dbMock);
	dbMock.where.mockImplementation((predicate: unknown) => {
		captured.where.push(predicate);
		call += 1;
		// 1st: the grant lookup. 2nd: the credential query, which then chains .orderBy.
		if (call === 1) return Promise.resolve(grantRows);
		return dbMock;
	});
});

function scopeClause(): any {
	// where[1] is the credential query's `and(org, status, scope)`.
	return (captured.where[1] as any).parts[2];
}

describe('fetchDecryptedCredentialsForAgent scope', () => {
	it('an agent with no grants sees org-wide credentials only', async () => {
		await fetchDecryptedCredentialsForAgent('org-1', 'agent-1');

		expect(scopeClause()).toEqual({ op: 'isNull', col: 'projectId' });
	});

	it('an agent with grants sees org-wide OR its granted projects', async () => {
		grantRows.push({ projectId: 'proj-1' }, { projectId: 'proj-2' });

		await fetchDecryptedCredentialsForAgent('org-1', 'agent-1');

		expect(scopeClause()).toEqual({
			op: 'or',
			parts: [
				{ op: 'isNull', col: 'projectId' },
				{ op: 'inArray', col: 'projectId', values: ['proj-1', 'proj-2'] },
			],
		});
	});

	it('reads the grant set for the AUTHENTICATED agent', async () => {
		await fetchDecryptedCredentialsForAgent('org-1', 'agent-7');

		expect(captured.where[0]).toEqual({ op: 'eq', a: 'agentId', b: 'agent-7' });
	});

	it('still scopes by organization and active status', async () => {
		const clause = async () => {
			await fetchDecryptedCredentialsForAgent('org-1', 'agent-1');
			return (captured.where[1] as any).parts;
		};

		const parts = await clause();
		expect(parts[0]).toEqual({ op: 'eq', a: 'organizationId', b: 'org-1' });
		expect(parts[1]).toEqual({ op: 'eq', a: 'status', b: 'active' });
	});
});
