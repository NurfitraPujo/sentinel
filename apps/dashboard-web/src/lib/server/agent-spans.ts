import { getSpansStatuses, type SpanScope } from '$lib/server/queries/span-queries';
import type { SpansStatus } from '$lib/spans/status';

/**
 * sentinel-occurrence-spans TASK-01 — the agent API's `spansStatus` / `hasSpans` on an occurrence.
 *
 * `spansStatus` is `null` for an occurrence with no canonical (32-hex) trace id: it has no trace to have spans. Otherwise
 * `available | pending | none` through the waterfall's guard, with the key's organization scope (agent reads are
 * organization-wide, not environment-scoped — docs/ENVIRONMENT_ACCESS.md). One statement for the whole page.
 */
export interface WithSpans {
	spansStatus: SpansStatus | null;
	hasSpans: boolean;
}

export async function annotateSpans<T extends { traceId: string | null }>(rows: T[], scope: SpanScope): Promise<Array<T & WithSpans>> {
	const statuses = await getSpansStatuses(rows.map((r) => r.traceId), scope);
	return rows.map((r) => {
		const spansStatus = (r.traceId ? statuses.get(r.traceId) : undefined) ?? null;
		return { ...r, spansStatus, hasSpans: spansStatus === 'available' };
	});
}
