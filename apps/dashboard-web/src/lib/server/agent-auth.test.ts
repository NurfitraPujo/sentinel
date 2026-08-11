import { describe, it, expect, vi, beforeEach } from 'vitest';
import crypto from 'crypto';

/**
 * Manual Issues M5 stage 2 (design §7, B7) -- unit tests for agent-auth.ts. Uses the REAL sha256
 * hash (not a mock) so the "hash exactly like apikeys.ts's createApiKey computes it" claim in the
 * module's header is actually exercised, not merely asserted in a comment.
 */

function makeChainable() {
	const m: any = {};
	const methods = ['select', 'from', 'where'];
	for (const name of methods) {
		m[name] = vi.fn(() => m);
	}
	m.__result = [];
	m.then = vi.fn((resolve: any) => resolve(m.__result));
	return m;
}

const dbMock: any = makeChainable();
vi.mock('$lib/server/db', () => ({ db: dbMock }));
vi.mock('$lib/db/schema', () => ({
	projectApiKeys: {
		id: 'id',
		organizationId: 'organizationId',
		scope: 'scope',
		status: 'status',
		expiresAt: 'expiresAt',
		revokedAt: 'revokedAt',
		agentId: 'agentId',
		keyHash: 'keyHash',
	},
	agents: { id: 'id', orgId: 'orgId', name: 'name', status: 'status' },
}));

const { authenticateAgentRequest } = await import('./agent-auth');

function hashKey(raw: string): string {
	return crypto.createHash('sha256').update(raw).digest('hex');
}

function request(headers: Record<string, string> = {}): Request {
	return new Request('https://example.test/api/agent/issues', { headers });
}

// Queues the select-chain results for one call: first the key-row select, then (if reached) the
// agent-row select.
function queueResults(...results: unknown[][]) {
	let call = 0;
	dbMock.then = vi.fn((resolve: any) => resolve(results[call++] ?? []));
}

beforeEach(() => {
	vi.clearAllMocks();
});

describe('authenticateAgentRequest', () => {
	it('401s when the Authorization header is missing', async () => {
		await expect(authenticateAgentRequest(request())).rejects.toMatchObject({ status: 401 });
	});

	it('401s when the header is not a Bearer token', async () => {
		await expect(
			authenticateAgentRequest(request({ Authorization: 'Basic abc123' }))
		).rejects.toMatchObject({ status: 401 });
	});

	it('401s when the key hash matches no row (unknown key)', async () => {
		queueResults([]);
		await expect(
			authenticateAgentRequest(request({ Authorization: 'Bearer sent_agent_doesnotexist' }))
		).rejects.toMatchObject({ status: 401 });
	});

	it('401s when the key exists but has the wrong scope (e.g. "ingest")', async () => {
		queueResults([
			{
				id: 'key-1',
				organizationId: 'org-1',
				scope: 'ingest',
				status: 'active',
				expiresAt: null,
				revokedAt: null,
				agentId: 'agent-1',
			},
		]);
		await expect(
			authenticateAgentRequest(request({ Authorization: 'Bearer sent_ingest_key' }))
		).rejects.toMatchObject({ status: 401 });
	});

	it('401s when the key is revoked', async () => {
		queueResults([
			{
				id: 'key-1',
				organizationId: 'org-1',
				scope: 'agent',
				status: 'revoked',
				expiresAt: null,
				revokedAt: new Date(),
				agentId: 'agent-1',
			},
		]);
		await expect(
			authenticateAgentRequest(request({ Authorization: 'Bearer sent_agent_revoked' }))
		).rejects.toMatchObject({ status: 401 });
	});

	it('401s when the key is expired', async () => {
		queueResults([
			{
				id: 'key-1',
				organizationId: 'org-1',
				scope: 'agent',
				status: 'active',
				expiresAt: new Date(Date.now() - 1000),
				revokedAt: null,
				agentId: 'agent-1',
			},
		]);
		await expect(
			authenticateAgentRequest(request({ Authorization: 'Bearer sent_agent_expired' }))
		).rejects.toMatchObject({ status: 401 });
	});

	it('401s when the linked agent is disabled', async () => {
		queueResults(
			[
				{
					id: 'key-1',
					organizationId: 'org-1',
					scope: 'agent',
					status: 'active',
					expiresAt: null,
					revokedAt: null,
					agentId: 'agent-1',
				},
			],
			[{ id: 'agent-1', orgId: 'org-1', name: 'Bot', status: 'disabled' }]
		);
		await expect(
			authenticateAgentRequest(request({ Authorization: 'Bearer sent_agent_disabled' }))
		).rejects.toMatchObject({ status: 401 });
	});

	it('resolves {agentId, organizationId} for a valid active agent key', async () => {
		queueResults(
			[
				{
					id: 'key-1',
					organizationId: 'org-1',
					scope: 'agent',
					status: 'active',
					expiresAt: null,
					revokedAt: null,
					agentId: 'agent-1',
				},
			],
			[{ id: 'agent-1', orgId: 'org-1', name: 'AutoFix', status: 'active' }]
		);

		const ctx = await authenticateAgentRequest(request({ Authorization: 'Bearer sent_agent_valid' }));

		expect(ctx.agentId).toBe('agent-1');
		expect(ctx.organizationId).toBe('org-1');
		expect(ctx.agentName).toBe('AutoFix');
		expect(ctx.keyPrefixForAudit).toBe(hashKey('sent_agent_valid').slice(0, 12));
	});
});
