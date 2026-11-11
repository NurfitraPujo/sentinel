import { describe, it, expect, vi, beforeEach } from 'vitest';
import { resetRateLimitKey } from '$lib/rate-limit';

/**
 * sentinel-cli-browser-login: the two UNAUTHENTICATED device-flow routes, with the state machine mocked (it has its own
 * real-Postgres suite, lib/server/cli-device.integration.test.ts). Pins what the routes themselves own: JSON only, input
 * cleaning, rate limits, the verification URI's origin (AUTH_URL, never the Host header), RFC-shaped errors, no-store.
 */

const startDeviceAuthorization = vi.fn();
const pollDeviceToken = vi.fn();
vi.mock('$lib/server/cli-device', async (orig) => ({
	...(await orig<typeof import('$lib/server/cli-device')>()),
	startDeviceAuthorization,
	pollDeviceToken,
}));

const { POST: START } = await import('./device/+server');
const { POST: TOKEN } = await import('./device/token/+server');

function event(path: string, body: unknown, opts: { ip?: string; contentType?: string; host?: string } = {}) {
	const url = new URL(`http://${opts.host ?? 'dash.internal'}${path}`);
	const request = new Request(url, {
		method: 'POST',
		headers: { 'content-type': opts.contentType ?? 'application/json', 'user-agent': 'sentinel/1.2.3 (linux)' },
		body: typeof body === 'string' ? body : JSON.stringify(body),
	});
	return { request, url, getClientAddress: () => opts.ip ?? '203.0.113.5', params: {}, locals: {} } as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	vi.unstubAllEnvs();
	for (const ip of ['203.0.113.5', '203.0.113.6', '203.0.113.99']) {
		resetRateLimitKey(`cli-device-start:ip:${ip}`);
		resetRateLimitKey(`cli-device-token:ip:${ip}`);
	}
	resetRateLimitKey('cli-device-start:global');
	startDeviceAuthorization.mockResolvedValue({ deviceCode: 'D'.repeat(43), userCode: 'BCDFGHJK', expiresIn: 600, interval: 5 });
});

describe('POST /api/cli/device', () => {
	it('starts a flow: RFC 8628 response, verification URI on AUTH_URL (not the Host header), no-store', async () => {
		vi.stubEnv('AUTH_URL', 'https://sentinel.example.com');
		const res = await START(event('/api/cli/device', { client: 'sentinel-cli', version: 'v1.2.3', device_name: 'ada-laptop', profile: 'production' }, { host: 'evil.example' }));
		expect(res.status).toBe(200);
		expect(res.headers.get('cache-control')).toBe('no-store');
		expect(await res.json()).toEqual({
			device_code: 'D'.repeat(43),
			user_code: 'BCDF-GHJK',
			verification_uri: 'https://sentinel.example.com/cli/approve',
			verification_uri_complete: 'https://sentinel.example.com/cli/approve?code=BCDF-GHJK',
			expires_in: 600,
			interval: 5,
		});
		expect(startDeviceAuthorization).toHaveBeenCalledWith({
			client: 'sentinel-cli',
			clientVersion: 'v1.2.3',
			deviceName: 'ada-laptop',
			profile: 'production',
			ip: '203.0.113.5',
			userAgent: 'sentinel/1.2.3 (linux)',
		});
	});

	it('cleans what it stores: control characters stripped, lengths bounded, a bad profile dropped', async () => {
		await START(event('/api/cli/device', { client: 'x'.repeat(100), version: 'v\u0007', device_name: ` evil\n\u001b[31mname${'y'.repeat(100)}`, profile: '../etc' }));
		const arg = startDeviceAuthorization.mock.calls[0][0];
		expect(arg.deviceName).not.toMatch(/[\u0000-\u001f]/);
		expect(arg.deviceName.length).toBeLessThanOrEqual(64);
		expect(arg.client.length).toBe(32);
		expect(arg.clientVersion).toBe('v');
		expect(arg.profile).toBe('');
	});

	it('400 without a device name; 415 for a non-JSON body (a cross-site form cannot start a flow)', async () => {
		expect((await START(event('/api/cli/device', { client: 'sentinel-cli' }))).status).toBe(400);
		const form = await START(event('/api/cli/device', 'device_name=x', { contentType: 'application/x-www-form-urlencoded' }));
		expect(form.status).toBe(415);
		expect(startDeviceAuthorization).not.toHaveBeenCalled();
	});

	it('rate-limits starts per IP (10 / 10 min) without affecting another IP', async () => {
		const body = { device_name: 'd' };
		for (let i = 0; i < 10; i++) expect((await START(event('/api/cli/device', body))).status).toBe(200);
		const limited = await START(event('/api/cli/device', body));
		expect(limited.status).toBe(429);
		expect(await limited.json()).toMatchObject({ error: 'slow_down' });
		expect((await START(event('/api/cli/device', body, { ip: '203.0.113.6' }))).status).toBe(200);
	});
});

