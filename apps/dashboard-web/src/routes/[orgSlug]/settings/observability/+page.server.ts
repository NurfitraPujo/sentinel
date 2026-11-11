import type { PageServerLoad } from './$types';
import { loadObservability } from '$lib/server/observability';

/**
 * Org-scoped view of the same observability data as `/settings/observability`.
 *
 * This passes the WHOLE event through. It previously passed only `{ fetch }` behind an
 * `as Parameters<typeof baseLoad>[0]` cast, so once the shared loader started calling
 * `locals.auth()` (D05), `locals` was undefined here and every request to this route threw a
 * 500 — on the exact page D17 existed to fix. The cast is what kept `svelte-check` quiet.
 *
 * Note this route sits OUTSIDE `settings/+layout.server.ts` (it is `/[orgSlug]/settings/...`,
 * not `/settings/...`), so that layout guard does not cover it. `hooks.server.ts`'s `orgHandle`
 * does 403 a non-member, but the auth check inside `loadObservability` is what stands in front
 * of this data for an anonymous visitor — which is precisely why it must receive a real
 * `locals`.
 *
 * The previous `if (!orgSlug)` branch was unreachable (SvelteKit does not match this route
 * without the param) and returned a stale shape still carrying the `dlq_*` cross-tenant fields
 * that D05 removed from `ObservabilityData`. Dropped rather than maintained.
 *
 * `orgSlug` is deliberately unused: the payload is instance-wide health, already stripped of
 * cross-tenant aggregates by the shared loader. It is not scoped per organization; this route
 * exists only so the org-nav shell can link to it.
 */
export const load: PageServerLoad = (event) => loadObservability(event);
