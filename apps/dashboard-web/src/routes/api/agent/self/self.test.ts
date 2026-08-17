import { describe, it, expect, vi, beforeEach } from 'vitest';

// R1a (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7f). GET /api/agent/self echoes exactly
// what `authenticateAgentRequest` already resolved -- no second query.

const authenticateAgentRequest = vi.fn();
vi.mock('$lib/server/agent-auth', () => ({ authenticateAgentRequest }));

const { GET } = await import('./+server');

function makeEvent() {
	return { request: new Request('http://localhost/api/agent/self') } as any;
}

beforeEach(() => {
	vi.clearAllMocks();
});

describe('GET /api/agent/self', () => {
	it('401s when unauthenticated', async () => {
		authenticateAgentRequest.mockRejectedValue(Object.assign(new Error('Unauthorized'), { status: 401 }));
		await expect(GET(makeEvent())).rejects.toMatchObject({ status: 401 });
	});

	it('returns agentId/name/organizationId and the key row, with expiresAt ISO-formatted', async () => {
		const expiresAt = new Date('2026-09-01T00:00:00.000Z');
		authenticateAgentRequest.mockResolvedValue({
			agentId: 'agent-1',
			organizationId: 'org-1',
			agentName: 'Triage Bot',
			keyPrefixForAudit: 'abc123def456',
			keyId: 'key-1',
			keyPrefix: 'sent_agent_',
			keyExpiresAt: expiresAt,
		});

		const res = await GET(makeEvent());
		expect(res.status).toBe(200);
		const body = await res.json();

		expect(body).toEqual({
			agentId: 'agent-1',
			name: 'Triage Bot',
			organizationId: 'org-1',
			key: {
				id: 'key-1',
				prefix: 'sent_agent_',
				expiresAt: '2026-09-01T00:00:00.000Z',
				lastUsedAt: null,
			},
		});
	});

	it('returns null expiresAt for a non-expiring key', async () => {
		authenticateAgentRequest.mockResolvedValue({
			agentId: 'agent-1',
			organizationId: 'org-1',
			agentName: 'Triage Bot',
			keyPrefixForAudit: 'abc123def456',
			keyId: 'key-1',
			keyPrefix: 'sent_agent_',
			keyExpiresAt: null,
		});

		const res = await GET(makeEvent());
		const body = await res.json();
		expect(body.key.expiresAt).toBeNull();
	});
});
