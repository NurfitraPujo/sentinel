import { describe, it, expect, vi, beforeEach } from 'vitest';

// A chainable Drizzle-query double, mirroring src/lib/db/queries/apikeys.test.ts's approach:
// each awaited chain resolves via the queued `then` implementation below.
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
	// `clearAllMocks` clears call records but NOT queued `mockImplementationOnce` entries, so an
	// un-consumed queued resolution leaks into the NEXT test and answers the wrong query, making
	// results order-dependent. `mockReset` on the queue-bearing mock drops the queue; the base
	// implementation is re-established on the next line.
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

const apikeyQueries = {
	getOrganizationApiKeys: vi.fn(),
	createApiKey: vi.fn(),
	getApiKeyById: vi.fn(),
	revokeApiKey: vi.fn(),
	rotateApiKey: vi.fn(),
	createNatsPublisher: vi.fn(() => ({ publish: vi.fn().mockResolvedValue(undefined) })),
	// Real implementation (not a mock) so the route code under test exercises the actual
	// stripping behaviour, matching src/lib/db/queries/apikeys.ts's toPublicKey.
	toPublicKey: (row: Record<string, unknown>) => {
		const { keyHash: _keyHash, ...rest } = row;
		return rest;
	},
};
vi.mock('$lib/db/queries/apikeys', () => apikeyQueries);

// M5 §7: keys/+server.ts's POST handler resolves agentId (for scope='agent') via
// $lib/db/queries/agents, so it needs its own double here — real agents.ts would otherwise hit
// this file's schema mock, which does not declare an `agents` export.
const agentQueries = {
	getAgentById: vi.fn(),
};
vi.mock('$lib/db/queries/agents', () => agentQueries);

// Recursively walks an arbitrary response body and fails if a 'keyHash' property is found at
// any depth (top-level, nested inside `key`, inside arrays, etc). D09: keyHash is the SHA-256 of
// the live secret and is the exact value the ingestor's Redis cache is keyed on
// (apps/ingestor-go/auth/apikey.go:53) — it must never reach the browser.
function assertNoKeyHash(value: unknown, path = 'body') {
	if (value === null || typeof value !== 'object') return;
	if (Array.isArray(value)) {
		value.forEach((item, i) => assertNoKeyHash(item, `${path}[${i}]`));
		return;
	}
	for (const [key, val] of Object.entries(value as Record<string, unknown>)) {
		if (key === 'keyHash') {
			throw new Error(`Found forbidden 'keyHash' key at ${path}.${key}`);
		}
		assertNoKeyHash(val, `${path}.${key}`);
	}
}

const { GET, POST } = await import('./[orgId]/keys/+server');
const { DELETE } = await import('./[orgId]/keys/[keyId]/+server');
const { POST: ROTATE } = await import('./[orgId]/keys/[keyId]/rotate/+server');

function locals(session: { id: string } | null) {
	return { auth: async () => (session ? { user: { id: session.id } } : null) } as any;
}

function membershipRow(role: string) {
	return { role };
}

