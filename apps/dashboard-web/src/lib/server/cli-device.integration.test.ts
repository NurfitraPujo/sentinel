import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

/**
 * sentinel-cli-browser-login (docs/plans/CLI_BROWSER_LOGIN.md) -- the device authorization state machine against a REAL
 * migrated Postgres: start -> pending -> (slow_down) -> approve/deny -> single delivery, expiry, single-use approval,
 * membership re-checked at approval AND at delivery, the audit rows, and that nothing secret is ever stored.
 *
 * Same database contract as routes/api/agent/personal-keys.integration.test.ts (DATABASE_URL at a DISPOSABLE Postgres;
 * CLI_LOGIN_INTEGRATION_REQUIRED=1 turns "no database" into a failure).
 */
const connectionString = process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.CLI_LOGIN_INTEGRATION_REQUIRED === '1';

let dbReady = false;
const probe = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	const [row] = await probe`select to_regclass('cli_device_authorizations')::text as t`;
	dbReady = row?.t === 'cli_device_authorizations';
} catch {
	dbReady = false;
} finally {
	await probe.end({ timeout: 1 }).catch(() => {});
}
if (required && !dbReady) {
	throw new Error(`CLI_LOGIN_INTEGRATION_REQUIRED=1 but no migrated (>= 1725900000) database answered at ${connectionString}.`);
}

const suffix = randomUUID().slice(0, 8);
const org = randomUUID();
const otherOrg = randomUUID();
const alice = randomUUID();
const bob = randomUUID();

