import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * N10 part 2: query layer. The load-bearing assertion is "no plaintext at rest" -- every value
 * handed to db.insert/db.update is captured raw and searched for the secret string. Real
 * AES-GCM runs underneath (only $env is mocked), so this fails if encryption is ever bypassed,
 * not just if a mock was miswired.
 */

const mockEnv: Record<string, string | undefined> = {
	SENTINEL_ENCRYPTION_KEY: Buffer.alloc(32, 5).toString('base64'),
};
vi.mock('$env/dynamic/private', () => ({ env: mockEnv }));

// Chainable db mock that CAPTURES insert/update payloads and returns canned rows.
let insertedValues: any[] = [];
let updateSets: any[] = [];
let selectRows: any[] = [];
let returningRow: any = {};

const dbMock: any = {};
dbMock.insert = vi.fn(() => dbMock);
dbMock.values = vi.fn((v: any) => {
	insertedValues.push(v);
	return dbMock;
});
dbMock.update = vi.fn(() => dbMock);
dbMock.set = vi.fn((v: any) => {
	updateSets.push(v);
	return dbMock;
});
dbMock.select = vi.fn(() => dbMock);
dbMock.from = vi.fn(() => dbMock);
dbMock.orderBy = vi.fn(() => Promise.resolve(selectRows));
dbMock.returning = vi.fn(() => Promise.resolve([returningRow]));
// where() is terminal for plain update (no returning) chains too, so keep it chainable AND
// thenable: drizzle builders are awaitable at any point.
dbMock.where = vi.fn(() => dbMock);
dbMock.then = (resolve: any, reject: any) => Promise.resolve(selectRows).then(resolve, reject);

vi.mock('$lib/server/db', () => ({ db: dbMock }));

const {
	createRepoCredential,
	replaceRepoCredentialSecret,
	revokeRepoCredential,
	fetchDecryptedCredentialsForAgent,
	secretPrefixFor,
} = await import('./repo-credentials');
const { encryptRepoCredentialSecret } = await import('$lib/server/repo-credential-crypto');

const SECRET_TOKEN = 'ghp_veryverysecrettoken';

beforeEach(() => {
	vi.clearAllMocks();
	insertedValues = [];
	updateSets = [];
	selectRows = [];
	returningRow = { id: 'cred-1', organizationId: 'org-1', provider: 'github', label: 'CI' };
	mockEnv.SENTINEL_ENCRYPTION_KEY = Buffer.alloc(32, 5).toString('base64');
});

describe('createRepoCredential', () => {
	it('stores NO plaintext at rest: raw insert payloads never contain the secret', async () => {
		await createRepoCredential('user-1', {
			orgId: 'org-1',
			provider: 'github',
			label: 'CI bot',
			secret: { token: SECRET_TOKEN },
		});

		expect(insertedValues.length).toBeGreaterThan(0);
		const raw = JSON.stringify(insertedValues);
		expect(raw).not.toContain(SECRET_TOKEN);
		// The row that was inserted is ciphertext + nonce + key version, not the token.
		const row = insertedValues[0];
		expect(row.encryptedSecret).toBeTruthy();
		expect(row.nonce).toBeTruthy();
		expect(row.keyVersion).toBe(1);
		// Prefix is the only fragment kept for display, and it is bounded.
		expect(row.secretPrefix).toBe(SECRET_TOKEN.slice(0, 8));
	});

	it('bitbucket username/appPassword pair: appPassword never at rest, prefix is the username', async () => {
		await createRepoCredential('user-1', {
			orgId: 'org-1',
			provider: 'bitbucket',
			label: 'BB',
			secret: { username: 'devbot', appPassword: 'ATBBsecretpw' },
		});
		const raw = JSON.stringify(insertedValues);
		expect(raw).not.toContain('ATBBsecretpw');
		expect(insertedValues[0].secretPrefix).toBe('devbot');
	});

	it('writes a repo_credential.created audit row without secret material', async () => {
		await createRepoCredential('user-1', {
			orgId: 'org-1',
			provider: 'github',
			label: 'CI bot',
			secret: { token: SECRET_TOKEN },
		});
		const audit = insertedValues.find((v) => v.action === 'repo_credential.created');
		expect(audit).toMatchObject({
			resourceType: 'repo_credential',
			resourceId: 'cred-1',
			actorId: 'user-1',
		});
		expect(JSON.stringify(audit)).not.toContain(SECRET_TOKEN);
	});

	it('refuses to store when the encryption key is missing (no insert happens)', async () => {
		delete mockEnv.SENTINEL_ENCRYPTION_KEY;
		await expect(
			createRepoCredential('user-1', {
				orgId: 'org-1',
				provider: 'github',
				label: 'CI',
				secret: { token: SECRET_TOKEN },
			})
		).rejects.toThrow('SENTINEL_ENCRYPTION_KEY');
		expect(dbMock.insert).not.toHaveBeenCalled();
	});
});

describe('replaceRepoCredentialSecret', () => {
	it('re-encrypts in place; update payload has no plaintext', async () => {
		await replaceRepoCredentialSecret('user-1', 'org-1', 'cred-1', { token: SECRET_TOKEN });
		expect(updateSets.length).toBe(1);
		expect(JSON.stringify(updateSets)).not.toContain(SECRET_TOKEN);
		expect(updateSets[0].encryptedSecret).toBeTruthy();
		const audit = insertedValues.find((v) => v.action === 'repo_credential.replaced');
		expect(audit).toBeTruthy();
	});
});

describe('revokeRepoCredential', () => {
	it('destroys ciphertext and marks revoked + audits', async () => {
		await revokeRepoCredential('user-1', 'org-1', 'cred-1');
		expect(updateSets[0]).toMatchObject({ status: 'revoked', encryptedSecret: '', nonce: '' });
		expect(updateSets[0].revokedAt).toBeInstanceOf(Date);
		const audit = insertedValues.find((v) => v.action === 'repo_credential.revoked');
		expect(audit).toMatchObject({ resourceId: 'cred-1', actorId: 'user-1' });
	});
});

describe('fetchDecryptedCredentialsForAgent', () => {
	it('decrypts real ciphertext back to the secret and stamps last_fetched_at', async () => {
		const enc = encryptRepoCredentialSecret('org-1', { token: SECRET_TOKEN });
		selectRows = [
			{
				id: 'cred-1',
				organizationId: 'org-1',
				provider: 'github',
				label: 'CI',
				encryptedSecret: enc.encryptedSecret,
				nonce: enc.nonce,
				keyVersion: enc.keyVersion,
				status: 'active',
			},
		];
		const out = await fetchDecryptedCredentialsForAgent('org-1');
		expect(out).toEqual([
			{ id: 'cred-1', provider: 'github', label: 'CI', secret: { token: SECRET_TOKEN } },
		]);
		expect(updateSets.some((s) => s.lastFetchedAt instanceof Date)).toBe(true);
	});
});

describe('secretPrefixFor', () => {
	it('bounds prefixes to the column width', () => {
		expect(secretPrefixFor({ token: 'ab' })).toBe('ab');
		expect(secretPrefixFor({ username: 'x'.repeat(40), appPassword: 'p' }).length).toBe(16);
	});
});
