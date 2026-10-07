import { describe, it, expect, vi, beforeEach } from 'vitest';

const authenticateAgentRequest = vi.fn();
vi.mock('$lib/server/agent-auth', () => ({ authenticateAgentRequest }));

const writeAgentAuditLog = vi.fn().mockResolvedValue(undefined);
vi.mock('$lib/server/agent-audit', () => ({ writeAgentAuditLog }));

const listOrganizationMembers = vi.fn();
const createOrganizationInvitation = vi.fn();
vi.mock('$lib/db/queries/organizations', () => ({
	listOrganizationMembers,
	createOrganizationInvitation,
	ORG_ROLES: ['owner', 'admin', 'engineer', 'support', 'viewer'],
}));

const sendInvitationEmail = vi.fn().mockResolvedValue(true);
vi.mock('$lib/server/email', () => ({ sendInvitationEmail }));

const dbMock: any = {
	select: vi.fn(),
	from: vi.fn(),
	where: vi.fn(),
	innerJoin: vi.fn(),
	limit: vi.fn(),
	for: vi.fn(),
	delete: vi.fn(),
	then: vi.fn(),
	transaction: vi.fn(async (cb: (tx: any) => unknown) => cb(dbMock)),
};
dbMock.select.mockReturnValue(dbMock);
dbMock.from.mockReturnValue(dbMock);
dbMock.where.mockReturnValue(dbMock);
dbMock.innerJoin.mockReturnValue(dbMock);
dbMock.limit.mockReturnValue(dbMock);
dbMock.for.mockReturnValue(dbMock);
dbMock.delete.mockReturnValue(dbMock);
dbMock.then.mockImplementation((resolve: any) => resolve([]));
vi.mock('$lib/server/db', () => ({ db: dbMock }));
// sentinel-cli-browser-login: member removal also revokes the member's personal CLI keys (cli-personal-keys.ts).
const revokePersonalKeysOfMember = vi.fn().mockResolvedValue([]);
vi.mock('$lib/server/cli-personal-keys', () => ({ revokePersonalKeysOfMember }));

vi.mock('$lib/db/schema', () => ({
	organizationMembers: { id: 'id', organizationId: 'organizationId', userId: 'userId', role: 'role' },
	users: { id: 'id', email: 'email', name: 'name' },
	organizations: { id: 'id', name: 'name', slug: 'slug' },
	organizationInvitations: { id: 'id', organizationId: 'organizationId', email: 'email' },
	issues: { id: 'id', projectId: 'projectId' },
	projects: { id: 'id', organizationId: 'organizationId' },
	issueSubscriptions: { id: 'id', issueId: 'issueId', subscriberType: 'subscriberType', subscriberId: 'subscriberId' },
	projectEnvironmentGrants: { userId: 'userId', projectId: 'projectId', environments: 'environments' },
}));

const { GET, POST } = await import('./+server');
const { DELETE } = await import('./[id]/+server');

const CTX = {
	agentId: 'agent-1',
	organizationId: 'org-1',
	agentName: 'bot',
	keyPrefixForAudit: 'abc123def456',
};

function makeEvent(path: string, options: RequestInit = {}) {
	const url = new URL(`http://localhost${path}`);
	return { request: new Request(url, options), url } as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	authenticateAgentRequest.mockResolvedValue(CTX);
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.innerJoin.mockReturnValue(dbMock);
	dbMock.limit.mockReturnValue(dbMock);
	dbMock.for.mockReturnValue(dbMock);
	dbMock.delete.mockReturnValue(dbMock);
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
});

describe('GET /api/agent/members', () => {
	it('401s when unauthenticated', async () => {
		authenticateAgentRequest.mockRejectedValue(Object.assign(new Error('Unauthorized'), { status: 401 }));
		await expect(GET(makeEvent('/api/agent/members'))).rejects.toMatchObject({ status: 401 });
	});

	it('returns organization members', async () => {
		listOrganizationMembers.mockResolvedValue([
			{ id: 'm1', userId: 'u1', role: 'owner', user: { name: 'Owner', email: 'owner@example.com' } },
			{ id: 'm2', userId: 'u2', role: 'engineer', user: { name: 'Eng', email: 'eng@example.com' } },
		]);

		const res = await GET(makeEvent('/api/agent/members'));
		expect(res.status).toBe(200);
		const body = await res.json();
		expect(listOrganizationMembers).toHaveBeenCalledWith('org-1');
		expect(body.members).toHaveLength(2);
	});

	it('filters by role when provided', async () => {
		listOrganizationMembers.mockResolvedValue([
			{ id: 'm1', userId: 'u1', role: 'owner', user: { name: 'Owner', email: 'owner@example.com' } },
			{ id: 'm2', userId: 'u2', role: 'engineer', user: { name: 'Eng', email: 'eng@example.com' } },
		]);

		const res = await GET(makeEvent('/api/agent/members?role=engineer'));
		expect(res.status).toBe(200);
		const body = await res.json();
		expect(body.members).toHaveLength(1);
		expect(body.members[0].role).toBe('engineer');
	});
});

