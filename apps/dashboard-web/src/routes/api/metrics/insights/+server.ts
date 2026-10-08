import type { RequestHandler } from './$types';
import { createHash, timingSafeEqual } from 'node:crypto';
import { env } from '$env/dynamic/private';
import { ensureInsightsRefreshing, renderInsights } from '$lib/server/analytics/insights-source';

/**
 * sentinel-reporting-and-dashboard D20: the dashboard's own numbers as Prometheus gauges, for VictoriaMetrics (and so
 * Grafana, Wayflow and any other reader of it).
 *
 * Unlike /api/metrics, this response carries project names, service names and counts across every project, so it is
 * NOT public: `Authorization: Bearer <METRICS_SCRAPE_TOKEN>`, compared in constant time. Missing or wrong token: 401.
 * No token configured on the server: 503 (a server that cannot authenticate must not answer). /api/metrics keeps its
 * unauthenticated, closed-label-set rule unchanged.
 *
 * A scrape only serves the last background refresh (insights-state.ts); the first scrape starts the refresh loop.
 */
export const GET: RequestHandler = async ({ request }) => {
	const expected = env.METRICS_SCRAPE_TOKEN;
	if (!expected) return new Response('metrics scrape token is not configured', { status: 503 });

	const header = request.headers.get('authorization') ?? '';
	const match = /^Bearer (.+)$/.exec(header);
	if (!match || !tokensEqual(match[1], expected)) {
		return new Response('unauthorized', { status: 401, headers: { 'WWW-Authenticate': 'Bearer' } });
	}

	ensureInsightsRefreshing();
	return new Response(renderInsights(), {
		status: 200,
		headers: { 'Content-Type': 'text/plain; version=0.0.4; charset=utf-8', 'Cache-Control': 'no-store' },
	});
};

/** Constant-time compare that does not leak length: both sides are hashed to a fixed width first. */
function tokensEqual(a: string, b: string): boolean {
	const ha = createHash('sha256').update(a).digest();
	const hb = createHash('sha256').update(b).digest();
	return timingSafeEqual(ha, hb);
}