describe('POST /api/cli/device/token', () => {
	it.each([
		['authorization_pending', undefined],
		['slow_down', 10],
		['access_denied', undefined],
		['expired_token', undefined],
		['invalid_grant', undefined],
	] as const)('%s is an RFC 6749 400 error', async (code, interval) => {
		pollDeviceToken.mockResolvedValue({ kind: 'error', error: code, ...(interval ? { interval } : {}) });
		const res = await TOKEN(event('/api/cli/device/token', { device_code: 'D'.repeat(43) }));
		expect(res.status).toBe(400);
		expect(res.headers.get('cache-control')).toBe('no-store');
		const body = await res.json();
		expect(body.error).toBe(code);
		expect(typeof body.error_description).toBe('string');
		if (interval) expect(body.interval).toBe(interval);
	});

	it('delivers the key once with the org, the public URL and the suggested profile', async () => {
		vi.stubEnv('AUTH_URL', 'https://sentinel.example.com');
		vi.stubEnv('SENTINEL_CLI_PROFILE', 'production');
		pollDeviceToken.mockResolvedValue({
			kind: 'token',
			key: 'sent_agent_abc',
			keyPrefix: 'sent_agent_',
			keyId: 'key-1',
			expiresAt: new Date('2027-01-01T00:00:00.000Z'),
			organizationId: 'org-1',
			orgSlug: 'acme',
			owner: { name: 'Ada', email: 'ada@example.test', role: 'engineer' },
		});
		const res = await TOKEN(event('/api/cli/device/token', { device_code: 'D'.repeat(43) }, { host: 'evil.example' }));
		expect(res.status).toBe(200);
		expect(res.headers.get('cache-control')).toBe('no-store');
		expect(await res.json()).toEqual({
			key: 'sent_agent_abc',
			key_prefix: 'sent_agent_',
			key_id: 'key-1',
			organization_id: 'org-1',
			org_slug: 'acme',
			url: 'https://sentinel.example.com',
			cli_profile: 'production',
			expires_at: '2027-01-01T00:00:00.000Z',
			owner: { name: 'Ada', email: 'ada@example.test', role: 'engineer' },
		});
		expect(pollDeviceToken).toHaveBeenCalledWith('D'.repeat(43), '203.0.113.5');
	});

	it('400 invalid_request without a device_code; rate-limited per IP', async () => {
		expect((await TOKEN(event('/api/cli/device/token', {}))).status).toBe(400);
		pollDeviceToken.mockResolvedValue({ kind: 'error', error: 'authorization_pending' });
		for (let i = 0; i < 120; i++) await TOKEN(event('/api/cli/device/token', { device_code: 'x' }, { ip: '203.0.113.99' }));
		const limited = await TOKEN(event('/api/cli/device/token', { device_code: 'x' }, { ip: '203.0.113.99' }));
		expect(limited.status).toBe(429);
	});
});
