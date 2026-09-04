import type { RequestHandler } from './$types';
import { renderMetrics } from '$lib/server/metrics';

/**
 * sentinel-user-report-issue contract SR-3 / §12 — dashboard-web's Prometheus scrape target.
 *
 * Verified at `b768e5b`, this app had no `/metrics` route; the chart's only `prometheus.io/scrape`
 * annotation is on `processor.yaml`, so neither this service nor the ingestor's existing
 * `/metrics` is scraped today. This route is the target half of SR-3's "emitter, scrape target and
 * alert rules"; the annotation and the rule files are the item that wires the chain.
 *
 * **Unauthenticated, and that is the shipped pattern here, not an oversight.** The ingestor mounts
 * its `/metrics` "outside both middlewares" (`apps/ingestor-go/main.go`) for the same reason: a
 * scraper has no API key and no session, and a Kubernetes scrape reaches the pod IP directly. What
 * makes that safe is the rule `metrics.ts` enforces -- every label value is one of a fixed, closed
 * constant set, so this response carries no organization, project, key, reporter or report content
 * whatsoever. That property is the access-control decision; keep it if you add a metric.
 *
 * The path is `/api/metrics` and not `/metrics` because `api` is in `hooks.server.ts`'s
 * `reservedRoutes` and a bare `/metrics` would be parsed as an organization slug by the
 * `[orgSlug]` catch-all -- the same prefix rule §4.1 states for `/api/report`.
 */
export const GET: RequestHandler = async () => {
	const { body, contentType } = await renderMetrics();
	return new Response(body, {
		status: 200,
		headers: {
			'Content-Type': contentType,
			// A scrape is a point-in-time read of a live counter; a cached one is a lie about now.
			'Cache-Control': 'no-store',
		},
	});
};
