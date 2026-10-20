import { describe, it, expect, vi } from 'vitest';
import { render, screen, within } from '@testing-library/svelte';
import SloTable from './SloTable.svelte';
import { SSE_CAVEAT, evaluateStatus } from '$lib/slo-status';

// sentinel-dashboard-metrics-improvement D2/D4/D10: the SLO status table renders from SloRow data.
const row = (o: Record<string, unknown> = {}) => {
	const burnIn = { h1: 0.2, h6: 0.4, h24: 0.5 };
	return {
		key: 'core-api:availability', sliId: 'core-api', kind: 'availability', sliKind: 'http', project: 'core',
		objective: 'requests not 5xx at the edge', threshold: null, failedLabel: '5xx at edge', windowDays: 28, target: 0.995,
		lastHour: { ratio: 0.999, hourStart: '2026-10-20T11:00:00.000Z' },
		range: { ratio: 0.9978, failed: 101, total: 457463 },
		budgetLeft: 0.56, burn: burnIn,
		status: evaluateStatus({ hasTarget: true, budget: 0.56, burn1h: burnIn.h1, burn6h: burnIn.h6, burn24h: burnIn.h24 }),
		...o,
	};
};
const latency = row({ key: 'core-api:latency', kind: 'latency', objective: 'requests answered in < 1 s at the edge', threshold: '< 1 s', failedLabel: 'slower than 1 s', target: 0.94 });
const nats = row({
	key: 'core-nats:availability', sliId: 'core-nats', sliKind: 'nats', objective: 'NATS handler runs that did not fail', failedLabel: 'NATS handler runs',
	range: { ratio: 0.9892, failed: 54941, total: 5100000 }, budgetLeft: -1, burn: { h1: null, h6: 2.1, h24: 2.2 },
	status: evaluateStatus({ hasTarget: true, budget: -1, burn1h: null, burn6h: 2.1, burn24h: 2.2 }),
});
const bare = row({ key: 'x:availability', sliId: 'x', target: null, budgetLeft: null, burn: { h1: null, h6: null, h24: null }, status: evaluateStatus({ hasTarget: false, budget: null, burn1h: null, burn6h: null, burn24h: null }) });

