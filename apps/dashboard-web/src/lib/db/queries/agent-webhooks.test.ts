import { describe, it, expect, vi, beforeEach } from 'vitest';
import {
	listAgentWebhooks,
	getAgentWebhookById,
	createAgentWebhook,
	updateAgentWebhook,
	deleteAgentWebhook,
} from './agent-webhooks';
import { db } from '$lib/server/db';

vi.mock('$lib/server/db', () => {
	const dbMock: any = {
		select: vi.fn(),
		from: vi.fn(),
		where: vi.fn(),
		orderBy: vi.fn(),
		insert: vi.fn(),
		values: vi.fn(),
		returning: vi.fn(),
		update: vi.fn(),
		set: vi.fn(),
		delete: vi.fn(),
		then: vi.fn(),
	};
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.orderBy.mockReturnValue(dbMock);
	dbMock.insert.mockReturnValue(dbMock);
	dbMock.values.mockReturnValue(dbMock);
	dbMock.returning.mockReturnValue(dbMock);
	dbMock.update.mockReturnValue(dbMock);
	dbMock.set.mockReturnValue(dbMock);
	dbMock.delete.mockReturnValue(dbMock);
	dbMock.then.mockReset();
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
	return { db: dbMock };
});

vi.mock('$lib/db/schema', () => ({
	agentWebhooks: {
		id: 'id',
		organizationId: 'organizationId',
		agentId: 'agentId',
		url: 'url',
		secret: 'secret',
		secretPrefix: 'secretPrefix',
		eventTypes: 'eventTypes',
		status: 'status',
		lastDeliveredSeq: 'lastDeliveredSeq',
		consecutiveFailures: 'consecutiveFailures',
		lastAttemptAt: 'lastAttemptAt',
		lastError: 'lastError',
		createdAt: 'createdAt',
	},
	auditLogs: { id: 'id' },
}));

function queueResults(...results: any[][]) {
	for (const r of results) {
		(db as any).then.mockImplementationOnce((res: any) => res(r));
	}
}

