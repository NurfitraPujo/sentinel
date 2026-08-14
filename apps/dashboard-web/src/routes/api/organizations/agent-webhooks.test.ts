import { describe, it, expect, vi, beforeEach } from 'vitest';

// A chainable Drizzle-query double, mirroring agents.test.ts's approach: requireOrgMembership
// (../keys/_shared.ts, reused unchanged by these routes) reads through this.
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

const agentQueries = {
	getAgentById: vi.fn(),
};
vi.mock('$lib/db/queries/agents', () => agentQueries);

const webhookQueries = {
	listAgentWebhooks: vi.fn(),
	getAgentWebhookById: vi.fn(),
	createAgentWebhook: vi.fn(),
	updateAgentWebhook: vi.fn(),
	deleteAgentWebhook: vi.fn(),
};
vi.mock('$lib/db/queries/agent-webhooks', () => webhookQueries);

const { GET, POST } = await import('./[orgId]/agents/[agentId]/webhooks/+server');
const { PATCH, DELETE } = await import('./[orgId]/agents/[agentId]/webhooks/[webhookId]/+server');

function locals(session: { id: string } | null) {
	return { auth: async () => (session ? { user: { id: session.id } } : null) } as any;
}

function membershipRow(role: string) {
	return { role };
}

const AGENT = { id: 'agent-1', orgId: 'org-1', name: 'AutoFix', kind: 'ai', status: 'active' };

