import { describe, it, expect, vi, beforeEach } from 'vitest';
import {
	getOrganizationApiKeys,
	createApiKey,
	rotateApiKey,
	revokeApiKey,
	rotateAgentKeyWithGrace,
	AgentKeyRotationError,
	type NatsPublisher,
} from './apikeys';
import { db } from '../../server/db';

vi.mock('../../server/db', () => {
	const dbMock = {
		select: vi.fn(),
		from: vi.fn(),
		where: vi.fn(),
		insert: vi.fn(),
		values: vi.fn(),
		returning: vi.fn(),
		update: vi.fn(),
		set: vi.fn(),
		then: vi.fn(),
	};
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.insert.mockReturnValue(dbMock);
	dbMock.values.mockReturnValue(dbMock);
	dbMock.returning.mockReturnValue(dbMock);
	dbMock.update.mockReturnValue(dbMock);
	dbMock.set.mockReturnValue(dbMock);
	// `clearAllMocks` clears call records but NOT queued `mockImplementationOnce` entries, so an
	// un-consumed queued resolution leaks into the NEXT test and answers the wrong query, making
	// results order-dependent. `mockReset` on the queue-bearing mock drops the queue; the base
	// implementation is re-established on the next line.
	dbMock.then.mockReset();
	dbMock.then.mockImplementation((resolve) => resolve([]));

	return { db: dbMock };
});

vi.mock('../schema', () => ({
	projectApiKeys: {
		id: 'id',
		organizationId: 'organizationId',
		projectId: 'projectId',
		name: 'name',
		keyPrefix: 'keyPrefix',
		scope: 'scope',
		status: 'status',
		rateLimitRpm: 'rateLimitRpm',
		expiresAt: 'expiresAt',
		revokedAt: 'revokedAt',
		createdBy: 'createdBy',
		createdAt: 'createdAt',
		agentId: 'agentId',
	},
	auditLogs: {
		id: 'id',
	}
}));

