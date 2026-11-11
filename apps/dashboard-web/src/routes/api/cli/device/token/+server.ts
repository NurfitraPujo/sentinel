import type { RequestHandler } from './$types';
import { checkRateLimitWithLimit } from '$lib/rate-limit';
import { cliProfileName, publicOrigin } from '$lib/server/cli-dist';
import { pollDeviceToken } from '$lib/server/cli-device';
import { clientIp, noStoreJson, oauthError, POLL_LIMIT_PER_IP, POLL_WINDOW_MS } from '$lib/server/cli-device-http';

// sentinel-cli-browser-login (docs/plans/CLI_BROWSER_LOGIN.md). POST /api/cli/device/token {device_code} -- the CLI's poll
// (RFC 8628 §3.4). UNAUTHENTICATED: the 256-bit device_code is the credential. Answers are RFC 6749 §5.2 errors with HTTP
// 400 (`authorization_pending`, `slow_down` (+ `interval`), `access_denied`, `expired_token`, `invalid_grant`), or 200
// with the personal key -- exactly once: the key is minted in the transaction that marks the request delivered, so a
// replay of the same device_code gets `invalid_grant` and the row never held the key. `Cache-Control: no-store`; the key
// is never logged (this route logs nothing about the body).

const DESCRIPTIONS: Record<string, string> = {
	invalid_grant: 'Unknown or already-used device code; run `sentinel login` again',
	authorization_pending: 'Waiting for the code to be approved in the browser',
	slow_down: 'Polling too fast; wait longer between polls',
	access_denied: 'The login request was denied',
	expired_token: 'The login request expired; run `sentinel login` again',
};

export const POST: RequestHandler = async (event) => {
	const ip = clientIp(event);
	const rl = checkRateLimitWithLimit(`cli-device-token:ip:${ip}`, POLL_LIMIT_PER_IP, POLL_WINDOW_MS);
	if (!rl.allowed) {
		return oauthError('slow_down', DESCRIPTIONS.slow_down, 429, { retry_after: rl.retryAfter ?? 60 });
	}

	const raw: unknown = await event.request.json().catch(() => null);
	const deviceCode = raw && typeof raw === 'object' ? (raw as Record<string, unknown>).device_code : undefined;
	if (typeof deviceCode !== 'string' || deviceCode.length === 0) {
		return oauthError('invalid_request', 'device_code is required');
	}

	const result = await pollDeviceToken(deviceCode, ip);
	if (result.kind === 'error') {
		return oauthError(result.error, DESCRIPTIONS[result.error], 400, result.interval ? { interval: result.interval } : {});
	}

	const cliProfile = cliProfileName();
	return noStoreJson({
		key: result.key,
		key_prefix: result.keyPrefix,
		key_id: result.keyId,
		organization_id: result.organizationId,
		org_slug: result.orgSlug,
		url: publicOrigin(event.url.origin),
		...(cliProfile ? { cli_profile: cliProfile } : {}),
		expires_at: result.expiresAt.toISOString(),
		owner: result.owner,
	});
};
