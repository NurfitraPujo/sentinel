import { describe, it, expect, vi, beforeEach } from 'vitest';

function locals(session: { email: string } | null) {
	return { auth: async () => (session ? { user: { email: session.email } } : null) } as any;
}

// Import the concrete shared function, not the `PageServerLoad`-annotated `load`. The latter's
// declared return type is `void | Record<string, any>`, so `result.observability` does not
// typecheck against it. `load` is a one-line delegator to this.
const { loadObservability } = await import('$lib/server/observability');
const { load: layoutLoad } = await import('../+layout.server');

describe('settings/observability +page.server.ts load (D05)', () => {
	beforeEach(() => {
		vi.stubGlobal('fetch', vi.fn());
	});

	it('401s an anonymous request instead of returning DLQ/observability data', async () => {
		const fetchMock = vi.fn();
		await expect(
			loadObservability({ fetch: fetchMock as any, locals: locals(null) })
		).rejects.toMatchObject({ status: 401 });
		// The old bug fetched processor/ingestor health (and DLQ data) before any
		// auth check ran. Assert the loader bailed before making any outbound call.
		expect(fetchMock).not.toHaveBeenCalled();
	});

	it('does not expose cross-tenant DLQ aggregates to an authenticated non-operator user', async () => {
		const fetchMock = vi.fn(async (url: string) => {
			if (url.includes('/health')) {
				return {
					ok: true,
					json: async () => ({
						status: 'healthy',
						database: 'ok',
						// Simulates the real /health payload, which mixes in cross-tenant
						// DLQ aggregates (D05) alongside per-instance health fields.
						dlq_depth: 42,
						dlq_publish_failures: 7,
						dlq_threshold: 25,
						dlq_stale_after_seconds: 3600,
						dlq_oldest_age_seconds: 999,
						dlq_oldest_class: 'permanent'
					})
				};
			}
			if (url.includes('/dlq')) {
				return {
					ok: true,
					json: async () => ({
						total_depth: 42,
						publish_failures: 7,
						items: [{ sequence: 1, event_id: 'evt-1', org_id: 'other-org', project_id: 'p1' }]
					})
				};
			}
			throw new Error(`unexpected url ${url}`);
		});

		const result = await loadObservability({ fetch: fetchMock as any, locals: locals({ email: 'a@b.com' }) });

		expect(result.observability.processor).not.toHaveProperty('dlq_depth');
		expect(result.observability.processor).not.toHaveProperty('dlq_publish_failures');
		expect(result.observability.processor).not.toHaveProperty('dlq_oldest_class');
		expect(result.observability).not.toHaveProperty('dlq');

		// The loader must never call the cross-tenant /dlq endpoint at all.
		const calledUrls = fetchMock.mock.calls.map((c) => c[0]);
		expect(calledUrls.some((u: string) => u.includes('/dlq'))).toBe(false);
	});
});

describe('settings/+layout.server.ts load (D05 guard for all settings/ routes)', () => {
	it('401s an anonymous request', async () => {
		await expect(layoutLoad({ locals: locals(null) } as any)).rejects.toMatchObject({ status: 401 });
	});

	it('passes through the session for an authenticated request', async () => {
		const result = (await layoutLoad({ locals: locals({ email: 'a@b.com' }) } as any)) as { session: { user?: { email?: string } } };
		expect(result.session?.user?.email).toBe('a@b.com');
	});
});
