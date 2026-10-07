import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// N10 part 2: the committed proof of the store's two central claims, against a REAL, freshly
// migrated Postgres (never the shared dev database -- disposable-DB contract, same as
// events.claim-state.integration.test.ts):
//   1. NO PLAINTEXT AT REST -- read the raw repo_credentials row over the wire and assert the
//      secret string appears nowhere in it.
//   2. A REVOKED CREDENTIAL IS NO LONGER SERVED -- and its ciphertext is destroyed in place.
// Skips if nothing answers DATABASE_URL; set N10_CREDENTIALS_INTEGRATION_REQUIRED=1 to make
// "no reachable database" a hard failure (CI posture).
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.N10_CREDENTIALS_INTEGRATION_REQUIRED === '1';

// The probe requires the repo_credentials TABLE, not just a listening Postgres: the default
// connection string can resolve to the shared dev database, which may predate migration
// 1723900000 -- and this suite must skip there rather than fail (and rather than litter a
// database it does not own with half-seeded state).
let dbReachable = false;
const probeSql = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	await probeSql`select 1 from repo_credentials limit 1`;
	dbReachable = true;
} catch {
	dbReachable = false;
} finally {
	await probeSql.end({ timeout: 1 }).catch(() => {});
}

if (required && !dbReachable) {
	throw new Error(
		`N10_CREDENTIALS_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}. ` +
			'This test is the committed proof that repo credentials are never stored in plaintext; ' +
			'skipping it silently would leave that claim unverified.'
	);
}

const suffix = randomUUID().slice(0, 8);
// 1724300000: the delivery path is scoped by the agent's project grants. This agent holds none, so
// it sees org-wide credentials only — which is exactly what the credential below is (project_id
// NULL), and is the pre-migration behaviour these cases were written to pin. No agents row is
// needed: the grant lookup is a plain read that returns nothing for an unknown id.
const agentIdForScope = randomUUID();
const SECRET_TOKEN = 'ghp_plaintext_must_never_rest_' + suffix;

process.env.SENTINEL_ENCRYPTION_KEY ??= Buffer.alloc(32, 11).toString('base64');

describe.skipIf(!dbReachable)('repo credentials at rest (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;
	// Each case seeds its OWN organization and credential: under `--sequence.shuffle` (B13) vitest
	// reorders the cases of a file too, and these used to be one chain sharing a `credentialId` that
	// only the first case set -- "served" ran before "stored" and read nothing.
	const orgIds: string[] = [];

	async function seedActiveCredential(): Promise<{ orgId: string; credentialId: string }> {
		const orgId = randomUUID();
		orgIds.push(orgId);
		const slug = 'n10-cred-' + suffix + '-' + orgIds.length;
		await sql`insert into organizations (id, name, slug) values (${orgId}, ${slug}, ${slug})`;
		const { createRepoCredential } = await import('./repo-credentials');
		const created = await createRepoCredential('user-int', {
			orgId,
			provider: 'github',
			label: 'integration cred',
			secret: { token: SECRET_TOKEN },
		});
		return { orgId, credentialId: created.id };
	}

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			for (const orgId of orgIds) {
				await sql`delete from audit_logs where resource_type = 'repo_credential' and metadata->>'orgId' = ${orgId}`;
				await sql`delete from repo_credentials where organization_id = ${orgId}`;
				await sql`delete from organizations where id = ${orgId}`;
			}
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	it('stores only ciphertext: the raw row never contains the secret', async () => {
		const { credentialId } = await seedActiveCredential();

		const [raw] = await sql`select * from repo_credentials where id = ${credentialId}`;
		expect(raw).toBeTruthy();
		expect(JSON.stringify(raw)).not.toContain(SECRET_TOKEN);
		expect(raw.encrypted_secret.length).toBeGreaterThan(0);
		expect(raw.nonce.length).toBeGreaterThan(0);
		expect(raw.key_version).toBe(1);
	});

	it('serves the decrypted secret to the delivery path while active', async () => {
		const { orgId, credentialId } = await seedActiveCredential();
		const { fetchDecryptedCredentialsForAgent } = await import('./repo-credentials');
		const served = await fetchDecryptedCredentialsForAgent(orgId, agentIdForScope);
		expect(served).toHaveLength(1);
		expect(served[0].secret).toEqual({ token: SECRET_TOKEN });

		const [raw] = await sql`select last_fetched_at from repo_credentials where id = ${credentialId}`;
		expect(raw.last_fetched_at).not.toBeNull();
	});

	it('a revoked credential is no longer served and its ciphertext is destroyed', async () => {
		const { orgId, credentialId } = await seedActiveCredential();
		const { revokeRepoCredential, fetchDecryptedCredentialsForAgent } = await import(
			'./repo-credentials'
		);
		// Served while active, so "no longer served" below is a change and not a credential that never was.
		expect(await fetchDecryptedCredentialsForAgent(orgId, agentIdForScope)).toHaveLength(1);
		await revokeRepoCredential('user-int', orgId, credentialId);

		const served = await fetchDecryptedCredentialsForAgent(orgId, agentIdForScope);
		expect(served).toHaveLength(0);

		const [raw] = await sql`select * from repo_credentials where id = ${credentialId}`;
		expect(raw.status).toBe('revoked');
		expect(raw.encrypted_secret).toBe('');
		expect(raw.nonce).toBe('');
		expect(raw.revoked_at).not.toBeNull();
	});

	it('mutations wrote audit rows without secret material', async () => {
		const { orgId, credentialId } = await seedActiveCredential();
		const { revokeRepoCredential } = await import('./repo-credentials');
		await revokeRepoCredential('user-int', orgId, credentialId);

		const rows = await sql`
			select action, metadata from audit_logs
			where resource_type = 'repo_credential' and resource_id = ${credentialId}
		`;
		const actions = rows.map((r) => r.action);
		expect(actions).toContain('repo_credential.created');
		expect(actions).toContain('repo_credential.revoked');
		expect(JSON.stringify(rows)).not.toContain(SECRET_TOKEN);
	});
});
