import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * Manual Issues M5 stage 2 (design §6, Q5) -- unit tests for agent-audit.ts. Asserts the actual
 * call into `db.insert(auditLogs).values(...)`, per this repo's convention (B10 addendum: "a mock
 * that returns itself for every call cannot distinguish did-the-right-thing from did-nothing").
 */

function makeChainable() {
	const m: any = {};
	m.insert = vi.fn(() => m);
	m.values = vi.fn(() => Promise.resolve(undefined));
	return m;
}

const dbMock = makeChainable();
vi.mock('$lib/server/db', () => ({ db: dbMock }));
vi.mock('$lib/db/schema', () => ({ auditLogs: { id: 'id' } }));
const logError = vi.fn();
vi.mock('$lib/server/observability/log', () => ({ log: { error: logError } }));

const { writeAgentAuditLog } = await import('./agent-audit');

beforeEach(() => {
	vi.clearAllMocks();
});

const ctx = {
	agentId: 'agent-1',
	organizationId: 'org-1',
	agentName: 'AutoFix',
	keyPrefixForAudit: 'abc123def456',
	keyId: 'key-1',
	keyPrefix: 'sent_agent_',
	keyExpiresAt: null,
};

describe('writeAgentAuditLog', () => {
	it('inserts an audit_logs row with actorId=agentId and metadata including the key prefix', async () => {
		await writeAgentAuditLog(ctx, 'agent.issue.claimed', 'issue', 'issue-1', { foo: 'bar' });

		expect(dbMock.insert).toHaveBeenCalledWith({ id: 'id' });
		expect(dbMock.values).toHaveBeenCalledWith(
			expect.objectContaining({
				action: 'agent.issue.claimed',
				resourceType: 'issue',
				resourceId: 'issue-1',
				actorId: 'agent-1',
				metadata: expect.objectContaining({
					agentName: 'AutoFix',
					keyPrefix: 'abc123def456',
					organizationId: 'org-1',
					foo: 'bar',
				}),
			})
		);
	});

	it('logs, but does not throw, when the insert fails', async () => {
		dbMock.values.mockImplementationOnce(() => Promise.reject(new Error('db down')));

		await expect(writeAgentAuditLog(ctx, 'agent.issue.claimed', 'issue', 'issue-1')).resolves.toBeUndefined();
		expect(logError).toHaveBeenCalledWith(
			'agent.audit_write_failed',
			expect.objectContaining({ action: 'agent.issue.claimed', agentId: 'agent-1' })
		);
	});
});
