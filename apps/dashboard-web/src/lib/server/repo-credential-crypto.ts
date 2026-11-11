import crypto from 'crypto';
import { env } from '$env/dynamic/private';

/**
 * N10 part 2 (docs/plans/AGENT_WORKER_PLAN.md §4.5 "Git credentials are server-side too"):
 * AES-256-GCM encryption for repo_credentials.encrypted_secret under the SENTINEL_ENCRYPTION_KEY
 * master key. Deliberately NOT settings.ts's aes-256-cbc helper (no auth tag, hardcoded fallback
 * key) and NOT agent_webhooks' plaintext pattern -- these credentials authorize repository
 * WRITES, so tampering and at-rest exposure are both in scope:
 *
 *  - GCM (authenticated): a flipped ciphertext bit fails decryption instead of yielding garbage
 *    that might get pushed to a git remote as a "credential".
 *  - The organization id is bound as AAD, so a ciphertext row copied across tenants by SQL-level
 *    tampering will not decrypt.
 *  - There is NO fallback key. Missing/malformed SENTINEL_ENCRYPTION_KEY throws
 *    EncryptionKeyUnavailableError; callers map it to 503 and refuse to store OR serve.
 *  - Per-row random 96-bit nonce; `keyVersion` names which master key encrypted the row so the
 *    key can be rotated (add the old key to a version map, re-encrypt lazily) without a
 *    stop-the-world migration.
 *
 * The plaintext is always the JSON encoding of RepoCredentialSecret. It must never be logged,
 * never appear in an audit row, and never be returned to a dashboard client after initial set.
 */

export const CURRENT_KEY_VERSION = 1;

export class EncryptionKeyUnavailableError extends Error {
	constructor(detail: string) {
		super(`SENTINEL_ENCRYPTION_KEY unavailable: ${detail}`);
		this.name = 'EncryptionKeyUnavailableError';
	}
}

export type RepoCredentialSecret =
	| { token: string }
	| { username: string; appPassword: string };

// Read lazily (not at module load) so the server can boot without the key and only the
// credential paths fail closed -- and so tests can vary the env per case.
function masterKey(version: number): Buffer {
	if (version !== CURRENT_KEY_VERSION) {
		// Future rotation: map old versions to SENTINEL_ENCRYPTION_KEY_V<n> here.
		throw new EncryptionKeyUnavailableError(`unknown key version ${version}`);
	}
	const raw = env.SENTINEL_ENCRYPTION_KEY;
	if (!raw) {
		throw new EncryptionKeyUnavailableError('not set');
	}
	let key: Buffer;
	try {
		key = Buffer.from(raw, 'base64');
	} catch {
		throw new EncryptionKeyUnavailableError('not valid base64');
	}
	if (key.length !== 32) {
		throw new EncryptionKeyUnavailableError(
			`must be 32 bytes after base64 decoding, got ${key.length}`
		);
	}
	return key;
}

/** True when the master key is present and well-formed; used for fail-closed route guards. */
export function isEncryptionKeyAvailable(): boolean {
	try {
		masterKey(CURRENT_KEY_VERSION);
		return true;
	} catch {
		return false;
	}
}

export interface EncryptedSecret {
	/** base64(ciphertext || 16-byte GCM auth tag) */
	encryptedSecret: string;
	/** base64 of the 12-byte random nonce */
	nonce: string;
	keyVersion: number;
}

export function encryptRepoCredentialSecret(
	organizationId: string,
	secret: RepoCredentialSecret
): EncryptedSecret {
	const key = masterKey(CURRENT_KEY_VERSION);
	const nonce = crypto.randomBytes(12);
	const cipher = crypto.createCipheriv('aes-256-gcm', key, nonce);
	cipher.setAAD(Buffer.from(organizationId, 'utf8'));
	const plaintext = Buffer.from(JSON.stringify(secret), 'utf8');
	const ciphertext = Buffer.concat([cipher.update(plaintext), cipher.final(), cipher.getAuthTag()]);
	return {
		encryptedSecret: ciphertext.toString('base64'),
		nonce: nonce.toString('base64'),
		keyVersion: CURRENT_KEY_VERSION,
	};
}

export function decryptRepoCredentialSecret(
	organizationId: string,
	row: EncryptedSecret
): RepoCredentialSecret {
	const key = masterKey(row.keyVersion);
	const data = Buffer.from(row.encryptedSecret, 'base64');
	if (data.length < 17) {
		throw new Error('repo credential ciphertext malformed');
	}
	const tag = data.subarray(data.length - 16);
	const ciphertext = data.subarray(0, data.length - 16);
	const decipher = crypto.createDecipheriv(
		'aes-256-gcm',
		key,
		Buffer.from(row.nonce, 'base64')
	);
	decipher.setAAD(Buffer.from(organizationId, 'utf8'));
	decipher.setAuthTag(tag);
	const plaintext = Buffer.concat([decipher.update(ciphertext), decipher.final()]);
	return JSON.parse(plaintext.toString('utf8')) as RepoCredentialSecret;
}
