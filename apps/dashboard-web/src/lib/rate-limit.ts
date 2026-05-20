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