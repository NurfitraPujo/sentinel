import { issueQueries } from '$lib/server/queries/issue-queries';
import { getSpansStatuses, type SpanScope } from '$lib/server/queries/span-queries';
import type { SpansStatus } from '$lib/spans/status';

/**
 * sentinel-occurrence-spans TASK-01 — which of an issue's occurrences have spans the viewer can open, for the issue page
 * and the agent API. Lives here, not in a route file (B12: a `+page.server.ts` may export only SvelteKit's names).
 *
 * `error_occurrences` has an index on `(issue_id, created_at DESC)` and none that relates an occurrence to its spans, and
 * `spans` is ~9 GB, so "occurrences WITH spans" cannot be a join. It is a BOUNDED WALK instead: read the issue's newest
 * `SPANS_SCAN_BOUND` occurrences' trace ids off that index (one cheap query), ask `getSpansStatuses` about them
 * `SPANS_STATUS_BATCH` at a time, newest first, and stop as soon as `want` have spans. The cost is therefore at most
 * 1 index range of SPANS_SCAN_BOUND entries + SPANS_SCAN_BOUND/SPANS_STATUS_BATCH status statements, however large the
 * issue (the status statement is one EXISTS probe per trace per partition). The price of the bound is that an
 * occurrence older than the newest SPANS_SCAN_BOUND is not found; callers say so (`exhausted: false`).
 *
 * Measured on prod 2026-10-06: ~3-4% of traced occurrences have spans, so 2,000 occurrences yield ~70 — a full page is
 * usually found only on issues whose pulls landed densely.
 */
export const SPANS_SCAN_BOUND = 2000;
export const SPANS_STATUS_BATCH = 200;

export interface SpansWalk {
	/** Occurrence ids that have visible spans, newest first, at most `want`. */
	ids: string[];
	/** The trace id of each id above. */
	traceIds: Map<string, string>;
	/** How many of the issue's occurrences the walk looked at (after `skip`). */
	scanned: number;
	/** True when the walk reached the end of the issue's occurrences, so nothing older was left unexamined. */
	exhausted: boolean;
	/** Where to continue (`before`) when something older may remain: the last occurrence returned if `want` was met, else the last one inspected. */
	continueBefore: Date | null;
}

export async function walkOccurrencesWithSpans(opts: {
	issueId: string;
	scope: SpanScope;
	want: number;
	/** Skip this many of the newest occurrences (already inspected by the caller). */
	skip?: number;
	/** Only occurrences strictly older than this (the agent API's `before` cursor). */
	before?: Date;
	bound?: number;
}): Promise<SpansWalk> {
	const bound = opts.bound ?? SPANS_SCAN_BOUND;
	const window = await issueQueries.scanOccurrenceTraces(opts.issueId, bound, { offset: opts.skip ?? 0, before: opts.before });
	const traced = window.flatMap((r) => (r.traceId === null ? [] : [{ id: r.id, traceId: r.traceId }]));
	const ids: string[] = [];
	const traceIds = new Map<string, string>();
	let lastFound: Date | null = null;
	let lastFoundAt = -1;
	const indexOf = new Map(window.map((r, i) => [r.id, i]));
	for (let i = 0; i < traced.length && ids.length < opts.want; i += SPANS_STATUS_BATCH) {
		const batch = traced.slice(i, i + SPANS_STATUS_BATCH);
		const statuses = await getSpansStatuses(batch.map((r) => r.traceId), opts.scope);
		for (const r of batch) {
			if (statuses.get(r.traceId) !== 'available') continue;
			ids.push(r.id);
			traceIds.set(r.id, r.traceId);
			lastFoundAt = indexOf.get(r.id) ?? -1;
			lastFound = window[lastFoundAt]?.createdAt ?? null;
			if (ids.length >= opts.want) break;
		}
	}
	const reachedWant = ids.length >= opts.want;
	const exhausted = window.length < bound;
	const continueBefore = reachedWant ? lastFound : exhausted ? null : (window[window.length - 1]?.createdAt ?? null);
	// `scanned` counts what was actually inspected: when `want` was met early, only up to the last occurrence returned.
	return { ids, traceIds, scanned: reachedWant ? lastFoundAt + 1 : window.length, exhausted: !reachedWant && exhausted, continueBefore };
}

/** Per-occurrence status for a page of listed occurrences: one statement. Occurrences without a canonical trace id are absent. */
export async function statusByOccurrence(
	occurrences: Array<{ id: string; traceId: string | null }>,
	scope: SpanScope
): Promise<Record<string, SpansStatus>> {
	const statuses = await getSpansStatuses(occurrences.map((o) => o.traceId), scope);
	const out: Record<string, SpansStatus> = {};
	for (const o of occurrences) {
		const s = o.traceId ? statuses.get(o.traceId) : undefined;
		if (s) out[o.id] = s;
	}
	return out;
}
