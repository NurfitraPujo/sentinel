import { describe, it, expect, vi, beforeEach } from 'vitest';

// sentinel-reporting-and-dashboard D20 / contract §6: 401 / 503 / 200 of GET /api/metrics/insights, at the route.
const { mockEnv, ensure, renderInsights } = vi.hoisted(() => ({
	mockEnv: {} as Record<string, string | undefined>,
	ensure: vi.fn(),
	renderInsights: vi.fn(() => 'sentinel_insight_marker 1\n'),
}));
vi.mock('$env/dynamic/private', () => ({ env: mockEnv }));
vi.mock('$lib/server/analytics/insights-source', () => ({ ensureInsightsRefreshing: ensure, renderInsights }));

import { GET } from './+server';

const call = (headers: Record<string, string> = {}) => GET({ request: new Request('http://x/api/metrics/insights', { headers }) } as never);

beforeEach(() => {
	for (const k of Object.keys(mockEnv)) delete mockEnv[k];
	ensure.mockClear();
});

describe('GET /api/metrics/insights', () => {
	it('answers 503 when the server has no token, even for a request that sends one', async () => {
		expect((await call({ authorization: 'Bearer anything' })).status).toBe(503);
		expect(ensure).not.toHaveBeenCalled();
	});

	it('answers 401 with no Authorization header, a non-bearer one, or the wrong token', async () => {
		mockEnv.METRICS_SCRAPE_TOKEN = 's3cret-token';
		expect((await call()).status).toBe(401);
		expect((await call({ authorization: 's3cret-token' })).status).toBe(401);
		expect((await call({ authorization: 'Bearer wrong' })).status).toBe(401);
		expect((await call({ authorization: 'Bearer s3cret-token-and-more' })).status).toBe(401);
		expect(ensure).not.toHaveBeenCalled(); // an unauthenticated caller never starts work
	});

	it('answers 200 with the last refresh, as text exposition, and starts the refresh loop', async () => {
		mockEnv.METRICS_SCRAPE_TOKEN = 's3cret-token';
		const res = await call({ authorization: 'Bearer s3cret-token' });
		expect(res.status).toBe(200);
		expect(res.headers.get('content-type')).toContain('text/plain');
		expect(res.headers.get('cache-control')).toBe('no-store');
		expect(await res.text()).toBe('sentinel_insight_marker 1\n');
		expect(ensure).toHaveBeenCalledTimes(1);
	});
});
