import { describe, it, expect, vi, beforeEach } from 'vitest';

// R1b (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7f). POST /api/agent/key/rotate:
// self-rotation with a grace window (never immediate revoke, unlike the human rotateApiKey).

const mockEnv: Record<string, string | undefined> = {};
vi.mock('$env/dynamic/private', () => ({ env: mockEnv }));

const authenticateAgentRequest = vi.fn();
vi.mock('$lib/server/agent-auth', () => ({ authenticateAgentRequest }));

const rotateAgentKeyWithGrace = vi.fn();
class AgentKeyRotationError extends Error {}
vi.mock('$lib/db/queries/apikeys', () => ({ rotateAgentKeyWithGrace, AgentKeyRotationError }));

const writeAgentAuditLog = vi.fn();
vi.mock('$lib/server/agent-audit', () => ({ writeAgentAuditLog }));

const rotatePersonalKey = vi.fn();
class PersonalKeyRotationError extends Error {}
vi.mock('$lib/server/cli-personal-keys', () => ({ rotatePersonalKey, PersonalKeyRotationError }));

const { POST } = await import('./+server');

function makeEvent() {
	return { request: new Request('http://localhost/api/agent/key/rotate', { method: 'POST' }) } as any;
}

const ctx = {
	agentId: 'agent-1',
	organizationId: 'org-1',
	agentName: 'Triage Bot',
	keyPrefixForAudit: 'abc',
	keyId: 'key-old',
	keyPrefix: 'sent_agent_',
	keyExpiresAt: null,
};

beforeEach(() => {
	vi.clearAllMocks();
	delete mockEnv.AGENT_KEY_ROTATION_GRACE_HOURS;
	delete mockEnv.AGENT_KEY_ROTATION_DEFAULT_DAYS;
	authenticateAgentRequest.mockResolvedValue(ctx);
});

