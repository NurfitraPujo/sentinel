import type { PageServerLoad } from './$types';
import { load as baseLoad, type ObservabilityData } from '../../../settings/observability/+page.server';

export const load: PageServerLoad = async (event) => {
	const { orgSlug } = event.params;
	// Validate orgSlug parameter is present
	if (!orgSlug) {
		return {
			observability: {
				processor: { status: 'offline', database: 'unreachable', dlq_depth: 0, dlq_publish_failures: 0, dlq_threshold: 25, dlq_stale_after_seconds: 3600 },
				ingestor: { status: 'offline' },
				dlq: { total_depth: 0, publish_failures: 0, items: [] },
				fetchedAt: new Date().toISOString()
			}
		};
	}

	// baseLoad only reads `fetch` off the event. Its declared parameter type is pinned to
	// the non-org route's literal RouteId ("/settings/observability"), which this route's
	// event ("/[orgSlug]/settings/observability") can never structurally satisfy even though
	// the two events are otherwise identical — so we pass just the piece baseLoad actually
	// uses, typed against baseLoad's own parameter shape rather than blanket-cast to `any`.
	//
	// baseLoad is declared `: PageServerLoad` with the default (unparameterized) OutputData,
	// so its *externally visible* return type is that generic's permissive `Record<string,
	// any> | void` shape, not the concrete `{ observability }` object it actually returns at
	// runtime. Left alone, that generic shape poisons this function's own inferred return
	// type when unioned with the branch above, which is exactly what produced the
	// `{ [x: string]: undefined }` PageData this file exists to fix. Narrow it back to the
	// concrete, known-at-runtime shape at this one boundary.
	const result = (await baseLoad({ fetch: event.fetch } as Parameters<typeof baseLoad>[0])) as {
		observability: ObservabilityData;
	};
	return result;
};

