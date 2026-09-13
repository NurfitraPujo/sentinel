/**
 * sentinel-issue-trend §3 ("Windows") — the two read windows over `issue_occurrence_buckets`, as
 * pure arithmetic on ONE instant, so a loader takes `now` once and both of its queries (and, on
 * `/`, the `new=24h` bound beside them) agree on what "now" is.
 *
 * Every window is in UTC hours: the processor writes `bucket_start` as the event timestamp
 * truncated to the UTC hour (§3, decision 2), so `trunc(now)` here is the same truncation. Both
 * windows END at the same `windowEnd = trunc(now)` — the current, partial hour — so the two headings
 * on the detail page agree. Both are bounded at BOTH ends by the readers (§3, decision 18: a
 * future-dated bucket row can exist and must neither extend a series nor be summed into its last
 * slot), which is why `windowEnd` is a value and not "everything after `windowStart`".
 *
 * This is its own module, not part of `issue-queries.ts`, because the loaders import it for real
 * while their tests replace `issue-queries` wholesale (`vi.mock` with a factory): a window helper
 * exported from the query module would be `undefined` under that mock.
 */
export const HOUR_MS = 60 * 60 * 1000;
export const DAY_MS = 24 * HOUR_MS;

/** §4.1 / §4.2: 24 hourly buckets ending with the current hour. */
export const TREND_HOURLY_SLOTS = 24;
/** §4.2, decision 11: 30 UTC calendar days ending with today (UTC). */
export const TREND_DAILY_SLOTS = 30;

/** `trunc(now)` in the contract's terms: the start of the UTC hour `instant` falls in. */
export function truncateToUtcHour(instant: Date): Date {
	return new Date(Math.floor(instant.getTime() / HOUR_MS) * HOUR_MS);
}

/** `00:00:00Z` of the UTC calendar day `instant` falls in. */
export function utcDayStart(instant: Date): Date {
	return new Date(Math.floor(instant.getTime() / DAY_MS) * DAY_MS);
}

export interface TrendWindow {
	/** Inclusive: the first bucket's start. */
	windowStart: Date;
	/** Inclusive: `trunc(now)`, the current partial hour's start. */
	windowEnd: Date;
}

/** §4.1 — the list's per-card window: `[trunc(now) − 23h … trunc(now)]`, 24 hourly slots. */
export function listTrendWindow(now: Date): TrendWindow {
	const windowEnd = truncateToUtcHour(now);
	return {
		windowStart: new Date(windowEnd.getTime() - (TREND_HOURLY_SLOTS - 1) * HOUR_MS),
		windowEnd,
	};
}

/**
 * §4.2 — the detail's window: `00:00:00Z` of (UTC today − 29 days) through `trunc(now)`, ≤ 720
 * rows — 29 full UTC days plus today's hours so far. One query feeds both series (decision 11).
 */
export function detailTrendWindow(now: Date): TrendWindow {
	const windowEnd = truncateToUtcHour(now);
	return {
		windowStart: new Date(utcDayStart(windowEnd).getTime() - (TREND_DAILY_SLOTS - 1) * DAY_MS),
		windowEnd,
	};
}
