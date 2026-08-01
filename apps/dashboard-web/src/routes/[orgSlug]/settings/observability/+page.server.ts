import type { PageServerLoad } from './$types';
import { load as baseLoad } from '../../settings/observability/+page.server';

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

	return baseLoad(event);
};