describe('POST /api/agent/key/rotate', () => {
	it('401s when unauthenticated', async () => {
		authenticateAgentRequest.mockRejectedValue(Object.assign(new Error('Unauthorized'), { status: 401 }));
		await expect(POST(makeEvent())).rejects.toMatchObject({ status: 401 });
		expect(rotateAgentKeyWithGrace).not.toHaveBeenCalled();
	});

	it('rotates the CALLING key only (ctx.keyId, never a request param), returns the new secret exactly once, and audits', async () => {
		const oldExpiresAt = new Date('2026-08-15T00:00:00.000Z');
		rotateAgentKeyWithGrace.mockResolvedValue({
			oldKey: { expiresAt: oldExpiresAt },
			newKey: { id: 'key-new', keyPrefix: 'sent_agent_' },
			secretToken: 'sent_agent_brandnewsecret',
		});

		const res = await POST(makeEvent());
		expect(res.status).toBe(200);
		const body = await res.json();

		expect(rotateAgentKeyWithGrace).toHaveBeenCalledWith('key-old', 24, null);
		expect(body).toEqual({
			success: true,
			oldKey: { id: 'key-old', expiresAt: '2026-08-15T00:00:00.000Z' },
			newKey: { id: 'key-new', prefix: 'sent_agent_', secret: 'sent_agent_brandnewsecret' },
		});

		// The response body carries the secret exactly once (no duplicate top-level field, no echo
		// under oldKey).
		expect(JSON.stringify(body).match(/sent_agent_brandnewsecret/g)).toHaveLength(1);

		expect(writeAgentAuditLog).toHaveBeenCalledWith(
			ctx,
			'agent.key.rotated',
			'api_key',
			'key-old',
			expect.objectContaining({ newKeyId: 'key-new', graceHours: 24 })
		);
	});

	it('honors AGENT_KEY_ROTATION_GRACE_HOURS from env, including 0 (immediate)', async () => {
		mockEnv.AGENT_KEY_ROTATION_GRACE_HOURS = '0';
		rotateAgentKeyWithGrace.mockResolvedValue({
			oldKey: { expiresAt: new Date() },
			newKey: { id: 'key-new', keyPrefix: 'sent_agent_' },
			secretToken: 'sent_agent_x',
		});

		await POST(makeEvent());
		expect(rotateAgentKeyWithGrace).toHaveBeenCalledWith('key-old', 0, null);
	});

	// N9 (C13): AGENT_KEY_ROTATION_DEFAULT_DAYS is parsed and passed through as the fallback
	// lifetime for the new key.
	it('passes AGENT_KEY_ROTATION_DEFAULT_DAYS through to rotateAgentKeyWithGrace', async () => {
		mockEnv.AGENT_KEY_ROTATION_DEFAULT_DAYS = '90';
		rotateAgentKeyWithGrace.mockResolvedValue({
			oldKey: { expiresAt: new Date() },
			newKey: { id: 'key-new', keyPrefix: 'sent_agent_' },
			secretToken: 'sent_agent_x',
		});

		await POST(makeEvent());
		expect(rotateAgentKeyWithGrace).toHaveBeenCalledWith('key-old', 24, 90);
	});

	it('500s when AGENT_KEY_ROTATION_DEFAULT_DAYS is misconfigured', async () => {
		mockEnv.AGENT_KEY_ROTATION_DEFAULT_DAYS = 'soon';
		await expect(POST(makeEvent())).rejects.toMatchObject({ status: 500 });
		expect(rotateAgentKeyWithGrace).not.toHaveBeenCalled();
	});

	it('maps AgentKeyRotationError to 400', async () => {
		rotateAgentKeyWithGrace.mockRejectedValue(new AgentKeyRotationError('not an agent key'));
		await expect(POST(makeEvent())).rejects.toMatchObject({ status: 400 });
		expect(writeAgentAuditLog).not.toHaveBeenCalled();
	});

	// sentinel-cli-browser-login: a PERSONAL key never takes the org-agent grace path. rotatePersonalKey (proved against a
	// real Postgres in personal-keys.integration.test.ts) revokes the calling key at once and keeps its exact expiry.
	describe('personal key', () => {
		const personalCtx = { ...ctx, owner: { userId: 'u-1', role: 'engineer', canWrite: true } };

		it('rotates via rotatePersonalKey with no grace window, even when AGENT_KEY_ROTATION_GRACE_HOURS is set', async () => {
			mockEnv.AGENT_KEY_ROTATION_GRACE_HOURS = '48';
			authenticateAgentRequest.mockResolvedValue(personalCtx);
			const revokedAt = new Date('2026-10-07T03:00:00.000Z');
			const expiresAt = new Date('2026-12-01T00:00:00.000Z');
			rotatePersonalKey.mockResolvedValue({
				oldKeyId: 'key-old',
				revokedAt,
				newKeyId: 'key-new',
				keyPrefix: 'sent_agent_',
				secretToken: 'sent_agent_personalsecret',
				expiresAt,
			});

			const res = await POST(makeEvent());
			expect(res.status).toBe(200);
			expect(rotatePersonalKey).toHaveBeenCalledWith('key-old');
			expect(rotateAgentKeyWithGrace).not.toHaveBeenCalled();
			// The old key's expiry is its revocation instant -- there is no grace window to report.
			expect(await res.json()).toEqual({
				success: true,
				oldKey: { id: 'key-old', expiresAt: revokedAt.toISOString() },
				newKey: { id: 'key-new', prefix: 'sent_agent_', secret: 'sent_agent_personalsecret' },
			});
			expect(writeAgentAuditLog).toHaveBeenCalledWith(
				personalCtx,
				'agent.key.rotated',
				'api_key',
				'key-old',
				expect.objectContaining({ newKeyId: 'key-new', graceHours: 0, personal: true, expiresAt: expiresAt.toISOString() })
			);
		});

		it('maps PersonalKeyRotationError (key no longer live) to 400 and audits nothing', async () => {
			authenticateAgentRequest.mockResolvedValue(personalCtx);
			rotatePersonalKey.mockRejectedValue(new PersonalKeyRotationError('This key is no longer active'));
			await expect(POST(makeEvent())).rejects.toMatchObject({ status: 400 });
			expect(writeAgentAuditLog).not.toHaveBeenCalled();
		});
	});
});
