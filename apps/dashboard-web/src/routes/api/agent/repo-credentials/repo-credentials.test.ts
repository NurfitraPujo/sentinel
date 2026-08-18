import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * N10 part 2: GET /api/agent/repo-credentials -- the scoped delivery endpoint. The red-first
 * security assertions live here: a plain agent key (flag false) gets 403 and NO decryption is
 * even attempted; every served credential writes an audit row; a missing encryption key is a
 * 503, not a serve-what-we-can.
 */

const authenticateAgentRequest = vi.fn();
vi.mock('$lib/server/agent-auth', () => ({ authenticateAgentRequest }));

const writeAgentAuditLog = vi.fn().mockResolvedValue(undefined);
vi.mock('$lib/server/agent-audit', () => ({ writeAgentAuditLog }));

const fetchDecryptedCredentialsForAgent = vi.fn();
vi.mock('$lib/db/queries/repo-credentials', () => ({ fetchDecryptedCredentialsForAgent }));

const isEncryptionKeyAvailable = vi.fn();
vi.mock('$lib/server/repo-credential-crypto', () => ({ isEncryptionKeyAvailable }));

// db.select().from().where() -> agent flag row
let agentFlagRows: Array<{ canAccessRepoCredentials: boolean }> = [];
const dbMock: any = {};
dbMock.select = vi.fn(() => dbMock);
dbMock.from = vi.fn(() => dbMock);
dbMock.where = vi.fn(() => Promise.resolve(agentFlagRows));
vi.mock('$lib/server/db', () => ({ db: dbMock }));

const { GET } = await import('./+server');

const CTX = {
	agentId: 'agent-1',
	organizationId: 'org-1',
	agentName: 'worker',
	keyPrefixForAudit: 'abc123def456',
};

function makeEvent() {
	return { request: new Request('http://localhost/api/agent/repo-credentials') } as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	agentFlagRows = [{ canAccessRepoCredentials: true }];
	authenticateAgentRequest.mockResolvedValue(CTX);
	isEncryptionKeyAvailable.mockReturnValue(true);
	fetchDecryptedCredentialsForAgent.mockResolvedValue([]);
});

describe('GET /api/agent/repo-credentials', () => {
	it('401s when unauthenticated', async () => {
		authenticateAgentRequest.mockRejectedValue(
			Object.assign(new Error('Unauthorized'), { status: 401 })
		);
		await expect(GET(makeEvent())).rejects.toMatchObject({ status: 401 });
		expect(fetchDecryptedCredentialsForAgent).not.toHaveBeenCalled();
	});

	it('403s for a plain agent key without can_access_repo_credentials -- no decryption attempted', async () => {
		agentFlagRows = [{ canAccessRepoCredentials: false }];
		await expect(GET(makeEvent())).rejects.toMatchObject({ status: 403 });
		expect(fetchDecryptedCredentialsForAgent).not.toHaveBeenCalled();
		expect(writeAgentAuditLog).not.toHaveBeenCalled();
	});

	it('403s when the agent row is missing', async () => {
		agentFlagRows = [];
		await expect(GET(makeEvent())).rejects.toMatchObject({ status: 403 });
		expect(fetchDecryptedCredentialsForAgent).not.toHaveBeenCalled();
	});

	it('503s when the encryption key is not configured, even for a flagged agent', async () => {
		isEncryptionKeyAvailable.mockReturnValue(false);
		await expect(GET(makeEvent())).rejects.toMatchObject({ status: 503 });
		expect(fetchDecryptedCredentialsForAgent).not.toHaveBeenCalled();
	});

	it('serves decrypted credentials scoped by the credential org (B7) and audits each one', async () => {
		fetchDecryptedCredentialsForAgent.mockResolvedValue([
			{ id: 'cred-1', provider: 'github', label: 'CI bot', secret: { token: 'ghp_abc' } },
			{
				id: 'cred-2',
				provider: 'bitbucket',
				label: 'BB',
				secret: { username: 'u', appPassword: 'p' },
			},
		]);

		const res = await GET(makeEvent());
		expect(res.status).toBe(200);
		expect(res.headers.get('cache-control')).toBe('no-store');
		const body = await res.json();

		// Tenant scope came from the authenticated context, nowhere else.
		expect(fetchDecryptedCredentialsForAgent).toHaveBeenCalledWith('org-1');
		expect(body.credentials).toEqual([
			{ id: 'cred-1', provider: 'github', label: 'CI bot', secret: { token: 'ghp_abc' } },
			{
				id: 'cred-2',
				provider: 'bitbucket',
				label: 'BB',
				secret: { username: 'u', appPassword: 'p' },
			},
		]);

		// One audit row per served credential, carrying agent identity + credential id.
		expect(writeAgentAuditLog).toHaveBeenCalledTimes(2);
		expect(writeAgentAuditLog).toHaveBeenCalledWith(
			CTX,
			'agent.repo_credential_fetched',
			'repo_credential',
			'cred-1',
			expect.objectContaining({ provider: 'github', fetchedAt: expect.any(String) })
		);
		expect(writeAgentAuditLog).toHaveBeenCalledWith(
			CTX,
			'agent.repo_credential_fetched',
			'repo_credential',
			'cred-2',
			expect.objectContaining({ provider: 'bitbucket' })
		);
	});

	it('audit metadata never contains secret material', async () => {
		fetchDecryptedCredentialsForAgent.mockResolvedValue([
			{ id: 'cred-1', provider: 'github', label: 'CI', secret: { token: 'ghp_topsecret' } },
		]);
		await GET(makeEvent());
		const serialized = JSON.stringify(writeAgentAuditLog.mock.calls);
		expect(serialized).not.toContain('ghp_topsecret');
	});
});
