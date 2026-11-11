// Lives in $lib rather than in the route file because SvelteKit route modules may only export a
// fixed set of names (load, actions, prerender, ...) and reject anything else at BUILD time.
// Exporting `loadObservability` from `routes/settings/observability/+page.server.ts` failed
// `pnpm build` with "Invalid export 'loadObservability'" while `pnpm check` and `pnpm test` both
// passed — the same trap that caught INVITE_TOKEN_COOKIE (see $lib/server/invite-cookie).
import { error } from '@sveltejs/kit';
import { env } from '$env/dynamic/private';

// This route deliberately does not expose DLQ items or DLQ depth aggregates
// to the browser — see the load function below for why (D05).
export interface ObservabilityData {
	processor: {
		status: string;
		database: string;
		error?: string;
	};
	ingestor: {
		status: string;
		database?: string;
		redis?: string;
		error?: string;
	};
	fetchedAt: string;
}

/**
 * Shared loader body for BOTH observability routes: `/settings/observability` and
 * `/[orgSlug]/settings/observability`.
 *
 * Extracted as a plain function rather than letting the org route call `load` directly.
 * The two routes' generated `PageServerLoad` types are pinned to different literal RouteIds,
 * so the org route's event can never structurally satisfy this one's parameter type. The
 * previous workaround passed `{ fetch }` alone through an `as Parameters<typeof load>[0]`
 * cast — which silently dropped `locals` and made EVERY request to the org route throw a 500
 * on `locals.auth()` below, with the cast hiding it from `svelte-check`.
 *
 * A concrete signature both routes can call keeps the auth check reachable and keeps the
 * return type narrow: a bare `PageServerLoad` annotation widens it to
 * `void | Record<string, any>`, which is what made `result.observability` un-typeable in the
 * tests and produced the `{ [x: string]: undefined }` PageData on the org page.
 */
export async function loadObservability({
	fetch,
	locals
}: {
	fetch: typeof globalThis.fetch;
	locals: App.Locals;
}): Promise<{ observability: ObservabilityData }> {
	// This page previously never called locals.auth() at all — an anonymous
	// visitor got DLQ depth, publish-failure counts, oldest-message age,
	// JetStream stream names, and error classes (D05). `settings/+layout.server.ts`
	// now enforces this too, but the check is repeated here so this loader is
	// safe even if the layout is ever bypassed or this file is moved.
	const session = await locals.auth();
	if (!session?.user?.email) {
		throw error(401, 'Unauthorized');
	}

	// No platform-operator concept exists anywhere in this codebase (searched
	// for isOperator/platformOperator/operator across src/lib — zero hits).
	// The `/dlq` payload is a cross-tenant aggregate: `dlq_depth`,
	// `dlq_publish_failures`, oldest-age/class, and the raw `items` array
	// (each item carries another org's org_id/project_id/payload) all mix
	// every tenant's failures together. Until a real operator role exists,
	// this loader is restricted to "any authenticated user" AND strips every
	// cross-tenant field: it fetches processor/ingestor `/health` (which
	// report only this instance's own status) but no longer calls `/dlq` at
	// all and no longer surfaces processor's dlq_* fields.
	// D45: read via $env/dynamic/private, not process.env. In a SvelteKit load these are not
	// equivalent -- process.env is populated only by adapters that happen to run on Node with the
	// variables already in the process environment, so the same code silently falls back to the
	// localhost defaults under other adapters. $env/dynamic/private is the documented server-side
	// accessor and works uniformly.
	const processorUrl = env.PROCESSOR_HEALTH_URL || 'http://localhost:8081';
	const ingestorUrl = env.INGESTOR_HEALTH_URL || 'http://localhost:8080';

	let processorData: ObservabilityData['processor'] = {
		status: 'unknown',
		database: 'unknown'
	};

	let ingestorData: ObservabilityData['ingestor'] = {
		status: 'unknown'
	};

	try {
		const res = await fetch(`${processorUrl}/health`);
		if (res.ok) {
			const raw = await res.json();
			// `/health`'s own JSON contract includes dlq_depth, dlq_publish_failures,
			// dlq_threshold, dlq_stale_after_seconds, dlq_oldest_age_seconds and
			// dlq_oldest_class — every one a cross-tenant aggregate (DLQ depth
			// aggregates every org). Deliberately keep only status/database/error.
			processorData = {
				status: raw.status ?? 'unknown',
				database: raw.database ?? 'unknown',
				error: raw.error
			};
		}
	} catch (e: any) {
		processorData = {
			...processorData,
			status: 'offline',
			database: 'unreachable'
		};
	}

	try {
		const res = await fetch(`${ingestorUrl}/health`);
		if (res.ok) {
			const raw = await res.json();
			ingestorData = {
				status: raw.status ?? 'unknown',
				database: raw.database,
				redis: raw.redis,
				error: raw.error
			};
		}
	} catch (e: any) {
		ingestorData = {
			status: 'offline'
		};
	}

	// The `/dlq` endpoint (D12) is skipped entirely: it is a raw, cross-tenant
	// list of parked events (org_id, project_id, raw_payload per item) with no
	// tenant scoping available to filter it down to the caller's own orgs.

	return {
		observability: {
			processor: processorData,
			ingestor: ingestorData,
			fetchedAt: new Date().toISOString()
		}
	};
}