describe('POST /api/agent/members', () => {
	it('400s on missing fields or invalid role', async () => {
		await expect(
			POST(makeEvent('/api/agent/members', { method: 'POST', body: JSON.stringify({}) }))
		).rejects.toMatchObject({ status: 400 });

		await expect(
			POST(
				makeEvent('/api/agent/members', {
					method: 'POST',
					body: JSON.stringify({ email: 'test@example.com', role: 'invalid_role' }),
				})
			)
		).rejects.toMatchObject({ status: 400 });
	});

	it('invites a new member successfully and audits', async () => {
		createOrganizationInvitation.mockResolvedValue({
			id: 'inv-1',
			email: 'new@example.com',
			role: 'engineer',
			status: 'pending',
			expiresAt: new Date('2026-10-01T00:00:00Z'),
		});

		const res = await POST(
			makeEvent('/api/agent/members', {
				method: 'POST',
				body: JSON.stringify({ email: 'new@example.com', role: 'engineer' }),
			})
		);
		expect(res.status).toBe(201);
		const body = await res.json();
		expect(body.id).toBe('inv-1');
		expect(body.email).toBe('new@example.com');
		expect(body.role).toBe('engineer');
		expect(body.delivered).toBe(true);

		expect(createOrganizationInvitation).toHaveBeenCalledWith(
			'org-1',
			'new@example.com',
			'engineer',
			expect.any(String),
			expect.any(Date),
			'agent-1'
		);
		expect(writeAgentAuditLog).toHaveBeenCalledWith(
			CTX,
			'agent.member_invited',
			'invitation',
			'inv-1',
			expect.objectContaining({ email: 'new@example.com', role: 'engineer' })
		);
	});
});

describe('DELETE /api/agent/members/[id]', () => {
	it('404s when member does not exist', async () => {
		dbMock.then.mockImplementationOnce((resolve: any) => resolve([])); // byId
		dbMock.then.mockImplementationOnce((resolve: any) => resolve([])); // byUserId

		await expect(
			DELETE({
				params: { id: 'nonexistent' },
				request: new Request('http://localhost/api/agent/members/nonexistent', { method: 'DELETE' }),
			} as any)
		).rejects.toMatchObject({ status: 404 });
	});

	it('400s when trying to delete the sole owner', async () => {
		// findOrgMember returns an owner
		dbMock.then.mockImplementationOnce((resolve: any) => resolve([{ id: 'm1', userId: 'u1', role: 'owner' }]));
		// countLockedOwners returns 1
		dbMock.then.mockImplementationOnce((resolve: any) => resolve([{ id: 'm1' }]));

		await expect(
			DELETE({
				params: { id: 'm1' },
				request: new Request('http://localhost/api/agent/members/m1', { method: 'DELETE' }),
			} as any)
		).rejects.toMatchObject({ status: 400 });
	});

	it('deletes member and cleans up subscriptions/grants', async () => {
		// findOrgMember returns engineer
		dbMock.then.mockImplementationOnce((resolve: any) => resolve([{ id: 'm2', userId: 'u2', role: 'engineer' }]));
		// delete member
		dbMock.then.mockImplementationOnce((resolve: any) => resolve([]));
		// select org issues
		dbMock.then.mockImplementationOnce((resolve: any) => resolve([{ id: 'iss-1' }]));
		// delete subscriptions
		dbMock.then.mockImplementationOnce((resolve: any) => resolve([]));
		// delete grants
		dbMock.then.mockImplementationOnce((resolve: any) => resolve([]));

		const res = await DELETE({
			params: { id: 'm2' },
			request: new Request('http://localhost/api/agent/members/m2', { method: 'DELETE' }),
		} as any);

		expect(res.status).toBe(200);
		const body = await res.json();
		expect(body).toEqual({ success: true, memberId: 'm2', userId: 'u2' });
		// sentinel-cli-browser-login: and the member's personal CLI keys for this org, in the same transaction.
		expect(revokePersonalKeysOfMember).toHaveBeenCalledWith(expect.anything(), {
			organizationId: CTX.organizationId,
			userId: 'u2',
			actorId: CTX.agentId,
		});
		expect(writeAgentAuditLog).toHaveBeenCalledWith(
			CTX,
			'agent.member_removed',
			'member',
			'm2',
			expect.objectContaining({ userId: 'u2', role: 'engineer' })
		);
	});
});
