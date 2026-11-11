import { describe, it, expect, vi, beforeEach } from 'vitest';

// A chainable Drizzle-query double, mirroring keys.test.ts's approach: requireOrgMembership
// (../keys/_shared.ts, reused unchanged by the agents routes) reads through this.
function makeDbMock() {
	const dbMock: any = {
		select: vi.fn(),
		from: vi.fn(),
		where: vi.fn(),
		then: vi.fn(),
	};
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.then.mockReset();
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
	return dbMock;
}

const dbMock = makeDbMock();

vi.mock('$lib/server/db', () => ({ db: dbMock }));
vi.mock('$lib/db/schema', () => ({
	organizationMembers: { organizationId: 'organizationId', userId: 'userId', role: 'role' },
	projects: { id: 'id', organizationId: 'organizationId', name: 'name' },
}));

const agentQueries = {
	listAgents: vi.fn(),
	createAgent: vi.fn(),
	getAgentById: vi.fn(),
	setAgentStatus: vi.fn(),
};
vi.mock('$lib/db/queries/agents', () => agentQueries);

// sentinel-cli-browser-login: the roster route also lists members' personal CLI keys.
const listPersonalKeysForOrg = vi.fn();
vi.mock('$lib/server/cli-personal-keys', () => ({ listPersonalKeysForOrg }));

const { GET, POST } = await import('./[orgId=uuid]/agents/+server');
const { PATCH } = await import('./[orgId=uuid]/agents/[agentId=uuid]/+server');

function locals(session: { id: string } | null) {
	return { auth: async () => (session ? { user: { id: session.id } } : null) } as any;
}

function membershipRow(role: string) {
	return { role };
}

