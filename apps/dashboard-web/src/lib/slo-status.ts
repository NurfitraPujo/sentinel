/**
 * sentinel-dashboard-metrics-improvement D2-D6, D11: the SLO maths. Pure and client-safe: burn rate, budget left, the
 * status rules, the latency threshold wording and the burn-down series. Display only: no alert rule reads any of it.
 */

export const HOUR_MS = 3_600_000;

/**
 * Burn-rate thresholds. Source: Google SRE workbook, "Alerting on SLOs", multiwindow burn-rate alerts. 14.4× over 1 h
 * spends 2 % of a 30-day budget; 6× over 6 h spends 5 %. `sli_hourly` is hourly, so the workbook's 5-minute short
 * windows are unavailable and the latest complete hour stands in.
 */
export const BURN_FAST_1H = 14.4;
export const BURN_FAST_6H = 6;
/** Source: SRE workbook ticket-level burn (3× over 24 h = 10 % of a 30-day budget in a day). */
export const BURN_SLOW_24H = 3;
/** Amber when less than this fraction of the budget is left (contract D4 rule 4, decision Q2). */
export const BUDGET_LOW = 0.25;
/** A burn window with fewer events than this is "low traffic": shown as "—" and never feeds the status (contract D3). */
export const MIN_BURN_TOTAL = 100;
/** Budget left is floored here, like `errorBudgetRemaining` (−100 %). */
export const BUDGET_FLOOR = -1;
/** Collection runs hourly at :05; within this long after the hour a missing last-hour row is lateness, not a gap. */
export const COLLECTION_GRACE_MS = 15 * 60_000;
/** Burn windows in hours, as columns. */
export const BURN_HOURS = [1, 6, 24] as const;

/** The per-hour counts of one objective: `bad` is `errors` (availability) or `totalWithFast − fast` (latency). */
export interface HourBucket {
	hourStart: Date;
	total: number;
	bad: number;
}

export interface HourlyInput {
	sliId: string;
	hourStart: Date;
	total: number;
	errors: number;
	fast: number | null;
}

export type ObjectiveKind = 'availability' | 'latency';

export function bucketsFor(rows: readonly HourlyInput[], kind: ObjectiveKind): HourBucket[] {
	const out: HourBucket[] = [];
	for (const r of rows) {
		if (kind === 'availability') out.push({ hourStart: r.hourStart, total: r.total, bad: r.errors });
		else if (r.fast !== null) out.push({ hourStart: r.hourStart, total: r.total, bad: r.total - r.fast });
	}
	return out;
}

export const sumBuckets = (b: readonly HourBucket[]) => b.reduce((a, x) => ({ total: a.total + x.total, bad: a.bad + x.bad }), { total: 0, bad: 0 });

/** `1 − (bad/total)/(1 − target)`, floored at −100 %. null with no traffic or an invalid target. */
export function budgetLeft(s: { total: number; bad: number }, target: number | null): number | null {
	if (target === null || s.total === 0 || !(target > 0 && target < 1)) return null;
	return Math.max(BUDGET_FLOOR, 1 - s.bad / s.total / (1 - target));
}

/** The end of the last complete hour window: `now` floored to the hour, one hour earlier while the :05 run is late. */
export function windowEnd(now: Date, hasLastHour: boolean): Date {
	const floor = Math.floor(now.getTime() / HOUR_MS) * HOUR_MS;
	return new Date(!hasLastHour && now.getTime() - floor < COLLECTION_GRACE_MS ? floor - HOUR_MS : floor);
}

/** `(bad/total)/(1 − target)` over the `hours` complete hours ending at `end`. null below MIN_BURN_TOTAL events. */
export function burnRate(buckets: readonly HourBucket[], target: number | null, end: Date, hours: number): number | null {
	if (target === null || !(target > 0 && target < 1)) return null;
	const from = end.getTime() - hours * HOUR_MS;
	const s = sumBuckets(buckets.filter((b) => b.hourStart.getTime() >= from && b.hourStart.getTime() < end.getTime()));
	if (s.total < MIN_BURN_TOTAL) return null;
	return s.bad / s.total / (1 - target);
}

