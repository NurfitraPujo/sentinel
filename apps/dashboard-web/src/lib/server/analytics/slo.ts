import type { ServiceSli } from '../sli/config';
import {
	BURN_HOURS, HOUR_MS, bucketsFor, budgetLeft, burnDown, burnRate, evaluateStatus, latencyThreshold, sumBuckets, windowEnd,
	type BurnPoint, type HourBucket, type HourlyInput, type ObjectiveKind, type SloStatus,
} from '../../slo-status';
import type { SliSums } from './metrics';

/**
 * sentinel-dashboard-metrics-improvement D2/D3/D11: turns `sli_hourly` sums and rows into the status table's rows and the
 * burn-down charts. Pure: the service reads the data (two queries for every visible SLI), this shapes it.
 */

export interface SloRow {
	key: string;
	sliId: string;
	kind: ObjectiveKind;
	sliKind: ServiceSli['kind'];
	project: string;
	/** Plain words, e.g. "requests not 5xx at the edge". */
	objective: string;
	/** Latency rows: the threshold in words ("< 1 s"). */
	threshold: string | null;
	/** What "failed" means in this row, e.g. "5xx at edge". */
	failedLabel: string;
	windowDays: number;
	target: number | null;
	/** Good ratio of the latest complete `sli_hourly` hour, with that hour (ISO). */
	lastHour: { ratio: number | null; hourStart: string | null };
	/** Good ratio over the selected range, with the failed and total counts behind it. */
	range: { ratio: number | null; failed: number; total: number };
	budgetLeft: number | null;
	burn: { h1: number | null; h6: number | null; h24: number | null };
	status: SloStatus;
}

export interface BudgetChart {
	key: string;
	title: string;
	windowDays: number;
	target: number;
	/** Window bounds, epoch ms: the window ends now and starts `windowDays` earlier. */
	windowStart: number;
	windowEnd: number;
	points: BurnPoint[] | null;
}

export const ratioOf = (s: { total: number; bad: number }): number | null => (s.total === 0 ? null : 1 - s.bad / s.total);

function objectiveWords(sli: ServiceSli, kind: ObjectiveKind): { objective: string; threshold: string | null; failedLabel: string } {
	if (kind === 'latency') {
		const threshold = latencyThreshold(sli.latency!.le);
		return { objective: `requests answered in ${threshold} at the edge`, threshold, failedLabel: `slower than ${threshold.replace(/^< /, '')}` };
	}
	switch (sli.kind) {
		case 'http': return { objective: 'requests not 5xx at the edge', threshold: null, failedLabel: '5xx at edge' };
		case 'jobs': return { objective: 'job runs that did not fail', threshold: null, failedLabel: 'failed runs' };
		case 'nats': return { objective: 'NATS handler runs that did not fail', threshold: null, failedLabel: 'failed runs' };
	}
}

/** The objectives an SLI has, in row order: availability, then latency; an SLI with no target still gets one row. */
export function objectivesOf(sli: ServiceSli): { kind: ObjectiveKind; target: number | null }[] {
	const out: { kind: ObjectiveKind; target: number | null }[] = [];
	if (sli.availabilityTarget !== undefined) out.push({ kind: 'availability', target: sli.availabilityTarget });
	if (sli.kind === 'http' && sli.latency) out.push({ kind: 'latency', target: sli.latency.target });
	if (out.length === 0) out.push({ kind: 'availability', target: null });
	return out;
}

export function buildSlo(i: {
	slis: readonly ServiceSli[];
	/** Σ over the selected range, per SLI. */
	rangeSums: readonly SliSums[];
	/** Hourly rows covering the longest budget window up to `now`. */
	hourly: readonly HourlyInput[];
	now: Date;
}): { rows: SloRow[]; charts: BudgetChart[] } {
	const rows: SloRow[] = [];
	const charts: BudgetChart[] = [];
	const lastHourStart = Math.floor(i.now.getTime() / HOUR_MS) * HOUR_MS - HOUR_MS;
	for (const sli of i.slis) {
		const winStart = i.now.getTime() - sli.windowDays * 86_400_000;
		const own = i.hourly.filter((h) => h.sliId === sli.id && h.hourStart.getTime() >= winStart && h.hourStart.getTime() < i.now.getTime());
		const sums = i.rangeSums.find((s) => s.sliId === sli.id);
		for (const { kind, target } of objectivesOf(sli)) {
			const buckets: HourBucket[] = bucketsFor(own, kind);
			const words = objectiveWords(sli, kind);
			const key = `${sli.id}:${kind}`;
			const end = windowEnd(i.now, buckets.some((b) => b.hourStart.getTime() === lastHourStart));
			const [h1, h6, h24] = BURN_HOURS.map((h) => burnRate(buckets, target, end, h));
			const budget = budgetLeft(sumBuckets(buckets), target);
			const complete = buckets.filter((b) => b.hourStart.getTime() + HOUR_MS <= i.now.getTime());
			const last = complete.reduce<HourBucket | null>((a, b) => (a === null || b.hourStart > a.hourStart ? b : a), null);
			const rangeTotals = !sums
				? { total: 0, bad: 0 }
				: kind === 'availability' ? { total: sums.total, bad: sums.errors }
				: sums.fast === null ? { total: 0, bad: 0 } : { total: sums.totalWithFast, bad: sums.totalWithFast - sums.fast };
			rows.push({
				key, sliId: sli.id, kind, sliKind: sli.kind, project: sli.project, ...words, windowDays: sli.windowDays, target,
				lastHour: { ratio: last ? ratioOf(last) : null, hourStart: last ? last.hourStart.toISOString() : null },
				range: { ratio: ratioOf(rangeTotals), failed: rangeTotals.bad, total: rangeTotals.total },
				budgetLeft: budget,
				burn: { h1, h6, h24 },
				status: evaluateStatus({ hasTarget: target !== null, budget, burn1h: h1, burn6h: h6, burn24h: h24 }),
			});
			if (target !== null) {
				charts.push({
					key, title: `${sli.id} · ${words.objective} · ${sli.windowDays}-day budget`, windowDays: sli.windowDays, target,
					windowStart: winStart, windowEnd: i.now.getTime(), points: burnDown(buckets, target),
				});
			}
		}
	}
	return { rows, charts };
}
