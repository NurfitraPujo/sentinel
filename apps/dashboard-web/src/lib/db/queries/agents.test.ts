import { describe, it, expect, vi, beforeEach } from 'vitest';
import { listAgents, getAgentById, createAgent, setAgentStatus } from './agents';
import { db } from '../../server/db';

vi.mock('../../server/db', () => {
	const dbMock = {
		select: vi.fn(),
		from: vi.fn(),
		where: vi.fn(),
		orderBy: vi.fn(),
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
	dbMock.orderBy.mockReturnValue(dbMock);
	dbMock.insert.mockReturnValue(dbMock);
	dbMock.values.mockReturnValue(dbMock);
	dbMock.returning.mockReturnValue(dbMock);
	dbMock.update.mockReturnValue(dbMock);
	dbMock.set.mockReturnValue(dbMock);
	// See apikeys.test.ts's identical comment: mockReset (not clearAllMocks) drops queued
	// mockImplementationOnce entries so they cannot leak into the next test.
	dbMock.then.mockReset();
	dbMock.then.mockImplementation((resolve: any) => resolve([]));

	return { db: dbMock };
});

vi.mock('../schema', () => ({
	agents: {
		id: 'id',
		orgId: 'orgId',
		name: 'name',
		kind: 'kind',
		status: 'status',
		createdBy: 'createdBy',
		createdAt: 'createdAt',
	},
	auditLogs: {
		id: 'id',
	},
}));

describe('agents queries', () => {
	beforeEach(() => {
		vi.clearAllMocks();
	});

	it('listAgents returns rows scoped to orgId, newest first', async () => {
		const mockAgents = [{ id: 'a1' }, { id: 'a2' }];
		(db as any).then.mockImplementationOnce((res: any) => res(mockAgents));

		const result = await listAgents('org-1');

		expect(result).toEqual(mockAgents);
		expect(db.select).toHaveBeenCalled();
		expect((db as any).where).toHaveBeenCalled();
		expect((db as any).orderBy).toHaveBeenCalled();
	});

	it('getAgentById returns undefined when no row matches', async () => {
		(db as any).then.mockImplementationOnce((res: any) => res([]));

		const result = await getAgentById('missing');

		expect(result).toBeUndefined();
	});

	it('createAgent inserts the agent and writes an audit_logs row', async () => {
		const newAgent = { id: 'new-agent', orgId: 'org-1', name: 'AutoFix', kind: 'ai', status: 'active' };
		(db as any).then.mockImplementationOnce((res: any) => res([newAgent])); // insert returning
		(db as any).then.mockImplementationOnce((res: any) => res([{ id: 'audit-1' }])); // audit log

		const result = await createAgent('user-1', { orgId: 'org-1', name: 'AutoFix', kind: 'ai' });

		expect(result).toEqual(newAgent);
		expect(db.insert).toHaveBeenCalledTimes(2);
		expect((db as any).values).toHaveBeenNthCalledWith(
			2,
			expect.objectContaining({ action: 'agent.created', resourceType: 'agent', resourceId: 'new-agent' })
		);
	});

	it('setAgentStatus updates status scoped to (id, orgId) and writes an audit_logs row', async () => {
		const updated = { id: 'agent-1', orgId: 'org-1', status: 'disabled' };
		(db as any).then.mockImplementationOnce((res: any) => res([updated])); // update returning
		(db as any).then.mockImplementationOnce((res: any) => res([{ id: 'audit-2' }])); // audit log

		const result = await setAgentStatus('user-1', 'org-1', 'agent-1', 'disabled');

		expect(result).toEqual(updated);
		expect(db.update).toHaveBeenCalledTimes(1);
		expect((db as any).set).toHaveBeenCalledWith({ status: 'disabled' });
		expect((db as any).values).toHaveBeenCalledWith(
			expect.objectContaining({ action: 'agent.status_changed', resourceId: 'agent-1' })
		);
	});

	it('setAgentStatus throws when no row matches (id, orgId)', async () => {
		(db as any).then.mockImplementationOnce((res: any) => res([])); // update returning, no rows

		await expect(setAgentStatus('user-1', 'org-1', 'missing', 'disabled')).rejects.toThrow('Agent not found');
	});
});
