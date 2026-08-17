import { describe, it, expect, vi, beforeEach } from 'vitest';

// N7b (A02): GET /api/agent/issues -- since/sort/limit/cursor param validation and pass-through.
// Mirrors occurrences.test.ts's mock-and-assert style.

const listAgentIssues = vi.fn();
const decodeAgentIssuesCursor = vi.fn();

vi.mock('$lib/db/queries/agent-work', () => ({
	listAgentIssues,
	decodeAgentIssuesCursor,
	AGENT_ISSUES_MAX_LIMIT: 200,
}));

const authenticateAgentRequest = vi.fn();
vi.mock('$lib/server/agent-auth', () => ({ authenticateAgentRequest }));

const { GET } = await import('./+server');

function makeEvent(query = '') {
	const url = new URL(`http://localhost/api/agent/issues${query}`);
	return { request: new Request(url), url } as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	authenticateAgentRequest.mockResolvedValue({ agentId: 'agent-1', organizationId: 'org-1' });
	listAgentIssues.mockResolvedValue({ issues: [] });
});

describe('GET /api/agent/issues -- legacy behavior (params absent)', () => {
	it('passes since/sort/limit/cursor as undefined and response omits nextCursor', async () => {
		const res = await GET(makeEvent());

		expect(res.status).toBe(200);
		expect(listAgentIssues).toHaveBeenCalledWith(
			expect.objectContaining({ since: undefined, sort: undefined, limit: undefined, cursor: undefined })
		);
		const body = await res.json();
		expect(body).toEqual({ issues: [] });
		expect(body).not.toHaveProperty('nextCursor');
	});
});

describe('GET /api/agent/issues -- since', () => {
	it('400s on an invalid since timestamp', async () => {
		const res = await GET(makeEvent('?since=not-a-date'));
		expect(res.status).toBe(400);
		expect(await res.json()).toEqual({ error: 'since must be a valid ISO timestamp' });
		expect(listAgentIssues).not.toHaveBeenCalled();
	});

	it('parses a valid since into a Date and forwards it', async () => {
		await GET(makeEvent('?since=2026-08-01T00:00:00Z'));
		expect(listAgentIssues).toHaveBeenCalledWith(
			expect.objectContaining({ since: new Date('2026-08-01T00:00:00Z') })
		);
	});
});

describe('GET /api/agent/issues -- sort', () => {
	it('400s on an invalid sort value', async () => {
		const res = await GET(makeEvent('?sort=bogus'));
		expect(res.status).toBe(400);
		expect(await res.json()).toEqual({ error: 'sort must be one of: firstSeen, lastSeen' });
		expect(listAgentIssues).not.toHaveBeenCalled();
	});

	it('accepts firstSeen and lastSeen', async () => {
		await GET(makeEvent('?sort=firstSeen'));
		expect(listAgentIssues).toHaveBeenCalledWith(expect.objectContaining({ sort: 'firstSeen' }));
	});
});

describe('GET /api/agent/issues -- limit', () => {
	it('400s on a non-positive-integer limit', async () => {
		for (const bad of ['0', '-1', '1.5', 'abc']) {
			listAgentIssues.mockClear();
			const res = await GET(makeEvent(`?limit=${bad}`));
			expect(res.status).toBe(400);
			expect(listAgentIssues).not.toHaveBeenCalled();
		}
	});

	it('forwards a valid limit unclamped (clamping is the query layer\'s job)', async () => {
		await GET(makeEvent('?limit=5000'));
		expect(listAgentIssues).toHaveBeenCalledWith(expect.objectContaining({ limit: 5000 }));
	});
});

describe('GET /api/agent/issues -- claimed', () => {
	it('N9 (C12): claimed=me resolves claimedByAgentId from the credential (B7), not a param', async () => {
		await GET(makeEvent('?claimed=me'));
		expect(listAgentIssues).toHaveBeenCalledWith(
			expect.objectContaining({ claimedByAgentId: 'agent-1', claimed: undefined })
		);
	});

	it('forwards claimed=true/false as the boolean, with no claimedByAgentId', async () => {
		await GET(makeEvent('?claimed=true'));
		expect(listAgentIssues).toHaveBeenCalledWith(
			expect.objectContaining({ claimed: true, claimedByAgentId: undefined })
		);
	});

	it('400s on an invalid claimed value', async () => {
		const res = await GET(makeEvent('?claimed=bogus'));
		expect(res.status).toBe(400);
		expect(await res.json()).toEqual({ error: 'claimed must be "true", "false", or "me"' });
		expect(listAgentIssues).not.toHaveBeenCalled();
	});
});

describe('GET /api/agent/issues -- cursor', () => {
	it('400s when decodeAgentIssuesCursor throws', async () => {
		decodeAgentIssuesCursor.mockImplementation(() => {
			throw new Error('invalid cursor');
		});
		const res = await GET(makeEvent('?cursor=garbage'));
		expect(res.status).toBe(400);
		expect(await res.json()).toEqual({ error: 'cursor is invalid or malformed' });
		expect(listAgentIssues).not.toHaveBeenCalled();
	});

	it('forwards the decoded cursor', async () => {
		const decoded = { sortValue: new Date('2026-08-05T00:00:00Z'), id: 'issue-1' };
		decodeAgentIssuesCursor.mockReturnValue(decoded);
		await GET(makeEvent('?cursor=abc123'));
		expect(listAgentIssues).toHaveBeenCalledWith(expect.objectContaining({ cursor: decoded }));
	});
});

describe('GET /api/agent/issues -- nextCursor in response', () => {
	it('includes nextCursor in the JSON body when the query layer returns one', async () => {
		listAgentIssues.mockResolvedValue({ issues: [], nextCursor: 'next-abc' });
		const res = await GET(makeEvent('?limit=50'));
		const body = await res.json();
		expect(body).toEqual({ issues: [], nextCursor: 'next-abc' });
	});
});
