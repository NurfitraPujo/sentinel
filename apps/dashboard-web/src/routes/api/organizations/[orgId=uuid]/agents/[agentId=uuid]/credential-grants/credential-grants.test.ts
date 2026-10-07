import { describe, it, expect, vi, beforeEach } from 'vitest';

const requireOrgMembership = vi.fn();
vi.mock('../../../keys/_shared', () => ({
	requireOrgMembership: (...a: any[]) => requireOrgMembership(...a),
}));

const listAgentCredentialGrants = vi.fn();
const replaceAgentCredentialGrants = vi.fn();
vi.mock('$lib/db/queries/repo-credentials', () => ({
	listAgentCredentialGrants: (...a: any[]) => listAgentCredentialGrants(...a),
	replaceAgentCredentialGrants: (...a: any[]) => replaceAgentCredentialGrants(...a),
}));

const whereResults: any[][] = [];
const dbMock: any = { select: vi.fn(), from: vi.fn(), where: vi.fn() };
dbMock.select.mockReturnValue(dbMock);
dbMock.from.mockReturnValue(dbMock);
dbMock.where.mockImplementation(() => Promise.resolve(whereResults.shift() ?? []));
vi.mock('$lib/server/db', () => ({ db: dbMock }));
vi.mock('$lib/db/schema', () => ({
	agents: { id: 'id', orgId: 'orgId' },
	projects: { id: 'id', organizationId: 'organizationId' },
}));

const { GET, PUT } = await import('./+server');

function event(role: string | null, body?: unknown) {
	return {
		params: { orgId: 'org-1', agentId: 'agent-1' },
		locals: { auth: vi.fn().mockResolvedValue(role ? { user: { id: 'user-1' } } : null) },
		request: new Request('https://sentinel.test/x', { method: 'PUT', body: JSON.stringify(body ?? {}) }),
	} as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	whereResults.length = 0;
	requireOrgMembership.mockResolvedValue({ role: 'owner' });
	listAgentCredentialGrants.mockResolvedValue(['proj-1']);
	replaceAgentCredentialGrants.mockImplementation(async (_a: string, _b: string, ids: string[]) => ids);
});

describe('agent credential grants', () => {
	it('lists an agent\'s granted projects', async () => {
		whereResults.push([{ id: 'agent-1' }]);
		const res = await GET(event('owner'));
		expect(await res.json()).toEqual({ projectIds: ['proj-1'] });
	});

	it('replaces grants for an owner', async () => {
		whereResults.push([{ id: 'agent-1' }], [{ id: 'proj-1' }, { id: 'proj-2' }]);
		const res = await PUT(event('owner', { projectIds: ['proj-1', 'proj-2'] }));
		expect(res.status).toBe(200);
		expect(replaceAgentCredentialGrants).toHaveBeenCalledWith('user-1', 'agent-1', ['proj-1', 'proj-2']);
	});

	it('401s an anonymous caller', async () => {
		await expect(PUT(event(null, { projectIds: [] }))).rejects.toMatchObject({ status: 401 });
		expect(replaceAgentCredentialGrants).not.toHaveBeenCalled();
	});

	// Deciding who receives write-capable git tokens is owner/admin only, not engineer.
	it('403s an engineer, who lacks manage_agents', async () => {
		requireOrgMembership.mockResolvedValue({ role: 'engineer' });
		await expect(PUT(event('engineer', { projectIds: [] }))).rejects.toMatchObject({ status: 403 });
		expect(replaceAgentCredentialGrants).not.toHaveBeenCalled();
	});

	// The org in the URL is not proof the agent belongs to it.
	it('404s an agent that belongs to another organization', async () => {
		whereResults.push([]);
		await expect(PUT(event('owner', { projectIds: [] }))).rejects.toMatchObject({ status: 404 });
		expect(replaceAgentCredentialGrants).not.toHaveBeenCalled();
	});

	// The cross-tenant grant this endpoint exists to prevent: a project id from another org would
	// otherwise let this agent fetch that org's project-scoped credentials.
	it('400s when a projectId does not belong to the organization', async () => {
		whereResults.push([{ id: 'agent-1' }], [{ id: 'proj-1' }]);
		await expect(
			PUT(event('owner', { projectIds: ['proj-1', 'proj-from-another-org'] }))
		).rejects.toMatchObject({ status: 400 });
		expect(replaceAgentCredentialGrants).not.toHaveBeenCalled();
	});

	it('400s a malformed body', async () => {
		whereResults.push([{ id: 'agent-1' }]);
		await expect(PUT(event('owner', { projectIds: 'proj-1' }))).rejects.toMatchObject({ status: 400 });
	});
});
