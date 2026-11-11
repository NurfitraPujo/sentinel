import type { RequestHandler } from './$types';
import { checkRateLimitWithLimit } from '$lib/rate-limit';
import { publicOrigin } from '$lib/server/cli-dist';
import { cleanProfile, cleanText, formatUserCode, startDeviceAuthorization } from '$lib/server/cli-device';
import {
	clientIp,
	noStoreJson,
	oauthError,
	START_LIMIT_GLOBAL,
	START_LIMIT_PER_IP,
	START_WINDOW_MS,
} from '$lib/server/cli-device-http';

// sentinel-cli-browser-login (docs/plans/CLI_BROWSER_LOGIN.md). POST /api/cli/device -- start an RFC 8628 device
// authorization for `sentinel login`. UNAUTHENTICATED by design (the CLI has no credential yet), so:
//   - rate-limited per IP and globally (in-process like every limiter here, so per replica);
//   - JSON only, so a cross-site <form> cannot drive it;
//   - every input is length-bounded and stripped of control characters before it is stored or shown on the approval page;
//   - the verification URI is built from publicOrigin() (AUTH_URL first), never from the Host header, so the printed
//     link can't be pointed at another server.
// Nothing here grants access: a started request is inert until a signed-in member approves its user_code.

export const POST: RequestHandler = async (event) => {
	const { request, url } = event;
	if (!(request.headers.get('content-type') ?? '').toLowerCase().startsWith('application/json')) {
		return oauthError('invalid_request', 'Content-Type must be application/json', 415);
	}

	const ip = clientIp(event);
	for (const [key, limit] of [
		[`cli-device-start:ip:${ip}`, START_LIMIT_PER_IP],
		['cli-device-start:global', START_LIMIT_GLOBAL],
	] as const) {
		const rl = checkRateLimitWithLimit(key, limit, START_WINDOW_MS);
		if (!rl.allowed) {
			return oauthError('slow_down', 'Too many login attempts; try again later', 429, {
				retry_after: rl.retryAfter ?? 60,
			});
		}
	}

	const raw: unknown = await request.json().catch(() => null);
	const body = raw && typeof raw === 'object' ? (raw as Record<string, unknown>) : null;
	if (!body) {
		return oauthError('invalid_request', 'Expected a JSON object body');
	}

	const deviceName = cleanText(body.device_name, 64);
	if (!deviceName) {
		return oauthError('invalid_request', 'device_name is required');
	}

	const origin = publicOrigin(url.origin);
	if (!origin) {
		return oauthError('server_error', 'The dashboard has no usable public URL (set AUTH_URL)', 500);
	}

	const started = await startDeviceAuthorization({
		client: cleanText(body.client, 32),
		clientVersion: cleanText(body.version, 64),
		deviceName,
		profile: cleanProfile(body.profile),
		ip,
		userAgent: cleanText(request.headers.get('user-agent'), 255),
	});

	const userCode = formatUserCode(started.userCode);
	const verificationUri = `${origin}/cli/approve`;
	return noStoreJson({
		device_code: started.deviceCode,
		user_code: userCode,
		verification_uri: verificationUri,
		verification_uri_complete: `${verificationUri}?code=${encodeURIComponent(userCode)}`,
		expires_in: started.expiresIn,
		interval: started.interval,
	});
};
