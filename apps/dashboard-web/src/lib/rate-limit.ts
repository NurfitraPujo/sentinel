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
/**
 * sentinel-user-report-issue contract §4.5: a dedicated per-minute ceiling for
 * `POST /api/report/attachments`, on top of (not instead of) the key's own `rate_limit_rpm`.
 * The key's limit is sized for a service's normal traffic; a binary upload endpoint whose caller
 * is, in effect, a stranger with a service's credential needs a much tighter one, and the contract
 * fixes the default at 10/min.
 *
 * Keyed by `project_api_keys.id` -- never the raw secret, never anything the request supplies
 * (B7). SR-1: this stays the shipped in-process limiter, so it is per replica rather than per
 * cluster; the feature's durable abuse ceiling is the Postgres byte budget in `upload-core.ts`,
 * which is host- and replica-independent.
 */
export const DEFAULT_REPORT_UPLOAD_RPM = 10;

/**
 * Resolves `REPORT_UPLOAD_RPM`. An unset, unparseable or non-positive value falls back to the
 * contract default rather than to "no limit" -- a misconfigured throttle must not become an open
 * one.
 */
export function resolveReportUploadRpm(raw: string | undefined): number {
	if (raw === undefined || raw.trim() === '') return DEFAULT_REPORT_UPLOAD_RPM;
	const parsed = Number(raw);
	if (!Number.isFinite(parsed) || parsed <= 0) return DEFAULT_REPORT_UPLOAD_RPM;
	return Math.floor(parsed);
}

export function checkReportUploadRateLimit(
	keyId: string,
	rpm: number
): { allowed: boolean; retryAfter?: number } {
	return checkRateLimitWithLimit(`report-upload:${keyId}`, rpm);
}
