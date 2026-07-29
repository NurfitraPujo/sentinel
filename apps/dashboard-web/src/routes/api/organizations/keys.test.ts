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
};
vi.mock('$lib/db/queries/apikeys', () => apikeyQueries);

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
			apikeyQueries.getOrganizationApiKeys.mockResolvedValueOnce([{ id: 'key-1' }]);

			const res = await GET({ params: { orgId: 'org-1' }, locals: locals({ id: 'user-1' }) } as any);
			const body = await res.json();
			expect(body).toEqual({ keys: [{ id: 'key-1' }] });
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
			apikeyQueries.createApiKey.mockResolvedValueOnce({
				apiKey: { id: 'key-new' },
				secretToken: 'sent_org_deadbeef',
			});
			const request = new Request('http://x', { method: 'POST', body: JSON.stringify({ name: 'k', scope: 'ingest' }) });

			const res = await POST({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-1' }) } as any);
			expect(res.status).toBe(201);
			const body = await res.json();
			expect(body.token).toBe('sent_org_deadbeef');
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
			apikeyQueries.rotateApiKey.mockResolvedValueOnce({
				apiKey: { id: 'key-2' },
				secretToken: 'sent_org_newsecret',
			});

			const res = await ROTATE({ params: { orgId: 'org-1', keyId: 'key-1' }, locals: locals({ id: 'user-1' }) } as any);
			const body = await res.json();
			expect(body.success).toBe(true);
			expect(body.token).toBe('sent_org_newsecret');
		});
	});
});