describe('agent webhook management routes', () => {
	beforeEach(() => {
		vi.clearAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.then.mockReset();
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
	});

	describe('GET /webhooks', () => {
		it('401s when there is no session', async () => {
			await expect(
				GET({ params: { orgId: 'org-1', agentId: 'agent-1' }, locals: locals(null) } as any)
			).rejects.toMatchObject({ status: 401 });
		});

		it.each(['engineer', 'support', 'viewer'])('403s for org %s (not owner/admin)', async (role) => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow(role)]));
			await expect(
				GET({ params: { orgId: 'org-1', agentId: 'agent-1' }, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 403 });
			expect(webhookQueries.listAgentWebhooks).not.toHaveBeenCalled();
		});

		it('404s when the agent belongs to a different organization', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			agentQueries.getAgentById.mockResolvedValueOnce({ ...AGENT, orgId: 'org-2' });
			await expect(
				GET({ params: { orgId: 'org-1', agentId: 'agent-1' }, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 404 });
			expect(webhookQueries.listAgentWebhooks).not.toHaveBeenCalled();
		});

		it('200s and lists webhooks without the secret field', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('admin')]));
			agentQueries.getAgentById.mockResolvedValueOnce(AGENT);
			webhookQueries.listAgentWebhooks.mockResolvedValueOnce([
				{ id: 'wh-1', url: 'https://x.test/hook', secretPrefix: 'whsec_abc' },
			]);

			const res = await GET({ params: { orgId: 'org-1', agentId: 'agent-1' }, locals: locals({ id: 'user-1' }) } as any);
			const body = await res.json();
			expect(body.webhooks).toEqual([{ id: 'wh-1', url: 'https://x.test/hook', secretPrefix: 'whsec_abc' }]);
			expect(JSON.stringify(body)).not.toContain('"secret"');
			expect(webhookQueries.listAgentWebhooks).toHaveBeenCalledWith('org-1', 'agent-1');
		});
	});

	describe('POST /webhooks (create)', () => {
		it('403s for an engineer', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('engineer')]));
			const request = new Request('http://x', { method: 'POST', body: JSON.stringify({ url: 'https://x.test/hook' }) });
			await expect(
				POST({ params: { orgId: 'org-1', agentId: 'agent-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 403 });
			expect(webhookQueries.createAgentWebhook).not.toHaveBeenCalled();
		});

		it('404s when the agent belongs to a different organization', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			agentQueries.getAgentById.mockResolvedValueOnce({ ...AGENT, orgId: 'org-2' });
			const request = new Request('http://x', { method: 'POST', body: JSON.stringify({ url: 'https://x.test/hook' }) });
			await expect(
				POST({ params: { orgId: 'org-1', agentId: 'agent-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 404 });
			expect(webhookQueries.createAgentWebhook).not.toHaveBeenCalled();
		});

		it('400s for an invalid url', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			agentQueries.getAgentById.mockResolvedValueOnce(AGENT);
			const request = new Request('http://x', { method: 'POST', body: JSON.stringify({ url: 'http://example.com/hook' }) });
			await expect(
				POST({ params: { orgId: 'org-1', agentId: 'agent-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 400 });
			expect(webhookQueries.createAgentWebhook).not.toHaveBeenCalled();
		});

		it('400s for an invalid event type', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			agentQueries.getAgentById.mockResolvedValueOnce(AGENT);
			const request = new Request('http://x', {
				method: 'POST',
				body: JSON.stringify({ url: 'https://x.test/hook', eventTypes: ['not_a_real_type'] }),
			});
			await expect(
				POST({ params: { orgId: 'org-1', agentId: 'agent-1' }, request, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 400 });
			expect(webhookQueries.createAgentWebhook).not.toHaveBeenCalled();
		});

		it('201s and returns the secret exactly once on create', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			agentQueries.getAgentById.mockResolvedValueOnce(AGENT);
			webhookQueries.createAgentWebhook.mockResolvedValueOnce({
				webhook: { id: 'wh-1', url: 'https://x.test/hook', secretPrefix: 'whsec_abc' },
				secret: 'whsec_abcdef0123456789',
			});
			const request = new Request('http://x', {
				method: 'POST',
				body: JSON.stringify({ url: 'https://x.test/hook', eventTypes: ['status_changed'] }),
			});

			const res = await POST({ params: { orgId: 'org-1', agentId: 'agent-1' }, request, locals: locals({ id: 'user-1' }) } as any);
			expect(res.status).toBe(201);
			const body = await res.json();
			expect(body.secret).toBe('whsec_abcdef0123456789');
			expect(webhookQueries.createAgentWebhook).toHaveBeenCalledWith('user-1', {
				organizationId: 'org-1',
				agentId: 'agent-1',
				url: 'https://x.test/hook',
				eventTypes: ['status_changed'],
			});
		});
	});

	describe('PATCH /webhooks/[webhookId]', () => {
		it('404s when the webhook belongs to a different agent', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			agentQueries.getAgentById.mockResolvedValueOnce(AGENT);
			webhookQueries.getAgentWebhookById.mockResolvedValueOnce({
				id: 'wh-1',
				organizationId: 'org-1',
				agentId: 'agent-OTHER',
			});
			const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ status: 'disabled' }) });
			await expect(
				PATCH({
					params: { orgId: 'org-1', agentId: 'agent-1', webhookId: 'wh-1' },
					request,
					locals: locals({ id: 'user-1' }),
				} as any)
			).rejects.toMatchObject({ status: 404 });
			expect(webhookQueries.updateAgentWebhook).not.toHaveBeenCalled();
		});

		it('400s for an invalid status transition value', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			agentQueries.getAgentById.mockResolvedValueOnce(AGENT);
			webhookQueries.getAgentWebhookById.mockResolvedValueOnce({ id: 'wh-1', organizationId: 'org-1', agentId: 'agent-1' });
			const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ status: 'failed' }) });
			await expect(
				PATCH({
					params: { orgId: 'org-1', agentId: 'agent-1', webhookId: 'wh-1' },
					request,
					locals: locals({ id: 'user-1' }),
				} as any)
			).rejects.toMatchObject({ status: 400 });
			expect(webhookQueries.updateAgentWebhook).not.toHaveBeenCalled();
		});

		it('200s and applies a status transition for an admin', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('admin')]));
			agentQueries.getAgentById.mockResolvedValueOnce(AGENT);
			webhookQueries.getAgentWebhookById.mockResolvedValueOnce({ id: 'wh-1', organizationId: 'org-1', agentId: 'agent-1' });
			webhookQueries.updateAgentWebhook.mockResolvedValueOnce({ id: 'wh-1', status: 'active' });
			const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ status: 'active' }) });

			const res = await PATCH({
				params: { orgId: 'org-1', agentId: 'agent-1', webhookId: 'wh-1' },
				request,
				locals: locals({ id: 'user-1' }),
			} as any);
			const body = await res.json();
			expect(body).toEqual({ webhook: { id: 'wh-1', status: 'active' } });
			expect(webhookQueries.updateAgentWebhook).toHaveBeenCalledWith('user-1', 'org-1', 'agent-1', 'wh-1', {
				status: 'active',
			});
		});
	});

	describe('DELETE /webhooks/[webhookId]', () => {
		it('cross-org 404s before deleting', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			agentQueries.getAgentById.mockResolvedValueOnce(AGENT);
			webhookQueries.getAgentWebhookById.mockResolvedValueOnce({ id: 'wh-1', organizationId: 'org-2', agentId: 'agent-1' });
			await expect(
				DELETE({ params: { orgId: 'org-1', agentId: 'agent-1', webhookId: 'wh-1' }, locals: locals({ id: 'user-1' }) } as any)
			).rejects.toMatchObject({ status: 404 });
			expect(webhookQueries.deleteAgentWebhook).not.toHaveBeenCalled();
		});

		it('204s and deletes for an owner', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			agentQueries.getAgentById.mockResolvedValueOnce(AGENT);
			webhookQueries.getAgentWebhookById.mockResolvedValueOnce({ id: 'wh-1', organizationId: 'org-1', agentId: 'agent-1' });

			const res = await DELETE({
				params: { orgId: 'org-1', agentId: 'agent-1', webhookId: 'wh-1' },
				locals: locals({ id: 'user-1' }),
			} as any);
			expect(res.status).toBe(204);
			expect(webhookQueries.deleteAgentWebhook).toHaveBeenCalledWith('user-1', 'org-1', 'agent-1', 'wh-1');
		});
	});
});
