import { describe, it, expect, vi, beforeEach } from 'vitest';

// N1c (agent read endpoints). GET /api/agent/projects -- org isolation (organizationId always
// comes from AgentAuthContext, per B7) and response shape.

const listAgentProjects = vi.fn();
vi.mock('$lib/db/queries/agent-reads', () => ({ listAgentProjects }));

const authenticateAgentRequest = vi.fn();
vi.mock('$lib/server/agent-auth', () => ({ authenticateAgentRequest }));

const { GET } = await import('./+server');

function makeEvent() {
	return { request: new Request('http://localhost/api/agent/projects') } as any;
}

beforeEach(() => {
	vi.clearAllMocks();
});

describe('GET /api/agent/projects', () => {
	it('401s when unauthenticated', async () => {
		authenticateAgentRequest.mockRejectedValue(Object.assign(new Error('Unauthorized'), { status: 401 }));
		await expect(GET(makeEvent())).rejects.toMatchObject({ status: 401 });
		expect(listAgentProjects).not.toHaveBeenCalled();
	});

	it('lists only the calling key organization projects', async () => {
		authenticateAgentRequest.mockResolvedValue({
			agentId: 'agent-1',
			organizationId: 'org-1',
			agentName: 'bot',
			keyPrefixForAudit: 'abc',
		});
		listAgentProjects.mockResolvedValue([{ id: 'p1', name: 'Web', isInbox: false }]);

		const res = await GET(makeEvent());
		expect(res.status).toBe(200);
		const body = await res.json();

		expect(listAgentProjects).toHaveBeenCalledWith('org-1');
		expect(body.projects).toEqual([{ id: 'p1', name: 'Web', isInbox: false }]);
	});
});