export type SloState = 'no-data' | 'breached' | 'burning-fast' | 'burning' | 'ok' | 'untracked';
export interface SloStatus {
	state: SloState;
	label: string;
	/** Which rule fired and its numbers, for the pill's `title`. */
	reason: string;
}

export const STATUS_LABELS: Record<SloState, string> = {
	'no-data': 'No data', breached: 'Breached', 'burning-fast': 'Burning fast', burning: 'Burning', ok: 'OK', untracked: 'Untracked',
};

const x = (v: number) => `${v.toFixed(1)}×`;
const mk = (state: SloState, reason: string): SloStatus => ({ state, label: STATUS_LABELS[state], reason });

/** D4, first rule that matches. Low-traffic (null) burn windows never fire a burn rule. */
export function evaluateStatus(i: { hasTarget: boolean; budget: number | null; burn1h: number | null; burn6h: number | null; burn24h: number | null }): SloStatus {
	if (!i.hasTarget) return mk('untracked', 'No target is configured for this objective');
	if (i.budget === null) return mk('no-data', 'No sli_hourly data in the budget window');
	if (i.budget <= 0) return mk('breached', `Budget left ${(i.budget * 100).toFixed(0)}% ≤ 0%`);
	if (i.burn1h !== null && i.burn1h >= BURN_FAST_1H) return mk('burning-fast', `1h burn ${x(i.burn1h)} ≥ ${BURN_FAST_1H}×`);
	if (i.burn6h !== null && i.burn6h >= BURN_FAST_6H) return mk('burning-fast', `6h burn ${x(i.burn6h)} ≥ ${BURN_FAST_6H}×`);
	if (i.burn24h !== null && i.burn24h >= BURN_SLOW_24H) return mk('burning', `24h burn ${x(i.burn24h)} ≥ ${BURN_SLOW_24H}×`);
	if (i.budget < BUDGET_LOW) return mk('burning', `Budget left ${(i.budget * 100).toFixed(0)}% < ${BUDGET_LOW * 100}%`);
	return mk('ok', `Budget left ${(i.budget * 100).toFixed(0)}%, burning within budget`);
}

/** D6: the `le` bucket in ms, spelled as the histogram spells it, as words: `"1000.0"` → `< 1 s`. */
export function latencyThreshold(le: string): string {
	const ms = Number(le);
	if (!Number.isFinite(ms)) return `< ${le} ms`;
	if (ms < 1000) return `< ${trim(ms)} ms`;
	if (ms < 60_000) return `< ${trim(ms / 1000)} s`;
	if (ms < 3_600_000) return `< ${trim(ms / 60_000)} min`;
	return `< ${trim(ms / 3_600_000)} h`;
}
const trim = (n: number) => String(Number(n.toFixed(2)));

/** The page-wide SSE caveat, now on the latency rows it is about (D10). Item F removes it. */
export const SSE_CAVEAT =
	'The edge sees long-lived streams as slow requests and has no route label to exclude them, so latency reads lower than ordinary requests feel.';

export interface BurnPoint {
	/** Epoch ms of the END of the hour the point covers. */
	t: number;
	/** Budget left after that hour, floored at −100 %. */
	v: number;
}

/**
 * D11: `1 − cumBad(t)/((1 − target)·Σtotal_window)` for each hour. The denominator is the WHOLE window's traffic, so
 * the last point equals `budgetLeft` over the same window (a test asserts it). null with no traffic.
 */
export function burnDown(buckets: readonly HourBucket[], target: number | null): BurnPoint[] | null {
	const sorted = [...buckets].sort((a, b) => a.hourStart.getTime() - b.hourStart.getTime());
	const all = sumBuckets(sorted);
	if (target === null || !(target > 0 && target < 1) || all.total === 0) return null;
	let cum = 0;
	return sorted.map((b) => {
		cum += b.bad;
		// same operation order as budgetLeft, so the last point is identical to it
		return { t: b.hourStart.getTime() + HOUR_MS, v: Math.max(BUDGET_FLOOR, 1 - cum / all.total / (1 - target)) };
	});
}
