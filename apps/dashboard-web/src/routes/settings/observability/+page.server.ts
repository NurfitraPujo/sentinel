import type { PageServerLoad } from './$types';
import { loadObservability } from '$lib/server/observability';

export type { ObservabilityData } from '$lib/server/observability';

export const load: PageServerLoad = (event) => loadObservability(event);