describe('SloTable', () => {
	it('renders a row per objective with the range label in the header', () => {
		render(SloTable, { rows: [row(), latency, nats, bare] as never, range: '7d' });
		expect(screen.getAllByTestId(/^slo-row-/)).toHaveLength(4);
		expect(screen.getByTestId('slo-range-header').textContent).toContain('Last 7 days');
		expect(screen.getByTestId('slo-budget-header').textContent).toBe('28-day budget left');
	});

	it('follows the selected range in the header and names each failure source', () => {
		render(SloTable, { rows: [row(), latency] as never, range: '24h' });
		expect(screen.getByTestId('slo-range-header').textContent).toContain('Last 24 hours');
		expect(screen.getByTestId('slo-range-core-api:availability').textContent).toContain('99.78%');
		expect(screen.getByTestId('slo-range-core-api:availability').textContent).toContain('101 failed (5xx at edge) / 457,463 total');
		expect(screen.getByTestId('slo-range-core-api:latency').textContent).toContain('(slower than 1 s)');
	});

	it('shows target, budget, burns as 0.2×, and a status WORD in a pill whose title says why', () => {
		render(SloTable, { rows: [row(), nats] as never, range: '7d' });
		const r = screen.getByTestId('slo-row-core-api:availability');
		expect(r.textContent).toContain('99.5%');
		expect(r.textContent).toContain('56%');
		expect(r.textContent).toContain('0.2×\u00a0·\u00a00.4×\u00a0·\u00a00.5×');
		const pill = screen.getByTestId('slo-status-core-nats:availability');
		expect(pill.textContent).toBe('Breached');
		expect(pill.getAttribute('title')).toContain('Budget left -100% ≤ 0%');
		expect(screen.getByTestId('slo-status-core-api:availability').textContent).toBe('OK');
		const dash = within(screen.getByTestId('slo-row-core-nats:availability')).getAllByTitle('low traffic');
		expect(dash).toHaveLength(1);
		expect(dash[0].textContent).toBe('—');
	});

	it('puts the hour of the last-hour cell in its title', () => {
		render(SloTable, { rows: [row()] as never, range: '7d' });
		const cell = screen.getByText('99.90%');
		expect(cell.getAttribute('title')).toBe('2026-10-20 11:00 UTC');
	});

	it('an untargeted objective reads Untracked with dashes', () => {
		render(SloTable, { rows: [bare] as never, range: '7d' });
		const r = screen.getByTestId('slo-row-x:availability');
		expect(screen.getByTestId('slo-status-x:availability').textContent).toBe('Untracked');
		expect(r.textContent).not.toContain('NaN');
		expect(screen.getByTestId('slo-budget-x:availability').textContent?.trim()).toBe('—');
	});

	it('carries the SSE caveat on latency rows only (D10)', () => {
		render(SloTable, { rows: [row(), latency] as never, range: '7d' });
		expect(screen.getByTestId('slo-sse-core-api:latency').getAttribute('title')).toBe(SSE_CAVEAT);
		expect(screen.queryByTestId('slo-sse-core-api:availability')).toBeNull();
	});

	it('lists the top NATS contributors under the NATS row; unavailable says so for the list only (D8)', async () => {
		const ok = { status: 'ok' as const, rows: [{ stream: 'whatsappv4', consumer: 'highThroughputConsumer', errors: 28160, runs: 723665, rate: 28160 / 723665 }, { stream: 'whatsmeowed', consumer: 'whatsappConsumer', errors: 25105, runs: 30539, rate: 25105 / 30539 }] };
		const a = render(SloTable, { rows: [row(), nats] as never, range: '7d', natsBreakdown: Promise.resolve(ok) });
		const items = await screen.findAllByTestId('nats-contributor');
		expect(items).toHaveLength(2);
		expect(items[0].textContent).toContain('highThroughputConsumer');
		expect(items[0].textContent).toContain('28,160 failed / 723,665 runs · 3.9%');
		expect(items[1].textContent).toContain('82.2%');
		// directly under the NATS row, not the API row
		expect(screen.getByTestId('slo-row-core-nats:availability').nextElementSibling).toBe(screen.getByTestId('nats-breakdown'));
		a.unmount();
		render(SloTable, { rows: [row(), nats] as never, range: '7d', natsBreakdown: Promise.resolve({ status: 'unavailable' as const }) });
		expect((await screen.findByTestId('nats-breakdown-unavailable')).textContent).toBe('breakdown unavailable');
		expect(screen.getAllByTestId(/^slo-row-/)).toHaveLength(2); // the table is intact
	});

	it('renders the table while the breakdown is still pending (D8)', async () => {
		let resolve!: (v: null) => void;
		render(SloTable, { rows: [row(), nats] as never, range: '7d', natsBreakdown: new Promise<null>((r) => (resolve = r)) });
		expect(screen.getAllByTestId(/^slo-row-/)).toHaveLength(2);
		expect(screen.getByTestId('nats-breakdown-loading')).toBeTruthy();
		resolve(null);
		await vi.waitFor(() => expect(screen.queryByTestId('nats-breakdown-loading')).toBeNull());
	});

	it('titles the budget column with the one shared window, e.g. 14-day', () => {
		render(SloTable, { rows: [row({ windowDays: 14 }), { ...nats, windowDays: 14 }] as never, range: '7d' });
		expect(screen.getByTestId('slo-budget-header').textContent).toBe('14-day budget left');
		expect(screen.queryByTestId('slo-budget-window-core-nats:availability')).toBeNull();
	});

	it('when windows differ the header is generic and each budget cell names its own window', () => {
		render(SloTable, { rows: [row(), { ...nats, windowDays: 14 }] as never, range: '7d' });
		expect(screen.getByTestId('slo-budget-header').textContent).toBe('Budget left');
		expect(screen.getByTestId('slo-budget-window-core-api:availability').textContent).toBe('28-day window');
		expect(screen.getByTestId('slo-budget-window-core-nats:availability').textContent).toBe('14-day window');
	});
});
