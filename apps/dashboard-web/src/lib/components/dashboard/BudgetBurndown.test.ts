import { describe, it, expect } from 'vitest';
import { render, screen, within } from '@testing-library/svelte';
import BudgetBurndown from './BudgetBurndown.svelte';
import { HOUR_MS, bucketsFor, budgetLeft, burnDown, sumBuckets } from '$lib/slo-status';
import { errorBudgetRemaining } from '$lib/server/analytics/metrics';

// sentinel-dashboard-metrics-improvement D11: the burn-down chart.
const NOW = Date.UTC(2026, 9, 20, 12, 0, 0);
const DAY = 86_400_000;
const hourly = (n: number, errPerHour: (i: number) => number) =>
	Array.from({ length: n }, (_, i) => ({ sliId: 's', hourStart: new Date(NOW - (n - i) * HOUR_MS), total: 1000, errors: errPerHour(i), fast: null }));
const chartFor = (rows: ReturnType<typeof hourly>, windowDays = 28, target = 0.995) => ({
	key: 's:availability', title: `s · requests not 5xx at the edge · ${windowDays}-day budget`, windowDays, target,
	windowStart: NOW - windowDays * DAY, windowEnd: NOW, points: burnDown(bucketsFor(rows, 'availability'), target),
});
const cy = (id: string) => Number(screen.getByTestId(id).getAttribute('cy'));
const y1 = (id: string) => Number(screen.getByTestId(id).getAttribute('y1'));

describe('BudgetBurndown', () => {
	it('draws the 0% line emphasised and the dashed ideal from 100% at the window start to 0% at its end', () => {
		render(BudgetBurndown, { charts: [chartFor(hourly(48, () => 1))] });
		const ideal = screen.getByTestId('budget-s:availability-ideal');
		const zero = screen.getByTestId('budget-s:availability-zero');
		expect(zero.getAttribute('class')).toContain('zero');
		expect(ideal.getAttribute('class')).toContain('ideal');
		// the window's start is the plot's left edge, now is its right edge; 100% is above 0%
		expect(Number(ideal.getAttribute('x1'))).toBeLessThan(Number(ideal.getAttribute('x2')));
		expect(Number(ideal.getAttribute('y1'))).toBeLessThan(Number(ideal.getAttribute('y2')));
		expect(Number(ideal.getAttribute('y2'))).toBe(Number(zero.getAttribute('y1'))); // ends ON the zero line
	});

	it('its last point is the table\'s budget left (formatted), and the numbers equal errorBudgetRemaining', () => {
		const rows = hourly(48, (i) => (i % 5 === 0 || i === 47 ? 3 : 0)); // the last hour burns too, so the last point moves
		const c = chartFor(rows);
		render(BudgetBurndown, { charts: [c] });
		const sums = sumBuckets(bucketsFor(rows, 'availability'));
		expect(c.points!.at(-1)!.v).toBeLessThan(c.points!.at(-2)!.v);
		expect(c.points!.at(-1)!.v).toBe(errorBudgetRemaining({ total: sums.total, errors: sums.bad }, 0.995));
		expect(c.points!.at(-1)!.v).toBe(budgetLeft(sums, 0.995));
		expect(screen.getByTestId('budget-s:availability-now').textContent).toBe(`${Math.round(c.points!.at(-1)!.v * 100)}% left`);
	});

	it('draws mid-window points at their own budget, not just the end', () => {
		// 5 errors/h for the first 24 h, 10/h for the next 24 h: 48 000 requests, 0.5 % budget = 240 errors
		const c = chartFor(hourly(48, (i) => (i < 24 ? 5 : 10)));
		expect(c.points![23].v).toBeCloseTo(0.5); // 120 spent
		expect(c.points!.at(-1)!.v).toBeCloseTo(-0.5); // 360 spent
		render(BudgetBurndown, { charts: [c] });
		const d = screen.getByTestId('budget-s:availability-line').getAttribute('d')!;
		const ys = [...d.matchAll(/[ML][\d.]+,(-?[\d.]+)/g)].map((m) => Number(m[1]));
		expect(ys).toHaveLength(48);
		expect(ys[23]).toBeCloseTo(8 + (1 - 0.5) / 2 * 94, 1); // PAD_T + (1 - v) / 2 * plotH
		expect(ys[47]).toBeCloseTo(8 + (1 + 0.5) / 2 * 94, 1);
	});

	it('ticks every day and labels weekly on a 28-day chart; a 14-day chart labels every other day', () => {
		const a = render(BudgetBurndown, { charts: [chartFor(hourly(24, () => 0), 28)] });
		expect(screen.getAllByTestId('budget-s:availability-tick')).toHaveLength(29);
		expect(screen.getAllByTestId('budget-s:availability-tick-label')).toHaveLength(5);
		a.unmount();
		render(BudgetBurndown, { charts: [chartFor(hourly(24, () => 0), 14)] });
		expect(screen.getAllByTestId('budget-s:availability-tick')).toHaveLength(15);
		expect(screen.getAllByTestId('budget-s:availability-tick-label')).toHaveLength(8);
	});

	it('draws a negative budget below the zero line, in the breached colour', () => {
		render(BudgetBurndown, { charts: [chartFor(hourly(24, () => 50))] });
		expect(cy('budget-s:availability-end')).toBeGreaterThan(y1('budget-s:availability-zero'));
		expect(screen.getByTestId('budget-s:availability-end').getAttribute('class')).toContain('over');
		expect(screen.getByTestId('budget-s:availability-now').textContent).toBe('-100% left');
	});

	it('draws a healthy budget above the zero line', () => {
		render(BudgetBurndown, { charts: [chartFor(hourly(24, () => 0))] });
		expect(cy('budget-s:availability-end')).toBeLessThan(y1('budget-s:availability-zero'));
	});

	it('titles the chart with the objective and the SLI\'s own window, and says it ignores the range selector', () => {
		render(BudgetBurndown, { charts: [chartFor(hourly(24, () => 0), 14)] });
		expect(screen.getByTestId('budget-s:availability').textContent).toContain('s · requests not 5xx at the edge · 14-day budget');
		expect(screen.getByTestId('budget-note').textContent).toContain('does not follow the range selector');
	});

	it('carries a daily table of the same numbers for screen readers, and says so when there is no data', () => {
		const a = render(BudgetBurndown, { charts: [chartFor(hourly(72, () => 0))] });
		const rows = within(screen.getByTestId('budget-s:availability-table')).getAllByRole('row');
		expect(rows.length).toBeGreaterThan(1);
		expect(screen.getByTestId('budget-s:availability').querySelector('svg')!.getAttribute('aria-hidden')).toBe('true');
		a.unmount();
		render(BudgetBurndown, { charts: [chartFor([])] });
		expect(screen.getByTestId('budget-s:availability-empty')).toBeTruthy();
	});

	it('replaced the good-ratio sparklines', () => {
		render(BudgetBurndown, { charts: [chartFor(hourly(24, () => 0))] });
		expect(screen.queryByTestId(/sli-chart/)).toBeNull();
		expect(screen.queryByTestId('sli-section')).toBeNull();
	});
});
