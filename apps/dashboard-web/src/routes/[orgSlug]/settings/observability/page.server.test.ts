import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * Regression fence for the 500 that shipped on this route (D17 / D12-REGRESSION).
 *
 * This loader delegates to the shared `loadObservability`. It previously forwarded only
 * `{ fetch }` behind an `as Parameters<typeof baseLoad>[0]` cast, so when the shared loader
 * started calling `locals.auth()` for D05, `locals` was `undefined` here and EVERY request to
 * this route threw. No test existed for this loader, and the cast hid it from `svelte-check` —
 * so a green suite and a clean typecheck both reported success on a page that could not render.
 *
 * These tests therefore assert the loader forwards the WHOLE event, by observing behaviour that
 * is only reachable if `locals` actually arrives.
 */

function locals(session: { email: string } | null) {
	return { auth: async () => (session ? { user: { email: session.email } } : null) } as any;
}

const { load } = await import('./+page.server');

describe('[orgSlug]/settings/observability +page.server.ts load', () => {
	beforeEach(() => {
		vi.restoreAllMocks();
	});

	it('forwards locals to the shared loader — an anonymous request 401s rather than throwing a 500', async () => {
		const fetchMock = vi.fn();
		// If `locals` is dropped on the way through, this rejects with a TypeError
		// ("Cannot read properties of undefined") and no `status` — i.e. a 500, not a 401.
		await expect(
			load({ fetch: fetchMock, locals: locals(null), params: { orgSlug: 'acme' } } as any)
		).rejects.toMatchObject({ status: 401 });
		expect(fetchMock).not.toHaveBeenCalled();
	});

	it('returns observability data for an authenticated request', async () => {
		const fetchMock = vi.fn(async () => ({
			ok: true,
			json: async () => ({ status: 'healthy', database: 'connected' })
		}));

		const result = (await load({
			fetch: fetchMock as any,
			locals: locals({ email: 'a@b.com' }),
			params: { orgSlug: 'acme' }
		} as any)) as { observability: { processor: unknown; ingestor: unknown } };

		expect(result.observability.processor).toBeDefined();
		expect(result.observability.ingestor).toBeDefined();
		// D05: cross-tenant DLQ aggregates must not reach the browser on this route either.
		expect(result.observability).not.toHaveProperty('dlq');
		expect(result.observability.processor).not.toHaveProperty('dlq_depth');
	});
});