describe.skipIf(!dbReady)('CLI device authorization flow (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;
	let dev: typeof import('./cli-device');
	let auth: typeof import('./agent-auth');

	const start = (deviceName = 'laptop') =>
		dev.startDeviceAuthorization({
			client: 'sentinel-cli',
			clientVersion: 'v-test',
			deviceName,
			profile: 'production',
			ip: '203.0.113.7',
			userAgent: 'sentinel/v-test',
		});
	const approve = (userCode: string, userId = alice, organizationId: string | null = org) =>
		dev.decideDeviceAuthorization({ userCode, userId, organizationId, decision: 'approve', ip: '198.51.100.1' });
	// Polls are ≥ interval apart in real life; tests rewind last_polled_at instead of sleeping.
	const rewind = (userCode: string) =>
		sql`update cli_device_authorizations set last_polled_at = now() - interval '1 minute' where user_code = ${userCode}`;
	const bearer = (key: string) => new Request('http://localhost/api/agent/self', { headers: { authorization: `Bearer ${key}` } });

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		dev = await import('./cli-device');
		auth = await import('./agent-auth');
		for (const [id, label] of [[org, 'a'], [otherOrg, 'b']] as const) {
			await sql`insert into organizations (id, name, slug) values (${id}, ${`cbl-dev-${label}-${suffix}`}, ${`cbl-dev-${label}-${suffix}`})`;
		}
		await sql`insert into "user" (id, name, email) values (${alice}, 'Alice', ${`cbl-dev-alice-${suffix}@example.test`})`;
		await sql`insert into "user" (id, name, email) values (${bob}, null, ${`cbl-dev-bob-${suffix}@example.test`})`;
		await sql`insert into organization_members (organization_id, user_id, role) values (${org}, ${alice}, 'engineer')`;
		await sql`insert into organization_members (organization_id, user_id, role) values (${org}, ${bob}, 'viewer')`;
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from audit_logs where actor_id in ${sql([alice, bob])}`;
			await sql`delete from cli_device_authorizations where client_version = 'v-test'`;
			await sql`delete from organizations where id in ${sql([org, otherOrg])}`;
			await sql`delete from "user" where id in ${sql([alice, bob])}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	it('start: 256-bit device code stored only as its hash, a consonant user code, 10 min / 5 s', async () => {
		const s = await start();
		expect(s.deviceCode).toMatch(/^[A-Za-z0-9_-]{43}$/);
		expect(s.userCode).toMatch(/^[BCDFGHJKLMNPQRSTVWXZ]{8}$/);
		expect(s).toMatchObject({ expiresIn: 600, interval: 5 });
		const [row] = await sql`select * from cli_device_authorizations where user_code = ${s.userCode} and status = 'pending'`;
		expect(row.device_code_hash).toBe(dev.hashDeviceCode(s.deviceCode));
		expect(JSON.stringify(row)).not.toContain(s.deviceCode);
		expect(row).toMatchObject({ device_name: 'laptop', requested_ip: '203.0.113.7', requested_profile: 'production', client: 'sentinel-cli' });
	});

	it('pending -> slow_down (interval +5) -> approve -> key exactly once -> replay is invalid_grant', async () => {
		const s = await start();
		expect(await dev.pollDeviceToken(s.deviceCode, '203.0.113.7')).toEqual({ kind: 'error', error: 'authorization_pending' });
		expect(await dev.pollDeviceToken(s.deviceCode, '203.0.113.7')).toEqual({ kind: 'error', error: 'slow_down', interval: 10 });

		const found = await dev.findDeviceAuthorization(s.userCode);
		expect(found).toMatchObject({ state: 'pending', deviceName: 'laptop', requestedIp: '203.0.113.7', clientVersion: 'v-test' });

		expect(await approve(s.userCode)).toEqual({ ok: true, state: 'approved' });
		await rewind(s.userCode);
		const token = await dev.pollDeviceToken(s.deviceCode, '203.0.113.9');
		if (token.kind !== 'token') throw new Error(`expected a token, got ${token.error}`);
		expect(token.key).toMatch(/^sent_agent_[0-9a-f]{64}$/);
		expect(token).toMatchObject({ organizationId: org, owner: { name: 'Alice', role: 'engineer' } });
		expect((token.expiresAt.getTime() - Date.now()) / 86_400_000).toBeGreaterThan(89.9);

		// The key works, as Alice's personal key, and nothing about it is stored in plaintext.
		const ctx = await auth.authenticateAgentRequest(bearer(token.key));
		expect(ctx).toMatchObject({ agentKind: 'personal', organizationId: org, agentName: 'Alice (CLI)' });
		expect(ctx.owner).toMatchObject({ userId: alice, role: 'engineer', canWrite: true });
		const [keyRow] = await sql`select * from project_api_keys where id = ${token.keyId}`;
		expect(JSON.stringify(keyRow)).not.toContain(token.key);
		expect(keyRow).toMatchObject({ name: 'laptop', scope: 'agent', created_by: alice });
		const [devRow] = await sql`select status, api_key_id, delivered_at from cli_device_authorizations where user_code = ${s.userCode} and api_key_id is not null`;
		expect(devRow).toMatchObject({ status: 'delivered', api_key_id: token.keyId });

		await rewind(s.userCode);
		expect(await dev.pollDeviceToken(s.deviceCode, '203.0.113.9')).toEqual({ kind: 'error', error: 'invalid_grant' });
		// ...and the approval page now says so.
		expect((await dev.findDeviceAuthorization(s.userCode))?.state).toBe('delivered');

		const audits = await sql`select action, metadata from audit_logs where actor_id = ${alice} and (resource_id = ${token.keyId} or metadata->>'deviceName' = 'laptop') order by created_at`;
		const actions = audits.map((a) => a.action);
		expect(actions).toContain('cli.device.approved');
		expect(actions).toContain('cli.key.minted');
		const minted = audits.find((a) => a.action === 'cli.key.minted')!;
		expect(minted.metadata).toMatchObject({ organizationId: org, deviceName: 'laptop', requestedIp: '203.0.113.7', deliveredToIp: '203.0.113.9' });
		expect(JSON.stringify(audits)).not.toContain(token.key);
	});

	it('concurrent polls of one approved code deliver at most one key', async () => {
		const s = await start('race');
		await approve(s.userCode);
		const results = await Promise.all([1, 2, 3, 4].map(() => dev.pollDeviceToken(s.deviceCode, 'x')));
		expect(results.filter((r) => r.kind === 'token')).toHaveLength(1);
	});

	it('deny -> access_denied; the code cannot then be approved', async () => {
		const s = await start();
		expect(await dev.decideDeviceAuthorization({ userCode: s.userCode, userId: alice, organizationId: null, decision: 'deny', ip: 'x' })).toEqual({
			ok: true,
			state: 'denied',
		});
		expect(await dev.pollDeviceToken(s.deviceCode, 'x')).toEqual({ kind: 'error', error: 'access_denied' });
		expect(await approve(s.userCode)).toEqual({ ok: false, reason: 'already_decided' });
		const [audit] = await sql`select action from audit_logs where actor_id = ${alice} and action = 'cli.device.denied'`;
		expect(audit).toBeTruthy();
	});

	it('expired: no approval, and the poll says expired_token', async () => {
		const s = await start();
		await sql`update cli_device_authorizations set expires_at = now() - interval '1 second' where user_code = ${s.userCode}`;
		expect(await approve(s.userCode)).toEqual({ ok: false, reason: 'expired' });
		expect(await dev.pollDeviceToken(s.deviceCode, 'x')).toEqual({ kind: 'error', error: 'expired_token' });
		expect((await dev.findDeviceAuthorization(s.userCode))?.state).toBe('expired');
	});

	it('approved but not collected in time: expired_token, no key minted', async () => {
		const s = await start();
		await approve(s.userCode);
		await sql`update cli_device_authorizations set expires_at = now() - interval '1 second' where user_code = ${s.userCode}`;
		expect(await dev.pollDeviceToken(s.deviceCode, 'x')).toEqual({ kind: 'error', error: 'expired_token' });
	});

	it('single use: a second approval of the same code is refused', async () => {
		const s = await start();
		expect(await approve(s.userCode)).toEqual({ ok: true, state: 'approved' });
		expect(await approve(s.userCode, bob)).toEqual({ ok: false, reason: 'already_decided' });
	});

	it('only for an org the approver belongs to', async () => {
		const s = await start();
		expect(await approve(s.userCode, alice, otherOrg)).toEqual({ ok: false, reason: 'not_a_member' });
		expect(await approve(s.userCode, alice, null)).toEqual({ ok: false, reason: 'not_a_member' });
		// Still pending, so the real owner can still approve it.
		expect((await dev.findDeviceAuthorization(s.userCode))?.state).toBe('pending');
	});

	it('unknown codes are not found; unknown device codes are invalid_grant', async () => {
		expect(await approve('BBBBBBBB')).toEqual({ ok: false, reason: 'not_found' });
		expect(await dev.pollDeviceToken('x'.repeat(43), 'x')).toEqual({ kind: 'error', error: 'invalid_grant' });
		expect(await dev.pollDeviceToken('short', 'x')).toEqual({ kind: 'error', error: 'invalid_grant' });
	});

	it('the approver removed between approval and delivery: access_denied, nothing minted', async () => {
		const s = await start('removed-between');
		await sql`insert into organization_members (organization_id, user_id, role) values (${otherOrg}, ${bob}, 'engineer')`;
		await approve(s.userCode, bob, otherOrg);
		await sql`delete from organization_members where organization_id = ${otherOrg} and user_id = ${bob}`;
		expect(await dev.pollDeviceToken(s.deviceCode, 'x')).toEqual({ kind: 'error', error: 'access_denied' });
		const keys = await sql`select k.id from project_api_keys k join agents a on a.id = k.agent_id where a.owner_user_id = ${bob} and a.org_id = ${otherOrg}`;
		expect(keys).toHaveLength(0);
	});

	it('a viewer can approve their own CLI; the agent is named after the email when there is no name', async () => {
		const s = await start('bob-laptop');
		await approve(s.userCode, bob);
		const token = await dev.pollDeviceToken(s.deviceCode, 'x');
		if (token.kind !== 'token') throw new Error(token.error);
		const ctx = await auth.authenticateAgentRequest(bearer(token.key));
		expect(ctx.agentName).toBe(`cbl-dev-bob-${suffix}@example.test (CLI)`);
		expect(ctx.owner).toMatchObject({ role: 'viewer', canWrite: false });
	});
});