describe('organization agent management routes', () => {
	beforeEach(() => {
		vi.clearAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.then.mockReset();
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
	});

	describe('GET /agents', () => {
		it('401s when there is no session', async () => {
			await expect(GET({ params: { orgId: 'org-1' }, locals: locals(null) } as any)).rejects.toMatchObject({
				status: 401,
			});
		});

		it('403s when the caller is not a member of the organization', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([]));
			await expect(
				GET({ params: { orgId: 'org-1' }, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 403 });
		});

		it.each(['engineer', 'support', 'viewer'])('403s for org %s (not owner/admin)', async (role) => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow(role)]));
			await expect(
				GET({ params: { orgId: 'org-1' }, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 403 });
			expect(agentQueries.listAgents).not.toHaveBeenCalled();
		});

		it.each(['owner', 'admin'])('200s and lists agents for org %s', async (role) => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow(role)]));
			agentQueries.listAgents.mockResolvedValueOnce([{ id: 'agent-1', name: 'AutoFix' }]);
			listPersonalKeysForOrg.mockResolvedValueOnce([]);

			const res = await GET({ params: { orgId: 'org-1' }, locals: locals({ id: 'user-1' }) } as any);
			const body = await res.json();
			expect(body).toEqual({ agents: [{ id: 'agent-1', name: 'AutoFix' }], personalKeys: [] });
			expect(agentQueries.listAgents).toHaveBeenCalledWith('org-1');
		});

		it('keeps personal agents out of the roster and lists their keys with the owner, never a secret', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('admin')]));
			agentQueries.listAgents.mockResolvedValueOnce([
				{ id: 'agent-1', name: 'AutoFix', kind: 'ai' },
				{ id: 'agent-2', name: 'Ada (CLI)', kind: 'personal', ownerUserId: 'user-ada' },
			]);
			listPersonalKeysForOrg.mockResolvedValueOnce([
				{
					keyId: 'key-1',
					deviceName: 'laptop',
					keyPrefix: 'sent_agent_',
					status: 'active',
					createdAt: null,
					expiresAt: null,
					revokedAt: null,
					agentId: 'agent-2',
					organizationId: 'org-1',
					organizationName: 'Org',
					organizationSlug: 'org',
					ownerUserId: 'user-ada',
					ownerName: 'Ada',
					ownerEmail: 'ada@example.test',
				},
			]);

			const body = await (await GET({ params: { orgId: 'org-1' }, locals: locals({ id: 'user-1' }) } as any)).json();
			expect(body.agents.map((a: { id: string }) => a.id)).toEqual(['agent-1']);
			expect(body.personalKeys).toEqual([
				{
					keyId: 'key-1',
					deviceName: 'laptop',
					keyPrefix: 'sent_agent_',
					status: 'active',
					createdAt: null,
					expiresAt: null,
					revokedAt: null,
					owner: { userId: 'user-ada', name: 'Ada', email: 'ada@example.test' },
				},
			]);
		});

		it('PATCH refuses a personal agent (status / repo-credential flag are never set by an admin)', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			agentQueries.getAgentById.mockResolvedValueOnce({ id: 'agent-2', orgId: 'org-1', kind: 'personal' });
			const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ canAccessRepoCredentials: true }) });
			await expect(
				PATCH({ params: { orgId: 'org-1', agentId: 'agent-2' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 400 });
			expect(agentQueries.setAgentStatus).not.toHaveBeenCalled();
		});
	});

	describe('POST /agents (create)', () => {
		it('403s for an engineer (only owner/admin may manage agents)', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('engineer')]));
			const request = new Request('http://x', {
				method: 'POST',
				body: JSON.stringify({ name: 'AutoFix', kind: 'ai' }),
			});
			await expect(
				POST({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 403 });
			expect(agentQueries.createAgent).not.toHaveBeenCalled();
		});

		it('400s when name is missing', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			const request = new Request('http://x', { method: 'POST', body: JSON.stringify({ kind: 'ai' }) });
			await expect(
				POST({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 400 });
			expect(agentQueries.createAgent).not.toHaveBeenCalled();
		});

		it('400s for an invalid kind', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			const request = new Request('http://x', {
				method: 'POST',
				body: JSON.stringify({ name: 'AutoFix', kind: 'human' }),
			});
			await expect(
				POST({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 400 });
			expect(agentQueries.createAgent).not.toHaveBeenCalled();
		});

		it('201s for an admin with a valid body', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('admin')]));
			agentQueries.createAgent.mockResolvedValueOnce({ id: 'agent-1', name: 'AutoFix', kind: 'ai', status: 'active' });
			const request = new Request('http://x', {
				method: 'POST',
				body: JSON.stringify({ name: '  AutoFix  ', kind: 'ai' }),
			});

			const res = await POST({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-1' }) } as any);
			expect(res.status).toBe(201);
			expect(agentQueries.createAgent).toHaveBeenCalledWith('user-1', { orgId: 'org-1', name: 'AutoFix', kind: 'ai' });
		});
	});

	describe('PATCH /agents/[agentId] (status)', () => {
		it('404s when the agent belongs to a different organization', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			agentQueries.getAgentById.mockResolvedValueOnce({ id: 'agent-1', orgId: 'org-2' });
			const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ status: 'disabled' }) });

			await expect(
				PATCH({ params: { orgId: 'org-1', agentId: 'agent-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 404 });
			expect(agentQueries.setAgentStatus).not.toHaveBeenCalled();
		});

		it('400s for an invalid status value', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			agentQueries.getAgentById.mockResolvedValueOnce({ id: 'agent-1', orgId: 'org-1' });
			const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ status: 'deleted' }) });

			await expect(
				PATCH({ params: { orgId: 'org-1', agentId: 'agent-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 400 });
			expect(agentQueries.setAgentStatus).not.toHaveBeenCalled();
		});

		it('403s for a support member', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('support')]));
			const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ status: 'disabled' }) });

			await expect(
				PATCH({ params: { orgId: 'org-1', agentId: 'agent-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 403 });
			expect(agentQueries.getAgentById).not.toHaveBeenCalled();
		});

		it('200s and disables an agent for an owner', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			agentQueries.getAgentById.mockResolvedValueOnce({ id: 'agent-1', orgId: 'org-1' });
			agentQueries.setAgentStatus.mockResolvedValueOnce({ id: 'agent-1', status: 'disabled' });
			const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ status: 'disabled' }) });

			const res = await PATCH({ params: { orgId: 'org-1', agentId: 'agent-1' }, request, locals: locals({ id: 'user-1' }) } as any);
			const body = await res.json();
			expect(body).toEqual({ agent: { id: 'agent-1', status: 'disabled' } });
			expect(agentQueries.setAgentStatus).toHaveBeenCalledWith('user-1', 'org-1', 'agent-1', 'disabled');
		});
	});
});
