import { describe, it, expect, vi, beforeEach } from 'vitest';
import { getOrganizationApiKeys, createApiKey, rotateApiKey, revokeApiKey, type NatsPublisher } from './apikeys';
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
		expect(db.from).toHaveBeenCalled();
		expect(db.where).toHaveBeenCalled();
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
});
