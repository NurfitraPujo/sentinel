import { describe, it, expect } from 'vitest';
import {
	BUDGET_LOW, BURN_FAST_1H, BURN_FAST_6H, BURN_SLOW_24H, HOUR_MS, MIN_BURN_TOTAL, bucketsFor, budgetLeft, burnDown, burnRate, evaluateStatus,
	latencyThreshold, sumBuckets, windowEnd, type HourBucket,
} from './slo-status';
import { EDGE_LATENCY_BUCKETS } from './server/sli/config';
import { errorBudgetRemaining } from './server/analytics/metrics';

const T0 = Date.UTC(2026, 9, 20, 12, 0, 0);
const hr = (h: number, total: number, bad: number): HourBucket => ({ hourStart: new Date(T0 - h * HOUR_MS), total, bad });
const END = new Date(T0);

describe('burnRate (D3)', () => {
	it('is (bad/total)/(1 − target) over the complete hours ending at `end`', () => {
		const b = [hr(1, 1000, 10), hr(2, 1000, 0), hr(7, 1000, 100)];
		expect(burnRate(b, 0.99, END, 1)).toBeCloseTo(1); // 1% bad against a 1% budget
		expect(burnRate(b, 0.99, END, 6)).toBeCloseTo(0.5);
		expect(burnRate(b, 0.99, END, 24)).toBeCloseTo((110 / 3000) / 0.01);
	});
	it('is null below the low-traffic guard, and at exactly the guard it counts', () => {
		expect(burnRate([hr(1, MIN_BURN_TOTAL - 1, 50)], 0.99, END, 1)).toBeNull();
		expect(burnRate([hr(1, MIN_BURN_TOTAL, 1)], 0.99, END, 1)).toBeCloseTo(1);
	});
	it('ignores hours outside the window, including the not-yet-complete current hour', () => {
		expect(burnRate([hr(0, 1000, 500), hr(2, 1000, 0)], 0.99, END, 1)).toBeNull();
	});
	it('is null with no usable target', () => {
		expect(burnRate([hr(1, 1000, 1)], null, END, 1)).toBeNull();
	});
	it('windowEnd steps back an hour while the :05 collection run is late, and not otherwise', () => {
		const at = (min: number) => new Date(T0 + min * 60_000);
		expect(windowEnd(at(3), false).getTime()).toBe(T0 - HOUR_MS);
		expect(windowEnd(at(3), true).getTime()).toBe(T0);
		expect(windowEnd(at(40), false).getTime()).toBe(T0); // a real gap, not lateness
	});
});

describe('evaluateStatus (D4)', () => {
	const base: Parameters<typeof evaluateStatus>[0] = { hasTarget: true, budget: 0.9, burn1h: 0.2, burn6h: 0.2, burn24h: 0.2 };
	const st = (o: Partial<typeof base>) => evaluateStatus({ ...base, ...o }).state;
	it('1: no data when there is no budget to compute', () => expect(st({ budget: null })).toBe('no-data'));
	it('2: breached at budget ≤ 0, including exactly 0', () => {
		expect(st({ budget: 0 })).toBe('breached');
		expect(st({ budget: -1 })).toBe('breached');
	});
	it('precedence: a breached SLI with a quiet hour is still Breached', () => {
		expect(st({ budget: -0.2, burn1h: 0, burn6h: 0, burn24h: 0 })).toBe('breached');
		expect(st({ budget: -0.2, burn1h: 20 })).toBe('breached'); // breached outranks burning fast
	});
	it('3: burning fast on 1h ≥ 14.4 or 6h ≥ 6, with the numbers in the reason', () => {
		expect(st({ burn1h: BURN_FAST_1H })).toBe('burning-fast');
		expect(st({ burn1h: BURN_FAST_1H - 0.1 })).toBe('ok');
		const r = evaluateStatus({ ...base, burn6h: 7.1 });
		expect(r.state).toBe('burning-fast');
		expect(r.reason).toBe('6h burn 7.1× ≥ 6×');
		expect(st({ burn6h: BURN_FAST_6H })).toBe('burning-fast');
	});
	it('4: burning on 24h ≥ 3 or budget < 25 %', () => {
		expect(st({ burn24h: BURN_SLOW_24H })).toBe('burning');
		expect(st({ budget: BUDGET_LOW - 0.01 })).toBe('burning');
		expect(st({ budget: BUDGET_LOW })).toBe('ok');
	});
	it('fast burn outranks slow burn', () => expect(st({ burn1h: 15, burn24h: 4 })).toBe('burning-fast'));
	it('5: OK otherwise, and a low-traffic (null) window never fires a rule', () => {
		expect(st({})).toBe('ok');
		expect(st({ burn1h: null, burn6h: null, burn24h: null })).toBe('ok');
	});
	it('an objective without a target is Untracked', () => expect(st({ hasTarget: false, budget: null })).toBe('untracked'));
	it('every state has a word, never colour alone', () => {
		for (const o of [{ budget: null }, { budget: 0 }, { burn1h: 20 }, { burn24h: 4 }, {}, { hasTarget: false }]) {
			expect(evaluateStatus({ ...base, ...o }).label).toMatch(/^[A-Z]/);
		}
	});
});