describe('apikeys queries', () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it('getOrganizationApiKeys should return api keys for the org', async () => {
		const mockKeys = [{ id: 'key1' }, { id: 'key2' }];
		(db as any).then.mockImplementationOnce((res: any) => res(mockKeys));

		const result = await getOrganizationApiKeys('org-1');
		expect(result).toEqual(mockKeys);
		expect(db.select).toHaveBeenCalled();
		expect((db as any).from).toHaveBeenCalled();
		expect((db as any).where).toHaveBeenCalled();
	});

	it('createApiKey should insert key and audit log, then return raw token', async () => {
		const mockKey = { id: 'new-key-id', name: 'my-key', scope: 'ingest' };
		(db as any).then.mockImplementationOnce((res: any) => res([mockKey])); // returning
		(db as any).then.mockImplementationOnce((res: any) => res([{ id: 'audit-id' }])); // audit log

		const result = await createApiKey('user-1', {
			organizationId: 'org-1',
			projectId: 'proj-1',
			name: 'Test Key',
			scope: 'ingest',
		});

		expect(result.apiKey).toEqual(mockKey);
		expect(result.secretToken).toMatch(/^sent_live_[0-9a-f]{64}$/);
		
		expect(db.insert).toHaveBeenCalledTimes(2);
	});

	it('rotateApiKey should update existing and create new', async () => {
		const existingKey = { id: 'key-1', organizationId: 'org-1', projectId: 'proj-1', name: 'old', scope: 'ingest', rateLimitRpm: 100 };
		(db as any).then.mockImplementationOnce((res: any) => res([existingKey])); // select
		(db as any).then.mockImplementationOnce((res: any) => res(undefined)); // update
		const newKeyMock = { id: 'new-key-id', name: 'old', scope: 'ingest' };
		(db as any).then.mockImplementationOnce((res: any) => res([newKeyMock])); // create new returning
		(db as any).then.mockImplementationOnce((res: any) => res([{ id: 'audit-1' }])); // create new audit
		(db as any).then.mockImplementationOnce((res: any) => res([{ id: 'audit-2' }])); // rotate audit 

		const result = await rotateApiKey('user-1', 'key-1', '24h');

		expect(result.apiKey).toEqual(newKeyMock);
		expect(result.secretToken).toMatch(/^sent_live_[0-9a-f]{64}$/);
		expect(db.update).toHaveBeenCalledTimes(1);
		expect(db.insert).toHaveBeenCalledTimes(3);
	});

	it('revokeApiKey should update status and publish event', async () => {
		const revokedKey = { id: 'key-1', status: 'revoked' };
		(db as any).then.mockImplementationOnce((res: any) => res([revokedKey])); // update
		(db as any).then.mockImplementationOnce((res: any) => res([{ id: 'audit-id' }])); // audit

		const publisher: NatsPublisher = {
			publish: vi.fn().mockResolvedValue(undefined),
		};

		const result = await revokeApiKey('user-1', 'key-1', publisher);

		expect(result).toEqual(revokedKey);
		expect(db.update).toHaveBeenCalledTimes(1);
		expect(db.insert).toHaveBeenCalledTimes(1);
		expect(publisher.publish).toHaveBeenCalledWith('api_key.invalidated', { keyId: 'key-1' });
	});

	// M5 §7: agent-scoped keys reuse createApiKey but are always org-scoped (projectId forced to
	// null even if a projectId was passed) and carry agentId + the 'sent_agent_' prefix.
	it('createApiKey with scope "agent" forces projectId null and sets agentId', async () => {
		const mockKey = { id: 'agent-key-id', name: 'agent key', scope: 'agent', agentId: 'agent-1' };
		(db as any).then.mockImplementationOnce((res: any) => res([mockKey])); // returning
		(db as any).then.mockImplementationOnce((res: any) => res([{ id: 'audit-id' }])); // audit log

		const result = await createApiKey('user-1', {
			organizationId: 'org-1',
			projectId: 'proj-1', // deliberately supplied to prove it gets ignored
			name: 'agent key',
			scope: 'agent',
			agentId: 'agent-1',
		});

		expect(result.apiKey).toEqual(mockKey);
		expect(result.secretToken).toMatch(/^sent_agent_[0-9a-f]{64}$/);
		expect((db as any).values).toHaveBeenCalledWith(
			expect.objectContaining({ projectId: null, agentId: 'agent-1', scope: 'agent' })
		);
	});

	// R1b (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7f): self-rotation with a grace
	// window, deliberately NOT immediate revoke (see rotateAgentKeyWithGrace's doc comment for why
	// this is a separate function from rotateApiKey rather than a variant).
	describe('rotateAgentKeyWithGrace', () => {
		it('sets the OLD key expires_at to now + graceHours, keeps status untouched, and returns a fresh secret', async () => {
			const existingKey = {
				id: 'key-1',
				organizationId: 'org-1',
				projectId: null,
				name: 'agent key',
				scope: 'agent',
				rateLimitRpm: 5000,
				agentId: 'agent-1',
			};
			(db as any).then.mockImplementationOnce((res: any) => res([existingKey])); // select existing
			const updatedOldKey = { id: 'key-1', expiresAt: new Date('2026-08-15T00:00:00.000Z') };
			(db as any).then.mockImplementationOnce((res: any) => res([updatedOldKey])); // update .returning()
			const newKeyMock = { id: 'key-2', name: 'agent key', scope: 'agent', agentId: 'agent-1' };
			(db as any).then.mockImplementationOnce((res: any) => res([newKeyMock])); // createApiKey .returning()
			(db as any).then.mockImplementationOnce((res: any) => res([{ id: 'audit-1' }])); // createApiKey audit

			const before = Date.now();
			const result = await rotateAgentKeyWithGrace('key-1', 24);
			const after = Date.now();

			expect(result.oldKey).toEqual(updatedOldKey);
			expect(result.newKey).toEqual(newKeyMock);
			expect(result.secretToken).toMatch(/^sent_agent_[0-9a-f]{64}$/);

			// The update call set expires_at to roughly now + 24h -- assert the SET payload directly
			// rather than trusting `updatedOldKey`'s mocked .returning() value.
			const setCall = (db as any).set.mock.calls.find(
				(args: any[]) => args[0] && Object.prototype.hasOwnProperty.call(args[0], 'expiresAt')
			);
			expect(setCall).toBeDefined();
			const setAt = (setCall![0].expiresAt as Date).getTime();
			expect(setAt).toBeGreaterThanOrEqual(before + 24 * 60 * 60 * 1000 - 1000);
			expect(setAt).toBeLessThanOrEqual(after + 24 * 60 * 60 * 1000 + 1000);
			// Status is never touched by this path (unlike rotateApiKey's immediate revoke).
			expect(setCall![0]).not.toHaveProperty('status');
		});

		it('graceHours=0 sets expires_at to (approximately) now — immediate expiry', async () => {
			const existingKey = {
				id: 'key-1',
				organizationId: 'org-1',
				projectId: null,
				name: 'agent key',
				scope: 'agent',
				rateLimitRpm: 5000,
				agentId: 'agent-1',
			};
			(db as any).then.mockImplementationOnce((res: any) => res([existingKey]));
			(db as any).then.mockImplementationOnce((res: any) => res([{ id: 'key-1', expiresAt: new Date() }]));
			(db as any).then.mockImplementationOnce((res: any) => res([{ id: 'key-2' }]));
			(db as any).then.mockImplementationOnce((res: any) => res([{ id: 'audit-1' }]));

			const before = Date.now();
			await rotateAgentKeyWithGrace('key-1', 0);
			const after = Date.now();

			const setCall = (db as any).set.mock.calls.find(
				(args: any[]) => args[0] && Object.prototype.hasOwnProperty.call(args[0], 'expiresAt')
			);
			const setAt = (setCall![0].expiresAt as Date).getTime();
			expect(setAt).toBeGreaterThanOrEqual(before - 1000);
			expect(setAt).toBeLessThanOrEqual(after + 1000);
		});

		it('throws AgentKeyRotationError for an unknown key', async () => {
			(db as any).then.mockImplementationOnce((res: any) => res([])); // select: not found

			await expect(rotateAgentKeyWithGrace('missing', 24)).rejects.toBeInstanceOf(AgentKeyRotationError);
		});

		it('throws AgentKeyRotationError for a non-agent-scoped key', async () => {
			(db as any).then.mockImplementationOnce((res: any) =>
				res([{ id: 'key-1', scope: 'ingest', agentId: null, organizationId: 'org-1' }])
			);

			await expect(rotateAgentKeyWithGrace('key-1', 24)).rejects.toBeInstanceOf(AgentKeyRotationError);
		});
	});
});
