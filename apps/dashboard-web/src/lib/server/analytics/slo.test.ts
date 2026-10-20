import { describe, it, expect } from 'vitest';
import { buildSlo } from './slo';
import { HOUR_MS } from '../../slo-status';
import type { ServiceSli } from '../sli/config';

const NOW = new Date('2026-10-20T12:30:00Z');
const H = (h: number) => new Date(Math.floor(NOW.getTime() / HOUR_MS) * HOUR_MS - h * HOUR_MS);
const slis: ServiceSli[] = [
	{ id: 'core-api', kind: 'http', project: 'core', source: 'a', availabilityTarget: 0.995, latency: { le: '1000.0', target: 0.94 }, service: 'api', windowDays: 28 },
	{ id: 'core-nats', kind: 'nats', project: 'core', source: 'n', availabilityTarget: 0.995, windowDays: 14 },
	{ id: 'bare', kind: 'jobs', project: 'core', source: 'j', windowDays: 28 },
];
const hourly = (sliId: string, hoursAgo: number[], total: number, errors: number, fast: number | null = null) =>
	hoursAgo.map((h) => ({ sliId, hourStart: H(h), total, errors, fast }));

describe('buildSlo (D2)', () => {
	const rows = [
		...hourly('core-api', [1, 2, 3], 1000, 5, 950),
		...hourly('core-api', [0], 1000, 500, 100), // the in-progress hour: collected partially, never "the last hour"
		...hourly('core-nats', [1, 2], 1000, 100),
		...hourly('core-nats', [24 * 20], 1000, 0), // outside the 14-day window
		...hourly('bare', [1], 100, 1),
	];
	const rangeSums = [
		{ sliId: 'core-api', environment: 'production', total: 9000, errors: 90, fast: 8500, totalWithFast: 9000 },
		{ sliId: 'core-nats', environment: 'production', total: 5000, errors: 500, fast: null, totalWithFast: 0 },
	];
	const out = buildSlo({ slis, rangeSums, hourly: rows, now: NOW });

	it('has a row per objective, grouped by SLI in config order, untargeted SLIs included', () => {
		expect(out.rows.map((r) => r.key)).toEqual(['core-api:availability', 'core-api:latency', 'core-nats:availability', 'bare:availability']);
		const bare = out.rows.at(-1)!;
		expect(bare).toMatchObject({ target: null, budgetLeft: null });
		expect(bare.status.state).toBe('untracked');
		expect(bare.burn).toEqual({ h1: null, h6: null, h24: null });
	});

	it('names the objective and what "failed" means in each row', () => {
		const [avail, lat, nats] = out.rows;
		expect(avail).toMatchObject({ objective: 'requests not 5xx at the edge', failedLabel: '5xx at edge', threshold: null });
		expect(lat).toMatchObject({ objective: 'requests answered in < 1 s at the edge', failedLabel: 'slower than 1 s', threshold: '< 1 s' });
		expect(nats.objective).toBe('NATS handler runs that did not fail');
	});

	it('range cells follow the selected range; failed and total come from the same sums', () => {
		const [avail, lat] = out.rows;
		expect(avail.range).toEqual({ ratio: 1 - 90 / 9000, failed: 90, total: 9000 });
		expect(lat.range).toEqual({ ratio: 8500 / 9000, failed: 500, total: 9000 });
	});

	it('last hour is the latest complete hour with its start; burn windows count complete hours only', () => {
		const avail = out.rows[0];
		expect(avail.lastHour).toEqual({ ratio: 1 - 5 / 1000, hourStart: H(1).toISOString() });
		expect(avail.lastHour.ratio).not.toBe(1 - 500 / 1000); // not the in-progress hour
		expect(avail.burn.h1).toBeCloseTo(0.005 / 0.005); // 5/1000 over a 0.5% budget
		expect(out.rows[2].burn.h1).toBeCloseTo(0.1 / 0.005);
	});

	it('the budget window is the SLI\'s own windowDays, not 28', () => {
		const nats = out.rows[2];
		expect(nats.windowDays).toBe(14);
		expect(nats.budgetLeft).toBe(-1); // 200/2000 failed against a 0.5 % budget; the 20-days-old clean hour is excluded
		expect(out.charts.find((c) => c.key === 'core-nats:availability')!.windowDays).toBe(14);
		expect(out.charts.find((c) => c.key === 'core-nats:availability')!.windowEnd - out.charts.find((c) => c.key === 'core-nats:availability')!.windowStart).toBe(14 * 86_400_000);
	});

	it('the budget window excludes hours older than the SLI\'s own windowDays, on an UNFLOORED budget', () => {
		// 14-day window: 1/1000 in window; a 20-day-old hour with 4/1000 must not count. Over 28 days it would: 5/2000 -> 0.5
		const r = buildSlo({ slis: [slis[1]], rangeSums: [], hourly: [...hourly('core-nats', [1], 1000, 1), ...hourly('core-nats', [24 * 20], 1000, 4)], now: NOW });
		expect(r.rows[0].budgetLeft).toBeCloseTo(1 - 0.001 / 0.005); // 0.8
		expect(r.charts[0].points!.at(-1)!.v).toBeCloseTo(0.8);
		expect(r.charts[0].points).toHaveLength(1);
	});

	it('labels jobs and nats failures "failed runs" (D2)', () => {
		const r = buildSlo({ slis: [{ ...slis[1] }, { id: 'j', kind: 'jobs', project: 'core', source: 'j', availabilityTarget: 0.995, windowDays: 28 }], rangeSums: [], hourly: [], now: NOW });
		expect(r.rows.map((x) => x.failedLabel)).toEqual(['failed runs', 'failed runs']);
	});

	it('precedence: a breached SLI is Breached even though its last hour was clean', () => {
		const r = buildSlo({ slis: [slis[1]], rangeSums: [], hourly: [...hourly('core-nats', [5, 4, 3, 2], 1000, 100), ...hourly('core-nats', [1], 1000, 0)], now: NOW });
		expect(r.rows[0].burn.h1).toBe(0);
		expect(r.rows[0].status.state).toBe('breached');
	});

	it('an SLI with no rows is No data, with a chart that has no points', () => {
		const r = buildSlo({ slis: [slis[1]], rangeSums: [], hourly: [], now: NOW });
		expect(r.rows[0].status.state).toBe('no-data');
		expect(r.charts[0].points).toBeNull();
	});

	it('a chart exists for every targeted objective and ends at the table\'s budget left', () => {
		expect(out.charts.map((c) => c.key)).toEqual(['core-api:availability', 'core-api:latency', 'core-nats:availability']);
		for (const row of out.rows.filter((r) => r.target !== null)) {
			expect(out.charts.find((c) => c.key === row.key)!.points!.at(-1)!.v).toBe(row.budgetLeft);
		}
		expect(out.charts[0].title).toBe('core-api · requests not 5xx at the edge · 28-day budget');
	});
});
