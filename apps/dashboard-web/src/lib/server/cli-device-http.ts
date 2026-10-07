import type { RequestEvent } from '@sveltejs/kit';

/**
 * sentinel-cli-browser-login: small HTTP helpers shared by the unauthenticated `/api/cli/device*` routes.
 */

// Rate limits for the unauthenticated device-flow routes (in-process, per replica, like every limiter in this app). They
// live here, not in the +server.ts files, because a route module may export only its handlers (B12).
export const START_LIMIT_PER_IP = 10;
export const START_LIMIT_GLOBAL = 300;
export const START_WINDOW_MS = 10 * 60 * 1000;
export const POLL_LIMIT_PER_IP = 120;
export const POLL_WINDOW_MS = 60 * 1000;
/** Failed approval-page lookups (unknown/expired codes) per signed-in user, against user_code guessing. */
export const APPROVE_LOOKUP_LIMIT_PER_USER = 20;
export const APPROVE_LOOKUP_WINDOW_MS = 10 * 60 * 1000;

/** The caller's IP (adapter-node's ADDRESS_HEADER / XFF_DEPTH, falling back to the socket peer); never throws. */
export { clientIp } from './client-ip';

/** JSON with `Cache-Control: no-store` -- every device-flow response carries a code or a key. */
export function noStoreJson(body: unknown, status = 200, headers: Record<string, string> = {}): Response {
	return new Response(JSON.stringify(body), {
		status,
		headers: { 'Content-Type': 'application/json', 'Cache-Control': 'no-store', Pragma: 'no-cache', ...headers },
	});
}

/** RFC 6749 §5.2-shaped error (`{error, error_description}`), which is what RFC 8628 §3.5 polls read. */
export function oauthError(error: string, description: string, status = 400, extra: Record<string, unknown> = {}): Response {
	return noStoreJson({ error, error_description: description, ...extra }, status);
}
