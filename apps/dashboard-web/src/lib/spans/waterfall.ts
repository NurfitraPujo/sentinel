/**
 * sentinel-span-ingest D6 — the pure layout of the trace page's span waterfall: spans grouped into lanes by
 * service, nested by `parentSpanId`, laid out on one time axis, with each error occurrence pinned to the span
 * that carries its `spanId`. No DOM, no I/O: `SpanWaterfall.svelte` renders what this returns, and the nesting,
 * orphan, pinning and cap rules are tested here.
 *
 * Not under `$lib/server/` because the component (client code) imports it.
 */

/** D6: more than this and the page renders the first 2,000 by start time and says so. */
export const SPAN_RENDER_CAP = 2000;
/** D1: spans are kept 14 days (errors stay on the occurrence retention window). */
export const SPAN_RETENTION_DAYS = 14;

export interface WaterfallSpan {
	spanId: string;
	parentSpanId: string;
	name: string;
	op: string;
	kind: string;
	source: 'mobile' | 'pulled' | string;
	serviceName: string;
	projectName: string | null;
	environment: string;
	/** Microseconds since the epoch — numbers, not Dates, so the axis arithmetic is exact. */
	startUs: number;
	durationUs: number;
	status: string;
	attributes: Record<string, unknown>;
}

export interface WaterfallError {
	id: string;
	spanId: string | null;
	/** The client's event time, ISO-8601. */
	createdAt: string | Date | null;
	errorClass: string | null;
	message: string | null;
	issueId: string;
}

export interface PinnedError {
	error: WaterfallError;
	/** Where the error sits on the axis, percent of the axis. */
	offsetPct: number;
}

export interface WaterfallRow {
	span: WaterfallSpan;
	/** Indentation within the lane: the number of the span's ancestors that are in the same lane. */
	depth: number;
	offsetPct: number;
	/** At least `MIN_WIDTH_PCT`, so a 5 µs span is still a visible, focusable bar. */
	widthPct: number;
	failed: boolean;
	errors: PinnedError[];
}

export interface WaterfallLane {
	key: string;
	label: string;
	source: string;
	rows: WaterfallRow[];
}

export interface Waterfall {
	lanes: WaterfallLane[];
	/** Errors with no matching span in the set (or no spanId), placed by time. */
	unmatchedErrors: PinnedError[];
	/** The axis, microseconds since the epoch. */
	startUs: number;
	endUs: number;
	spanCount: number;
	/** Spans past `SPAN_RENDER_CAP` that were not laid out. */
	truncated: number;
}

export const MIN_WIDTH_PCT = 0.4;

/** Status values that are not a failure. Anything else (an OTLP `error`, a Sentry `internal_error`, …) is. */
const OK_STATUSES = new Set(['', 'ok', 'unset', 'cancelled']);
export const isFailedStatus = (status: string): boolean => !OK_STATUSES.has(status.toLowerCase());

function toUs(v: string | Date | null): number | null {
	if (v === null || v === undefined) return null;
	const ms = v instanceof Date ? v.getTime() : Date.parse(v);
	return Number.isFinite(ms) ? ms * 1000 : null;
}

const laneKeyOf = (s: WaterfallSpan): string => `${s.source}:${s.serviceName || s.projectName || ''}`;
const laneLabelOf = (s: WaterfallSpan): string => s.serviceName || s.projectName || (s.source === 'mobile' ? 'mobile' : 'unknown service');

/**
 * Lays out `spans` (any order) and `errors`. `cap` is injectable so the cap rule is testable without 2,001 rows.
 */
