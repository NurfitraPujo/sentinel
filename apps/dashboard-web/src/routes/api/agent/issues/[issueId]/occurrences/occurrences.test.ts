import { describe, it, expect, vi, beforeEach } from 'vitest';

// N1c (agent read endpoints). GET /api/agent/issues/[issueId]/occurrences -- paging + clamp.

const listAgentOccurrences = vi.fn();
vi.mock('$lib/db/queries/agent-reads', () => ({ listAgentOccurrences }));

const authenticateAgentRequest = vi.fn();
const resolveAgentIssueScope = vi.fn();
const writeAgentAuditLog = vi.fn();

vi.mock('$lib/server/agent-auth', () => ({ authenticateAgentRequest }));
vi.mock('$lib/server/agent-issue-scope', () => ({ resolveAgentIssueScope }));
vi.mock('$lib/server/agent-audit', () => ({ writeAgentAuditLog }));

const { GET } = await import('./+server');

function makeEvent(query = '') {
	const url = new URL(`http://localhost/api/agent/issues/issue-1/occurrences${query}`);
	return {
		params: { issueId: 'issue-1' },
		request: new Request(url),
		url,
	} as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	authenticateAgentRequest.mockResolvedValue({
		agentId: 'agent-1',
		organizationId: 'org-1',
		agentName: 'bot',
		keyPrefixForAudit: 'abc',
	});
	resolveAgentIssueScope.mockResolvedValue({
		issueId: 'issue-1',
		projectId: 'project-1',
		organizationId: 'org-1',
		issueType: 'system_error',
		assignedTo: null,
		assigneeType: null,
		waitingOn: null,
	});
	listAgentOccurrences.mockResolvedValue([]);
});

describe('GET /api/agent/issues/[issueId]/occurrences', () => {
	it('401s when unauthenticated', async () => {
		authenticateAgentRequest.mockRejectedValue(Object.assign(new Error('Unauthorized'), { status: 401 }));
		await expect(GET(makeEvent())).rejects.toMatchObject({ status: 401 });
	});

	it('404s for a cross-org issue', async () => {
		resolveAgentIssueScope.mockRejectedValue(Object.assign(new Error('Issue not found'), { status: 404 }));
		await expect(GET(makeEvent())).rejects.toMatchObject({ status: 404 });
	});

	it('defaults limit and passes no before cursor', async () => {
		await GET(makeEvent());
		expect(listAgentOccurrences).toHaveBeenCalledWith({ issueId: 'issue-1', limit: undefined, before: undefined });
	});

	it('passes through a valid limit and before cursor', async () => {
		await GET(makeEvent('?limit=5&before=2026-01-01T00:00:00.000Z'));
		expect(listAgentOccurrences).toHaveBeenCalledWith({
			issueId: 'issue-1',
			limit: 5,
			before: new Date('2026-01-01T00:00:00.000Z'),
		});
	});

	it('rejects a non-integer limit with 400', async () => {
		await expect(GET(makeEvent('?limit=abc'))).rejects.toMatchObject({ status: 400 });
		expect(listAgentOccurrences).not.toHaveBeenCalled();
	});

	it('rejects an invalid before timestamp with 400', async () => {
		await expect(GET(makeEvent('?before=not-a-date'))).rejects.toMatchObject({ status: 400 });
		expect(listAgentOccurrences).not.toHaveBeenCalled();
	});

	it('returns the occurrences in the response body', async () => {
		listAgentOccurrences.mockResolvedValue([{ id: 'occ-1' }, { id: 'occ-2' }]);
		const res = await GET(makeEvent());
		expect(res.status).toBe(200);
		const body = await res.json();
		expect(body.occurrences).toEqual([{ id: 'occ-1' }, { id: 'occ-2' }]);
	});
});
