import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * N10 part 2: org repo-credentials management routes. Write-only contract: no response body from
 * any handler ever contains the submitted secret. RBAC: manage_agents (owner/admin) only.
 */

const listRepoCredentials = vi.fn();
const createRepoCredential = vi.fn();
const replaceRepoCredentialSecret = vi.fn();
const revokeRepoCredential = vi.fn();
vi.mock('$lib/db/queries/repo-credentials', () => ({
	listRepoCredentials,
	createRepoCredential,
	replaceRepoCredentialSecret,
	revokeRepoCredential,
}));

const requireOrgMembership = vi.fn();
vi.mock('../keys/_shared', () => ({ requireOrgMembership }));
vi.mock('../../keys/_shared', () => ({ requireOrgMembership }));

const { GET, POST } = await import('./+server');
const { PUT, DELETE } = await import('./[credentialId]/+server');

const METADATA_ROW = {
	id: 'cred-1',
	organizationId: 'org-1',
	provider: 'github',
	label: 'CI bot',
	secretPrefix: 'ghp_1234',
	status: 'active',
	createdBy: 'user-1',
	createdAt: null,
	revokedAt: null,
	lastFetchedAt: null,
};

function makeEvent(role: string | null, body?: unknown, credentialId?: string) {
	requireOrgMembership.mockResolvedValue(role ? { role } : null);
	return {
		params: { orgId: 'org-1', ...(credentialId ? { credentialId } : {}) },
		request: new Request('http://localhost/api/organizations/org-1/repo-credentials', {
			method: body ? 'POST' : 'GET',
			...(body ? { body: JSON.stringify(body), headers: { 'content-type': 'application/json' } } : {}),
		}),
		locals: { auth: vi.fn().mockResolvedValue({ user: { id: 'user-1' } }) },
	} as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	listRepoCredentials.mockResolvedValue([METADATA_ROW]);
	createRepoCredential.mockResolvedValue(METADATA_ROW);
	replaceRepoCredentialSecret.mockResolvedValue(METADATA_ROW);
	revokeRepoCredential.mockResolvedValue({ ...METADATA_ROW, status: 'revoked' });
});

describe('RBAC', () => {
	it('403s members without manage_agents on every handler', async () => {
		for (const [handler, ev] of [
			[GET, makeEvent('member')],
			[POST, makeEvent('member', { provider: 'github', label: 'x', token: 't' })],
			[PUT, makeEvent('member', { provider: 'github', token: 't' }, 'cred-1')],
			[DELETE, makeEvent('member', undefined, 'cred-1')],
		] as const) {
			await expect(handler(ev)).rejects.toMatchObject({ status: 403 });
		}
		expect(createRepoCredential).not.toHaveBeenCalled();
		expect(revokeRepoCredential).not.toHaveBeenCalled();
	});

	it('401s unauthenticated sessions', async () => {
		const ev = makeEvent('admin');
		ev.locals.auth.mockResolvedValue(null);
		await expect(GET(ev)).rejects.toMatchObject({ status: 401 });
	});
});

describe('POST create', () => {
	it('creates for an admin and NEVER echoes the secret back', async () => {
		const res = await POST(
			makeEvent('admin', { provider: 'github', label: 'CI bot', token: 'ghp_supersecret' })
		);
		expect(res.status).toBe(201);
		const text = await res.text();
		expect(text).not.toContain('ghp_supersecret');
		expect(createRepoCredential).toHaveBeenCalledWith('user-1', {
			orgId: 'org-1',
			provider: 'github',
			label: 'CI bot',
			secret: { token: 'ghp_supersecret' },
		});
	});

	it('accepts a bitbucket username+appPassword pair', async () => {
		await POST(
			makeEvent('owner', {
				provider: 'bitbucket',
				label: 'BB',
				username: 'devbot',
				appPassword: 'pw',
			})
		);
		expect(createRepoCredential).toHaveBeenCalledWith(
			'user-1',
			expect.objectContaining({ secret: { username: 'devbot', appPassword: 'pw' } })
		);
	});

	it('400s on a missing github token / invalid provider', async () => {
		await expect(
			POST(makeEvent('admin', { provider: 'github', label: 'x' }))
		).rejects.toMatchObject({ status: 400 });
		await expect(
			POST(makeEvent('admin', { provider: 'gitlab', label: 'x', token: 't' }))
		).rejects.toMatchObject({ status: 400 });
		expect(createRepoCredential).not.toHaveBeenCalled();
	});
});

describe('PUT replace / DELETE revoke', () => {
	it('replaces without echoing the secret', async () => {
		const res = await PUT(makeEvent('admin', { provider: 'github', token: 'ghp_newsecret' }, 'cred-1'));
		expect(await res.text()).not.toContain('ghp_newsecret');
		expect(replaceRepoCredentialSecret).toHaveBeenCalledWith('user-1', 'org-1', 'cred-1', {
			token: 'ghp_newsecret',
		});
	});

	it('404s a cross-org/unknown credential identically', async () => {
		revokeRepoCredential.mockRejectedValue(new Error('Credential not found'));
		await expect(DELETE(makeEvent('admin', undefined, 'cred-other'))).rejects.toMatchObject({
			status: 404,
		});
	});

	it('revokes for an admin', async () => {
		const res = await DELETE(makeEvent('admin', undefined, 'cred-1'));
		const body = await res.json();
		expect(body.credential.status).toBe('revoked');
		expect(revokeRepoCredential).toHaveBeenCalledWith('user-1', 'org-1', 'cred-1');
	});
});