describe('organization API key routes', () => {
	beforeEach(() => {
		vi.clearAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.then.mockReset();
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
	});

	describe('GET /keys', () => {
		it('401s when there is no session', async () => {
			await expect(
				GET({ params: { orgId: 'org-1' }, locals: locals(null) } as any)
			).rejects.toMatchObject({ status: 401 });
		});

		it('403s when the caller is not a member of the organization', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([])); // no membership row
			await expect(
				GET({ params: { orgId: 'org-1' }, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 403 });
		});

		it('200s and lists keys for a viewer member (read-only role can still view)', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('viewer')]));
			// D09 regression guard: even if getOrganizationApiKeys ever regresses to selecting
			// keyHash, the route must still strip it before serializing.
			apikeyQueries.getOrganizationApiKeys.mockResolvedValueOnce([{ id: 'key-1', keyHash: 'deadbeef' }]);

			const res = await GET({ params: { orgId: 'org-1' }, locals: locals({ id: 'user-1' }) } as any);
			const body = await res.json();
			expect(body).toEqual({ keys: [{ id: 'key-1' }] });
			assertNoKeyHash(body);
			expect(apikeyQueries.getOrganizationApiKeys).toHaveBeenCalledWith('org-1');
		});
	});

	describe('POST /keys (create)', () => {
		it('401s when there is no session', async () => {
			await expect(
				POST({ params: { orgId: 'org-1' }, request: new Request('http://x', { method: 'POST', body: '{}' }), locals: locals(null) } as any)
			).rejects.toMatchObject({ status: 401 });
		});

		it('403s for a viewer (insufficient permission to manage keys)', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('viewer')]));
			const request = new Request('http://x', { method: 'POST', body: JSON.stringify({ name: 'k' }) });
			await expect(
				POST({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 403 });
		});

		it.each(['owner', 'admin', 'engineer'])('201s for an org %s', async (role) => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow(role)]));
			// D09 regression guard: even if createApiKey's .returning() ever regresses to a bare
			// call that includes keyHash on the row, the route must still strip it.
			apikeyQueries.createApiKey.mockResolvedValueOnce({
				apiKey: { id: 'key-new', keyHash: 'deadbeef-hash' },
				secretToken: 'sent_org_deadbeef',
			});
			const request = new Request('http://x', { method: 'POST', body: JSON.stringify({ name: 'k', scope: 'ingest' }) });

			const res = await POST({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-1' }) } as any);
			expect(res.status).toBe(201);
			const body = await res.json();
			expect(body.token).toBe('sent_org_deadbeef');
			assertNoKeyHash(body);
			expect(apikeyQueries.createApiKey).toHaveBeenCalledWith(
				'user-1',
				expect.objectContaining({ organizationId: 'org-1', name: 'k', scope: 'ingest', projectId: null })
			);
		});

		it('403s for org support (read-only, cannot manage keys)', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('support')]));
			const request = new Request('http://x', { method: 'POST', body: JSON.stringify({ name: 'k' }) });
			await expect(
				POST({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 403 });
		});

		// D28: an unrecognized scope used to be silently downgraded to 'ingest' and reported as
		// a 201 success. It must now be rejected outright.
		it('400s for an unrecognized scope instead of silently downgrading to ingest', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			const request = new Request('http://x', {
				method: 'POST',
				body: JSON.stringify({ name: 'k', scope: 'superadmin' }),
			});
			await expect(
				POST({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 400 });
			expect(apikeyQueries.createApiKey).not.toHaveBeenCalled();
		});

		// D28: rateLimitRpm accepted any number (negative, zero, or absurdly large) and passed it
		// straight through.
		it.each([-1, 0, 1e12, 1.5])('400s for an invalid rateLimitRpm (%s)', async (rpm) => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			const request = new Request('http://x', {
				method: 'POST',
				body: JSON.stringify({ name: 'k', scope: 'ingest', rateLimitRpm: rpm }),
			});
			await expect(
				POST({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 400 });
			expect(apikeyQueries.createApiKey).not.toHaveBeenCalled();
		});

		// M5 §7/§9: agent-key issuance is gated by 'manage_agents' (owner/admin), narrower than
		// the 'manage_keys' gate (owner/admin/engineer) every other scope uses.
		it('403s for an engineer trying to issue an agent key', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('engineer')]));
			const request = new Request('http://x', {
				method: 'POST',
				body: JSON.stringify({ name: 'agent key', scope: 'agent', agentId: 'agent-1' }),
			});
			await expect(
				POST({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 403 });
			expect(apikeyQueries.createApiKey).not.toHaveBeenCalled();
		});

		it('400s issuing an agent key without an agentId', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			const request = new Request('http://x', {
				method: 'POST',
				body: JSON.stringify({ name: 'agent key', scope: 'agent' }),
			});
			await expect(
				POST({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 400 });
			expect(apikeyQueries.createApiKey).not.toHaveBeenCalled();
		});

		it('400s issuing an agent key whose agentId belongs to a different organization', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			agentQueries.getAgentById.mockResolvedValueOnce({ id: 'agent-1', orgId: 'org-2' });
			const request = new Request('http://x', {
				method: 'POST',
				body: JSON.stringify({ name: 'agent key', scope: 'agent', agentId: 'agent-1' }),
			});
			await expect(
				POST({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 400 });
			expect(apikeyQueries.createApiKey).not.toHaveBeenCalled();
		});

		it('201s for an owner issuing an agent key, org-scoped (projectId null) regardless of a supplied projectId', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			agentQueries.getAgentById.mockResolvedValueOnce({ id: 'agent-1', orgId: 'org-1' });
			apikeyQueries.createApiKey.mockResolvedValueOnce({
				apiKey: { id: 'key-new', keyHash: 'deadbeef-hash', agentId: 'agent-1' },
				secretToken: 'sent_agent_deadbeef',
			});
			const request = new Request('http://x', {
				method: 'POST',
				body: JSON.stringify({ name: 'agent key', scope: 'agent', agentId: 'agent-1', projectId: 'proj-1' }),
			});

			const res = await POST({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-1' }) } as any);
			expect(res.status).toBe(201);
			expect(apikeyQueries.createApiKey).toHaveBeenCalledWith(
				'user-1',
				expect.objectContaining({ scope: 'agent', agentId: 'agent-1', projectId: null })
			);
		});

		it('201s for a valid positive integer rateLimitRpm', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			apikeyQueries.createApiKey.mockResolvedValueOnce({
				apiKey: { id: 'key-new', keyHash: 'deadbeef-hash' },
				secretToken: 'sent_org_deadbeef',
			});
			const request = new Request('http://x', {
				method: 'POST',
				body: JSON.stringify({ name: 'k', scope: 'ingest', rateLimitRpm: 1000 }),
			});
			const res = await POST({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-1' }) } as any);
			expect(res.status).toBe(201);
			expect(apikeyQueries.createApiKey).toHaveBeenCalledWith(
				'user-1',
				expect.objectContaining({ rateLimitRpm: 1000 })
			);
		});
	});

	describe('DELETE /keys/[keyId] (revoke)', () => {
		it('404s when the key belongs to a different organization', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('admin')])); // membership in org-1
			apikeyQueries.getApiKeyById.mockResolvedValueOnce({ id: 'key-1', organizationId: 'org-2' }); // key is org-2's

			await expect(
				DELETE({ params: { orgId: 'org-1', keyId: 'key-1' }, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 404 });
			expect(apikeyQueries.revokeApiKey).not.toHaveBeenCalled();
		});

		it('revokes and returns success when the key belongs to the caller\'s organization', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('admin')]));
			apikeyQueries.getApiKeyById.mockResolvedValueOnce({ id: 'key-1', organizationId: 'org-1' });
			apikeyQueries.revokeApiKey.mockResolvedValueOnce({ id: 'key-1', status: 'revoked' });

			const res = await DELETE({ params: { orgId: 'org-1', keyId: 'key-1' }, locals: locals({ id: 'user-1' }) } as any);
			const body = await res.json();
			expect(body.success).toBe(true);
			expect(apikeyQueries.revokeApiKey).toHaveBeenCalledWith('user-1', 'key-1', expect.anything());
		});

		it('403s for a viewer', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('viewer')]));
			await expect(
				DELETE({ params: { orgId: 'org-1', keyId: 'key-1' }, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 403 });
			expect(apikeyQueries.getApiKeyById).not.toHaveBeenCalled();
		});

		// M5 §7/§9: revoking an agent's key needs 'manage_agents' (owner/admin), not the broader
		// 'manage_keys' (owner/admin/engineer) that every other scope uses.
		it('403s for an engineer revoking an agent-scoped key', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('engineer')]));
			apikeyQueries.getApiKeyById.mockResolvedValueOnce({ id: 'key-1', organizationId: 'org-1', scope: 'agent' });

			await expect(
				DELETE({ params: { orgId: 'org-1', keyId: 'key-1' }, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 403 });
			expect(apikeyQueries.revokeApiKey).not.toHaveBeenCalled();
		});

		it('revokes an agent-scoped key for an admin', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('admin')]));
			apikeyQueries.getApiKeyById.mockResolvedValueOnce({ id: 'key-1', organizationId: 'org-1', scope: 'agent' });
			apikeyQueries.revokeApiKey.mockResolvedValueOnce({ id: 'key-1', status: 'revoked' });

			const res = await DELETE({ params: { orgId: 'org-1', keyId: 'key-1' }, locals: locals({ id: 'user-1' }) } as any);
			const body = await res.json();
			expect(body.success).toBe(true);
		});
	});

	describe('POST /keys/[keyId]/rotate', () => {
		it('404s across organizations, same as revoke', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			apikeyQueries.getApiKeyById.mockResolvedValueOnce({ id: 'key-1', organizationId: 'org-2' });

			await expect(
				ROTATE({ params: { orgId: 'org-1', keyId: 'key-1' }, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 404 });
			expect(apikeyQueries.rotateApiKey).not.toHaveBeenCalled();
		});

		it('rotates for an owner and returns the new secret once', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			apikeyQueries.getApiKeyById.mockResolvedValueOnce({ id: 'key-1', organizationId: 'org-1' });
			// D09 regression guard: even if rotateApiKey's .returning() ever regresses to a bare
			// call that includes keyHash on the row, the route must still strip it.
			apikeyQueries.rotateApiKey.mockResolvedValueOnce({
				apiKey: { id: 'key-2', keyHash: 'new-hash' },
				secretToken: 'sent_org_newsecret',
			});

			const res = await ROTATE({ params: { orgId: 'org-1', keyId: 'key-1' }, locals: locals({ id: 'user-1' }) } as any);
			const body = await res.json();
			expect(body.success).toBe(true);
			expect(body.token).toBe('sent_org_newsecret');
			assertNoKeyHash(body);
		});
	});
});
