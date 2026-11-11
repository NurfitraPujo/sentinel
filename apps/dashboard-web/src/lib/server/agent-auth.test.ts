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
		createdAt: 'createdAt',
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
					createdAt: new Date('2026-08-01T00:00:00.000Z'),
				},
			],
			[{ id: 'agent-1', orgId: 'org-1', name: 'AutoFix', status: 'active' }]
		);

		const ctx = await authenticateAgentRequest(request({ Authorization: 'Bearer sent_agent_valid' }));

		expect(ctx.agentId).toBe('agent-1');
		expect(ctx.organizationId).toBe('org-1');
		expect(ctx.agentName).toBe('AutoFix');
		expect(ctx.keyPrefixForAudit).toBe(hashKey('sent_agent_valid').slice(0, 12));
		// N9 (C6): created_at is carried through from the same lookup for age-based rotation.
		expect(ctx.keyCreatedAt).toEqual(new Date('2026-08-01T00:00:00.000Z'));
	});
});

// R19 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): project_api_keys.rate_limit_rpm is now
// enforced inside authenticateAgentRequest itself.
describe('authenticateAgentRequest rate limiting (R19)', () => {
	function validKeyRows(rawKey: string, rateLimitRpm: number) {
		return [
			[
				{
					id: `key-for-${rawKey}`,
					organizationId: 'org-1',
					scope: 'agent',
					status: 'active',
					expiresAt: null,
					revokedAt: null,
					agentId: 'agent-1',
					rateLimitRpm,
				},
			],
			[{ id: 'agent-1', orgId: 'org-1', name: 'AutoFix', status: 'active' }],
		];
	}

	it('a key under its limit keeps authenticating', async () => {
		const rawKey = 'sent_agent_under_limit';
		queueResults(...validKeyRows(rawKey, 2));

		await expect(authenticateAgentRequest(request({ Authorization: `Bearer ${rawKey}` }))).resolves.toMatchObject({
			agentId: 'agent-1',
		});
	});

	it('a key that hits its rate_limit_rpm gets 429 with a Retry-After header', async () => {
		const rawKey = 'sent_agent_at_limit';

		// limit=1: the FIRST call establishes the window and is allowed (checkRateLimitWithLimit's
		// "no entry yet" branch always allows the opening request); the SECOND call within the same
		// window must be rejected.
		queueResults(...validKeyRows(rawKey, 1));
		await authenticateAgentRequest(request({ Authorization: `Bearer ${rawKey}` }));

		queueResults(...validKeyRows(rawKey, 1));
		let caught: unknown;
		try {
			await authenticateAgentRequest(request({ Authorization: `Bearer ${rawKey}` }));
		} catch (err) {
			caught = err;
		}

		expect(caught).toBeInstanceOf(Response);
		const res = caught as Response;
		expect(res.status).toBe(429);
		// A10 (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7f): must be a positive integer
		// number of seconds computed from the limiter's own `resetAt` (rate-limit.ts), not a
		// placeholder/fixed value -- "truthy" alone would pass a garbage non-numeric header.
		const retryAfter = res.headers.get('Retry-After');
		expect(retryAfter).toMatch(/^\d+$/);
		expect(Number(retryAfter)).toBeGreaterThan(0);
		expect(Number(retryAfter)).toBeLessThanOrEqual(60);
	});
});