export function buildWaterfall(spans: WaterfallSpan[], errors: WaterfallError[], cap: number = SPAN_RENDER_CAP): Waterfall {
	const sorted = [...spans].sort((a, b) => a.startUs - b.startUs || (a.spanId < b.spanId ? -1 : a.spanId > b.spanId ? 1 : 0));
	const truncated = Math.max(0, sorted.length - cap);
	const kept = truncated > 0 ? sorted.slice(0, cap) : sorted;

	// The first stored span of an id is the parent of its children; a duplicate id (the same span written twice
	// with different start times) must not make the tree ambiguous.
	const byId = new Map<string, WaterfallSpan>();
	for (const s of kept) if (!byId.has(s.spanId)) byId.set(s.spanId, s);

	// A span whose parent is not in the set (not stored, filtered out, past the cap, or the browser's unrecorded
	// span) is a root, as is one whose parent chain loops back on itself.
	const parentOf = (s: WaterfallSpan): WaterfallSpan | null => {
		if (!s.parentSpanId || s.parentSpanId === s.spanId) return null;
		return byId.get(s.parentSpanId) ?? null;
	};
	const inLaneAncestorDepth = (s: WaterfallSpan): number => {
		const lane = laneKeyOf(s);
		let depth = 0;
		const seen = new Set<WaterfallSpan>([s]);
		let p = parentOf(s);
		while (p && !seen.has(p)) {
			seen.add(p);
			if (laneKeyOf(p) === lane) depth += 1;
			p = parentOf(p);
		}
		return depth;
	};

	// Order within a lane: a depth-first walk of the tree (children after their parent, by start time), so nesting
	// reads top-down. Built from `kept`, which is already start-time ordered.
	const children = new Map<WaterfallSpan, WaterfallSpan[]>();
	const roots: WaterfallSpan[] = [];
	for (const s of kept) {
		const p = parentOf(s);
		if (p) {
			const list = children.get(p);
			if (list) list.push(s);
			else children.set(p, [s]);
		} else roots.push(s);
	}
	const order: WaterfallSpan[] = [];
	const visited = new Set<WaterfallSpan>();
	const walk = (s: WaterfallSpan) => {
		if (visited.has(s)) return;
		visited.add(s);
		order.push(s);
		for (const c of children.get(s) ?? []) walk(c);
	};
	for (const r of roots) walk(r);
	// A cycle has no root, so its members were never reached: lay them out as roots in start order rather than lose them.
	for (const s of kept) walk(s);

	// No spans, no waterfall: errors alone are the occurrence list below, not a chart of one lane.
	if (kept.length === 0) return { lanes: [], unmatchedErrors: [], startUs: 0, endUs: 0, spanCount: 0, truncated };

	// The axis: every span's extent plus every error's time that lands on it.
	let startUs = Infinity;
	let endUs = -Infinity;
	for (const s of kept) {
		startUs = Math.min(startUs, s.startUs);
		endUs = Math.max(endUs, s.startUs + s.durationUs);
	}
	const errorTimes = errors.map((e) => toUs(e.createdAt));
	for (const t of errorTimes) {
		if (t === null) continue;
		startUs = Math.min(startUs, t);
		endUs = Math.max(endUs, t);
	}
	const axis = Math.max(endUs - startUs, 1);
	const pct = (us: number) => ((us - startUs) / axis) * 100;

	const pinned = new Map<string, PinnedError[]>();
	const unmatchedErrors: PinnedError[] = [];
	errors.forEach((e, i) => {
		const t = errorTimes[i];
		const offsetPct = t === null ? 0 : pct(t);
		const entry: PinnedError = { error: e, offsetPct };
		if (e.spanId && byId.has(e.spanId)) {
			const list = pinned.get(e.spanId);
			if (list) list.push(entry);
			else pinned.set(e.spanId, [entry]);
		} else unmatchedErrors.push(entry);
	});
	unmatchedErrors.sort((a, b) => a.offsetPct - b.offsetPct);

	const lanes = new Map<string, WaterfallLane>();
	for (const s of order) {
		const key = laneKeyOf(s);
		let lane = lanes.get(key);
		if (!lane) {
			lane = { key, label: laneLabelOf(s), source: s.source, rows: [] };
			lanes.set(key, lane);
		}
		lane.rows.push({
			span: s,
			depth: inLaneAncestorDepth(s),
			offsetPct: pct(s.startUs),
			widthPct: Math.max((s.durationUs / axis) * 100, MIN_WIDTH_PCT),
			failed: isFailedStatus(s.status),
			// Only the first span of a duplicated id carries the pin, so an error is shown once.
			errors: byId.get(s.spanId) === s ? (pinned.get(s.spanId) ?? []) : [],
		});
	}

	return { lanes: [...lanes.values()], unmatchedErrors, startUs, endUs, spanCount: kept.length, truncated };
}

/** "1.25 ms", "340 µs", "2.4 s" — the unit that keeps the number readable. */
export function formatDuration(us: number): string {
	if (!Number.isFinite(us) || us < 0) return '—';
	if (us < 1000) return `${Math.round(us)} µs`;
	if (us < 1_000_000) return `${+(us / 1000).toFixed(us < 10_000 ? 2 : 1)} ms`;
	return `${+(us / 1_000_000).toFixed(2)} s`;
}