describe('latency budget (D6)', () => {
	it('is 1 − (bad/totalWithFast)/(1 − latency target), bad = totalWithFast − fast, floored at −100 %', () => {
		const rows = [{ sliId: 'a', hourStart: new Date(T0), total: 1000, errors: 0, fast: 940 }, { sliId: 'a', hourStart: new Date(T0 - HOUR_MS), total: 1000, errors: 0, fast: 980 }];
		const b = bucketsFor(rows, 'latency');
		expect(sumBuckets(b)).toEqual({ total: 2000, bad: 80 });
		expect(budgetLeft(sumBuckets(b), 0.94)).toBeCloseTo(1 - 0.04 / 0.06);
		expect(budgetLeft({ total: 1000, bad: 900 }, 0.94)).toBe(-1);
	});
	it('uses latencyTotal as the denominator and falls back to total for rows written before it existed', () => {
		const rows = [
			{ sliId: 'a', hourStart: new Date(T0), total: 1000, errors: 0, fast: 810, latencyTotal: 900 }, // 100 SSE requests in total
			{ sliId: 'a', hourStart: new Date(T0 - HOUR_MS), total: 1000, errors: 0, fast: 940, latencyTotal: null },
			{ sliId: 'a', hourStart: new Date(T0 - 2 * HOUR_MS), total: 1000, errors: 0, fast: 940 },
		];
		const b = bucketsFor(rows, 'latency');
		expect(b.map((x) => x.total)).toEqual([900, 1000, 1000]);
		expect(sumBuckets(b)).toEqual({ total: 2900, bad: 90 + 60 + 60 });
		expect(bucketsFor(rows, 'availability').map((x) => x.total)).toEqual([1000, 1000, 1000]); // availability keeps total
	});
	it('is null when no hour carries fast', () => {
		const b = bucketsFor([{ sliId: 'a', hourStart: new Date(T0), total: 1000, errors: 0, fast: null }], 'latency');
		expect(b).toEqual([]);
		expect(budgetLeft(sumBuckets(b), 0.94)).toBeNull();
	});
	it('availability budget agrees with metrics.errorBudgetRemaining', () => {
		expect(budgetLeft({ total: 5000, bad: 20 }, 0.995)).toBe(errorBudgetRemaining({ total: 5000, errors: 20 }, 0.995));
		expect(budgetLeft({ total: 5000, bad: 5000 }, 0.995)).toBe(errorBudgetRemaining({ total: 5000, errors: 5000 }, 0.995));
	});
});

describe('latencyThreshold (D6)', () => {
	it('words the examples in the contract', () => {
		expect(latencyThreshold('1000.0')).toBe('< 1 s');
		expect(latencyThreshold('250.0')).toBe('< 250 ms');
		expect(latencyThreshold('2500.0')).toBe('< 2.5 s');
	});
	it('words every bucket the edge histogram has', () => {
		const words = EDGE_LATENCY_BUCKETS.map(latencyThreshold);
		expect(words).toEqual([
			'< 0.5 ms', '< 1 ms', '< 5 ms', '< 10 ms', '< 25 ms', '< 50 ms', '< 100 ms', '< 250 ms', '< 500 ms', '< 1 s', '< 2.5 s', '< 5 s',
			'< 10 s', '< 30 s', '< 1 min', '< 5 min', '< 10 min', '< 30 min', '< 1 h',
		]);
		expect(new Set(words).size).toBe(EDGE_LATENCY_BUCKETS.length);
	});
});

describe('burnDown (D11)', () => {
	it('ends exactly at the budget left over the same window', () => {
		const b = [hr(5, 1000, 1), hr(4, 1000, 8), hr(3, 1000, 0), hr(2, 1000, 20), hr(1, 1000, 2)];
		const pts = burnDown(b, 0.995)!;
		expect(pts).toHaveLength(5);
		expect(pts.at(-1)!.v).toBe(budgetLeft(sumBuckets(b), 0.995));
		expect(pts.at(-1)!.v).toBe(errorBudgetRemaining({ total: 5000, errors: 31 }, 0.995));
		expect(pts[0].t).toBe(T0 - 4 * HOUR_MS); // a point sits at the end of its hour
	});
	it('goes below zero when the budget is overspent and floors at −100 %', () => {
		const pts = burnDown([hr(2, 1000, 0), hr(1, 1000, 1000)], 0.99)!;
		expect(pts[0].v).toBe(1);
		expect(pts[1].v).toBe(-1);
		expect(burnDown([hr(1, 1000, 20)], 0.99)!.at(-1)!.v).toBeCloseTo(-1);
		expect(burnDown([hr(1, 1000, 15)], 0.99)!.at(-1)!.v).toBeCloseTo(-0.5);
	});
	it('is null with no traffic', () => expect(burnDown([], 0.99)).toBeNull());
});
