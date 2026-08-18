import { describe, it, expect, vi, beforeEach } from 'vitest';

// N10 part 2: the crypto contract that the "no plaintext at rest" guarantee rests on.
const mockEnv: Record<string, string | undefined> = {};
vi.mock('$env/dynamic/private', () => ({ env: mockEnv }));

const KEY = Buffer.alloc(32, 7).toString('base64');
const OTHER_KEY = Buffer.alloc(32, 9).toString('base64');

const {
	encryptRepoCredentialSecret,
	decryptRepoCredentialSecret,
	isEncryptionKeyAvailable,
	EncryptionKeyUnavailableError,
	CURRENT_KEY_VERSION,
} = await import('./repo-credential-crypto');

describe('repo-credential-crypto', () => {
	beforeEach(() => {
		mockEnv.SENTINEL_ENCRYPTION_KEY = KEY;
	});

	it('round-trips a github token', () => {
		const enc = encryptRepoCredentialSecret('org-1', { token: 'ghp_supersecret123' });
		expect(decryptRepoCredentialSecret('org-1', enc)).toEqual({ token: 'ghp_supersecret123' });
	});

	it('round-trips a bitbucket username/app-password pair', () => {
		const enc = encryptRepoCredentialSecret('org-1', {
			username: 'devbot',
			appPassword: 'ATBBsecret',
		});
		expect(decryptRepoCredentialSecret('org-1', enc)).toEqual({
			username: 'devbot',
			appPassword: 'ATBBsecret',
		});
	});

	it('ciphertext does not contain the plaintext secret', () => {
		const enc = encryptRepoCredentialSecret('org-1', { token: 'ghp_supersecret123' });
		expect(enc.encryptedSecret).not.toContain('ghp_supersecret123');
		expect(Buffer.from(enc.encryptedSecret, 'base64').toString('utf8')).not.toContain(
			'ghp_supersecret123'
		);
		expect(enc.keyVersion).toBe(CURRENT_KEY_VERSION);
	});

	it('uses a fresh random nonce per encryption', () => {
		const a = encryptRepoCredentialSecret('org-1', { token: 't' });
		const b = encryptRepoCredentialSecret('org-1', { token: 't' });
		expect(a.nonce).not.toBe(b.nonce);
		expect(a.encryptedSecret).not.toBe(b.encryptedSecret);
	});

	it('refuses to decrypt under a different organization id (AAD binding)', () => {
		const enc = encryptRepoCredentialSecret('org-1', { token: 'secret' });
		expect(() => decryptRepoCredentialSecret('org-2', enc)).toThrow();
	});

	it('refuses to decrypt tampered ciphertext (auth tag)', () => {
		const enc = encryptRepoCredentialSecret('org-1', { token: 'secret' });
		const buf = Buffer.from(enc.encryptedSecret, 'base64');
		buf[0] ^= 0xff;
		expect(() =>
			decryptRepoCredentialSecret('org-1', { ...enc, encryptedSecret: buf.toString('base64') })
		).toThrow();
	});

	it('fails with the wrong master key', () => {
		const enc = encryptRepoCredentialSecret('org-1', { token: 'secret' });
		mockEnv.SENTINEL_ENCRYPTION_KEY = OTHER_KEY;
		expect(() => decryptRepoCredentialSecret('org-1', enc)).toThrow();
	});

	it('throws EncryptionKeyUnavailableError when the key is unset', () => {
		delete mockEnv.SENTINEL_ENCRYPTION_KEY;
		expect(() => encryptRepoCredentialSecret('org-1', { token: 'x' })).toThrow(
			EncryptionKeyUnavailableError
		);
		expect(isEncryptionKeyAvailable()).toBe(false);
	});

	it('rejects a key that is not 32 bytes', () => {
		mockEnv.SENTINEL_ENCRYPTION_KEY = Buffer.alloc(16, 1).toString('base64');
		expect(() => encryptRepoCredentialSecret('org-1', { token: 'x' })).toThrow(
			EncryptionKeyUnavailableError
		);
		expect(isEncryptionKeyAvailable()).toBe(false);
	});

	it('rejects an unknown key version on decrypt', () => {
		const enc = encryptRepoCredentialSecret('org-1', { token: 'x' });
		expect(() => decryptRepoCredentialSecret('org-1', { ...enc, keyVersion: 99 })).toThrow(
			EncryptionKeyUnavailableError
		);
	});
});
