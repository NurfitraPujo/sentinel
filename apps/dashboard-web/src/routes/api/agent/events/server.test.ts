import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * N1b (events feed) -- unit tests for GET /api/agent/events. Mirrors agent-auth.test.ts's style
 * of exercising the real Request/URL shapes with a mocked auth context and query layer.
 */

const authenticateAgentRequest = vi.fn();
vi.mock('$lib/server/agent-auth', () => ({ authenticateAgentRequest }));

const listOrgActivity = vi.fn();
vi.mock('$lib/db/queries/events', () => ({ listOrgActivity }));

const { GET } = await import('./+server');

function makeEvent(query = '') {
	const url = new URL(`https://example.test/api/agent/events${query}`);
	const request = new Request(url, { headers: { authorization: 'Bearer secret' } });
	return { request, url } as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	authenticateAgentRequest.mockResolvedValue({
		agentId: 'agent-1',
		organizationId: 'org-1',
		agentName: 'Bot',
		keyPrefixForAudit: 'abc',
	});
	listOrgActivity.mockResolvedValue({ events: [], cursor: 0, hasMore: false });
});

describe('GET /api/agent/events', () => {
	it('propagates a 401 from authenticateAgentRequest without calling the query layer', async () => {
		const unauthorized = Object.assign(new Error('Missing bearer token'), { status: 401 });
		authenticateAgentRequest.mockRejectedValue(unauthorized);

		await expect(GET(makeEvent())).rejects.toMatchObject({ status: 401 });
		expect(listOrgActivity).not.toHaveBeenCalled();
	});

	it('defaults after to 0 and limit to 50', async () => {
		await GET(makeEvent());

		expect(listOrgActivity).toHaveBeenCalledWith(
			expect.objectContaining({ organizationId: 'org-1', after: 0, limit: 50 })
		);
	});

	it('rejects a non-numeric after with 400', async () => {
		const res = await GET(makeEvent('?after=abc'));

		expect(res.status).toBe(400);
		expect(listOrgActivity).not.toHaveBeenCalled();
	});

	it('rejects a non-numeric limit with 400', async () => {
		const res = await GET(makeEvent('?limit=abc'));

		expect(res.status).toBe(400);
		expect(listOrgActivity).not.toHaveBeenCalled();
	});

	it('clamps limit into [1, 200]', async () => {
		await GET(makeEvent('?limit=9999'));
		expect(listOrgActivity).toHaveBeenCalledWith(expect.objectContaining({ limit: 200 }));

		await GET(makeEvent('?limit=0'));
		expect(listOrgActivity).toHaveBeenCalledWith(expect.objectContaining({ limit: 1 }));
	});

	it('parses a comma-separated type filter', async () => {
		await GET(makeEvent('?type=status_changed,claimed'));

		expect(listOrgActivity).toHaveBeenCalledWith(
			expect.objectContaining({ eventTypes: ['status_changed', 'claimed'] })
		);
	});

	it('rejects an unknown event type with 400', async () => {
		const res = await GET(makeEvent('?type=status_changed,bogus_type'));

		expect(res.status).toBe(400);
		const body = await res.json();
		expect(body.error).toMatch(/bogus_type/);
		expect(listOrgActivity).not.toHaveBeenCalled();
	});

	it('passes project through as projectId', async () => {
		await GET(makeEvent('?project=project-1'));

		expect(listOrgActivity).toHaveBeenCalledWith(expect.objectContaining({ projectId: 'project-1' }));
	});

	it('maps claimed=me to the credential agentId, never a request-supplied id', async () => {
		await GET(makeEvent('?claimed=me'));

		expect(listOrgActivity).toHaveBeenCalledWith(expect.objectContaining({ claimedByAgentId: 'agent-1' }));
	});

	it('rejects claimed values other than "me" with 400', async () => {
		const res = await GET(makeEvent('?claimed=agent-2'));

		expect(res.status).toBe(400);
		expect(listOrgActivity).not.toHaveBeenCalled();
	});

	it('scopes organizationId strictly from the credential, never the URL', async () => {
		await GET(makeEvent('?organizationId=org-attacker'));

		expect(listOrgActivity).toHaveBeenCalledWith(expect.objectContaining({ organizationId: 'org-1' }));
	});

	it('returns the events/cursor/hasMore shape from the query layer', async () => {
		listOrgActivity.mockResolvedValue({
			events: [{ seq: 7, eventType: 'claimed', actorType: 'agent', actorId: 'agent-1', oldValue: null, newValue: null, createdAt: null, issue: { id: 'i1', title: 't', status: 'unresolved', issueType: 'system_error', projectId: 'p1' } }],
			cursor: 7,
			hasMore: true,
		});

		const res = await GET(makeEvent());
		const body = await res.json();

		expect(body.cursor).toBe(7);
		expect(body.hasMore).toBe(true);
		expect(body.events).toHaveLength(1);
	});
});
