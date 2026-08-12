const rateLimitStore = new Map<string, { count: number; resetAt: number }>();

const WINDOW_MS = 60 * 1000;
const MAX_REQUESTS = 5;

export function checkRateLimit(email: string): { allowed: boolean; retryAfter?: number } {
	const now = Date.now();
	const key = email.toLowerCase();

	const entry = rateLimitStore.get(key);

	if (!entry || now > entry.resetAt) {
		rateLimitStore.set(key, { count: 1, resetAt: now + WINDOW_MS });
		return { allowed: true };
	}

	if (entry.count >= MAX_REQUESTS) {
		const retryAfter = Math.ceil((entry.resetAt - now) / 1000);
		return { allowed: false, retryAfter };
	}

	entry.count++;
	return { allowed: true };
}

export function resetRateLimit(email: string): void {
	const key = email.toLowerCase();
	rateLimitStore.delete(key);
}

/**
 * R19 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): a generic, per-key sliding-window-by-reset
 * limiter, sharing `rateLimitStore` with `checkRateLimit` above but keyed however the caller
 * likes (agent-auth.ts uses `agent-key:<project_api_keys.id>` -- never the raw secret, and never
 * derived from anything the request body claims, per B7) and with a caller-supplied `limit`
 * instead of the hardcoded `MAX_REQUESTS` -- `checkRateLimit`'s fixed 5-per-minute is right for a
 * human sign-in form, but `/api/agent/*` needs to honor each key's own
 * `project_api_keys.rate_limit_rpm`.
 */
export function checkRateLimitWithLimit(
	key: string,
	limit: number,
	windowMs: number = WINDOW_MS
): { allowed: boolean; retryAfter?: number } {
	const now = Date.now();
	const entry = rateLimitStore.get(key);

	if (!entry || now > entry.resetAt) {
		rateLimitStore.set(key, { count: 1, resetAt: now + windowMs });
		return { allowed: true };
	}

	if (entry.count >= limit) {
		const retryAfter = Math.ceil((entry.resetAt - now) / 1000);
		return { allowed: false, retryAfter };
	}

	entry.count++;
	return { allowed: true };
}

/** Test-only reset for a `checkRateLimitWithLimit` key (mirrors `resetRateLimit` above). */
export function resetRateLimitKey(key: string): void {
	rateLimitStore.delete(key);
}