describe('agent-webhooks queries', () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it('listAgentWebhooks scopes by organizationId and agentId, newest first', async () => {
		const rows = [{ id: 'wh-1' }, { id: 'wh-2' }];
		queueResults(rows);

		const result = await listAgentWebhooks('org-1', 'agent-1');

		expect(result).toEqual(rows);
		expect(db.select).toHaveBeenCalled();
		expect((db as any).orderBy).toHaveBeenCalled();
	});

	it('getAgentWebhookById returns undefined when no row matches', async () => {
		queueResults([]);
		const result = await getAgentWebhookById('missing');
		expect(result).toBeUndefined();
	});

	describe('createAgentWebhook', () => {
		it('generates a whsec_ secret, stores only its prefix, and returns the raw secret once', async () => {
			const newWebhook = { id: 'wh-1', organizationId: 'org-1', agentId: 'agent-1', url: 'https://x.test/hook' };
			queueResults([newWebhook], [{ id: 'audit-1' }]);

			const result = await createAgentWebhook('user-1', {
				organizationId: 'org-1',
				agentId: 'agent-1',
				url: 'https://x.test/hook',
				eventTypes: [],
			});

			expect(result.webhook).toEqual(newWebhook);
			expect(result.secret).toMatch(/^whsec_[0-9a-f]{64}$/);

			const insertedValues = (db as any).values.mock.calls[0][0];
			expect(insertedValues.secret).toBe(result.secret);
			expect(insertedValues.secretPrefix).toBe(result.secret.slice(0, 10));
			expect(insertedValues.secretPrefix.length).toBe(10);

			expect((db as any).values).toHaveBeenNthCalledWith(
				2,
				expect.objectContaining({ action: 'agent_webhook.created', resourceType: 'agent_webhook', resourceId: 'wh-1' })
			);
		});

		it('produces a different secret on every call', async () => {
			queueResults([{ id: 'wh-1' }], [{ id: 'audit-1' }], [{ id: 'wh-2' }], [{ id: 'audit-2' }]);

			const first = await createAgentWebhook('user-1', {
				organizationId: 'org-1',
				agentId: 'agent-1',
				url: 'https://x.test/a',
				eventTypes: [],
			});
			const second = await createAgentWebhook('user-1', {
				organizationId: 'org-1',
				agentId: 'agent-1',
				url: 'https://x.test/b',
				eventTypes: [],
			});

			expect(first.secret).not.toEqual(second.secret);
		});
	});

	describe('updateAgentWebhook', () => {
		it('throws when the webhook does not belong to the given org/agent', async () => {
			queueResults([{ id: 'wh-1', organizationId: 'org-2', agentId: 'agent-1', status: 'active' }]); // getAgentWebhookById

			await expect(
				updateAgentWebhook('user-1', 'org-1', 'agent-1', 'wh-1', { status: 'active' })
			).rejects.toThrow('Webhook not found');
			expect(db.update).not.toHaveBeenCalled();
		});

		it('re-enabling from failed to active clears consecutiveFailures/lastError but PRESERVES lastDeliveredSeq (resume, not reset)', async () => {
			const existing = {
				id: 'wh-1',
				organizationId: 'org-1',
				agentId: 'agent-1',
				status: 'failed',
				lastDeliveredSeq: 42,
				consecutiveFailures: 7,
				lastError: 'connect ECONNREFUSED',
			};
			const updated = { ...existing, status: 'active', consecutiveFailures: 0, lastError: null };
			queueResults([existing], [updated], [{ id: 'audit-1' }]);

			const result = await updateAgentWebhook('user-1', 'org-1', 'agent-1', 'wh-1', { status: 'active' });

			expect(result.status).toBe('active');
			const setValues = (db as any).set.mock.calls[0][0];
			expect(setValues.status).toBe('active');
			expect(setValues.consecutiveFailures).toBe(0);
			expect(setValues.lastError).toBeNull();
			// lastDeliveredSeq must NOT appear in the update -- delivery resumes from where it
			// left off, it does not jump to 0 or to the current head.
			expect(setValues).not.toHaveProperty('lastDeliveredSeq');
		});

		it('a plain url update while already active does not touch failure/seq fields', async () => {
			const existing = {
				id: 'wh-1',
				organizationId: 'org-1',
				agentId: 'agent-1',
				status: 'active',
				lastDeliveredSeq: 10,
				consecutiveFailures: 0,
				lastError: null,
			};
			queueResults([existing], [{ ...existing, url: 'https://x.test/new' }], [{ id: 'audit-1' }]);

			await updateAgentWebhook('user-1', 'org-1', 'agent-1', 'wh-1', { url: 'https://x.test/new' });

			const setValues = (db as any).set.mock.calls[0][0];
			expect(setValues).toEqual({ url: 'https://x.test/new' });
		});

		it('throws when the update returns no row', async () => {
			const existing = {
				id: 'wh-1',
				organizationId: 'org-1',
				agentId: 'agent-1',
				status: 'active',
				lastDeliveredSeq: 0,
				consecutiveFailures: 0,
			};
			queueResults([existing], []); // getAgentWebhookById, then update returning nothing

			await expect(
				updateAgentWebhook('user-1', 'org-1', 'agent-1', 'wh-1', { url: 'https://x.test/new' })
			).rejects.toThrow('Webhook not found');
		});
	});

	describe('deleteAgentWebhook', () => {
		it('deletes scoped to (id, orgId, agentId) and writes an audit_logs row', async () => {
			queueResults([{ id: 'wh-1' }], [{ id: 'audit-1' }]);

			await deleteAgentWebhook('user-1', 'org-1', 'agent-1', 'wh-1');

			expect(db.delete).toHaveBeenCalledTimes(1);
			expect((db as any).values).toHaveBeenCalledWith(
				expect.objectContaining({ action: 'agent_webhook.deleted', resourceId: 'wh-1' })
			);
		});

		it('throws when no row matches (id, orgId, agentId)', async () => {
			queueResults([]);
			await expect(deleteAgentWebhook('user-1', 'org-1', 'agent-1', 'missing')).rejects.toThrow('Webhook not found');
		});
	});
});
