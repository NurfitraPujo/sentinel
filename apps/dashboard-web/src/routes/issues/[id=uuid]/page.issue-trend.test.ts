import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import Page from './+page.svelte';
import type { PageData } from './$types';

// sentinel-issue-trend fe.md task 8, contract §11's `@testing-library/svelte` row — flows `D1`–`D6`:
// the `<section class="trend">` between `.issue-info` and the Occurrence History, its two charts'
// heading literals (`Last 24 hours: {sum}`, `Last 30 days (UTC): {sum}`, the decision-16 suffix
// after the sum), every bar a `<rect>` with a `<title>` CHILD (the last hourly one ending
// ` (current hour)`, the daily ones carrying the UTC calendar date — never `formatDate`'s local
// date-time), a visually-hidden table per chart with one row per point and an `aria-hidden`
// drawing, the all-zero shape, and the Occurrence History untouched.
//
// Page data is a literal built from contract §4.2's table: `trend.hourly` is 24 `TrendPoint`s,
// `trend.daily` is 30, both dense, oldest first, `[last]` the current partial slot. No DB, no
// loader. Height, label cadence, inline extremes and layout are guidance and are NOT asserted.

const HOUR_MS = 60 * 60 * 1000;
const DAY_MS = 24 * HOUR_MS;

// `trunc(now)` for the literal: 2026-09-13T14:00:00Z, so the hourly window starts the day
// before at 15:00Z and the daily window starts 2026-08-15T00:00:00Z — the contract's own excerpt.
const WINDOW_END = new Date('2026-09-13T14:00:00Z').getTime();
const DAILY_START = new Date('2026-08-15T00:00:00Z').getTime();

function iso(ms: number): string {
	return new Date(ms).toISOString().slice(0, 19) + 'Z';
}

function hourly(counts: number[]) {
	expect(counts.length).toBe(24);
	return counts.map((count, i) => ({ start: iso(WINDOW_END - (23 - i) * HOUR_MS), count }));
}

function daily(counts: number[]) {
	expect(counts.length).toBe(30);
	return counts.map((count, i) => ({ start: iso(DAILY_START + i * DAY_MS), count }));
}

// Hourly: 1,234 in total, 17 in the current hour. Daily: 2,345 in total, 212 today.
const HOURLY_COUNTS = [
	0, 0, 0, 0, 0, 0, 1, 2, 3, 4, 5, 6, 10, 20, 30, 40, 50, 60, 100, 200, 300, 200, 186, 17,
];
const DAILY_COUNTS = [
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 3, 9, 27, 81, 243, 729, 1040,
	212,
];

function pageData(overrides: Record<string, unknown> = {}) {
	return {
		issue: {
			id: 'issue-1',
			projectId: 'proj-1',
			errorClass: 'TimeoutError',
			message: 'upstream timed out',
			status: 'open',
			environment: 'production',
			count: 9876,
			firstSeen: '2026-09-01T10:00:00.000Z',
			lastSeen: '2026-09-12T10:00:00.000Z',
			affectedUserCount: 1234,
			affectedTenantCount: 3,
		},
		project: { id: 'proj-1', name: 'Checkout API' },
		environmentDisplayName: 'Production',
		occurrences: [],
		occurrenceTotal: 9876,
		relations: [],
		retentionDays: 30,
		trend: { hourly: hourly(HOURLY_COUNTS), daily: daily(DAILY_COUNTS) },
		...overrides,
	};
}

function text(el: Element | null | undefined): string {
	return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

// The page's own `formatDate` (issues/[id]/+page.svelte), reproduced so the hourly titles are
// asserted in the viewer's local time whatever TZ the test runs under.
function formatDateLikeThePage(start: string): string {
	return new Date(start).toLocaleString('en-US', {
		year: 'numeric',
		month: 'short',
		day: 'numeric',
		hour: '2-digit',
		minute: '2-digit',
		second: '2-digit',
	});
}

function rects(testId: string): SVGRectElement[] {
	return Array.from(screen.getByTestId(testId).querySelectorAll('svg rect'));
}

function titleOf(rect: Element): string {
	// The `<title>` must be a CHILD ELEMENT, not an attribute (flow D4).
	expect(rect.hasAttribute('title')).toBe(false);
	const title = rect.querySelector(':scope > title');
	expect(title).not.toBeNull();
	return title?.textContent ?? '';
}

describe('/issues/{id} trend section placement (contract §4.2, flows D1, D6)', () => {
	it('renders section.trend between .issue-info and section.occurrences, leaving the history untouched', () => {
		render(Page, { data: pageData() as unknown as PageData });

		const detail = document.querySelector('.issue-detail');
		const children = Array.from(detail?.children ?? []);
		const info = children.findIndex((c) => c.classList.contains('issue-info'));
		const trend = children.findIndex(
			(c) => c.tagName.toLowerCase() === 'section' && c.classList.contains('trend')
		);
		const history = children.findIndex(
			(c) => c.tagName.toLowerCase() === 'section' && c.classList.contains('occurrences')
		);
		expect(info).toBeGreaterThanOrEqual(0);
		expect(trend).toBeGreaterThan(info);
		expect(history).toBeGreaterThan(trend);

		// D6 — the lifetime count and the Occurrence History heading are unchanged.
		const items = Array.from(document.querySelectorAll('.meta-info .meta-item')).map(text);
		expect(items).toContain('Occurrences: 9,876');
		expect(text(document.querySelector('section.occurrences .section-title'))).toBe(
			'Occurrence History (9,876)'
		);
		// Two charts inside the section.
		expect(screen.getByTestId('trend-hourly')).toBeTruthy();
		expect(screen.getByTestId('trend-daily')).toBeTruthy();
		expect(screen.getByTestId('issue-trend').querySelectorAll('figure').length).toBe(2);
	});
});

describe('/issues/{id} chart headings (contract §4.2, §3, flows D1, D2, decision 16)', () => {
	it('reads "Last 24 hours: {sum}" and "Last 30 days (UTC): {sum}" through toLocaleString', () => {
		render(Page, { data: pageData() as unknown as PageData });
		expect(text(screen.getByTestId('trend-hourly-heading'))).toBe('Last 24 hours: 1,234');
		expect(text(screen.getByTestId('trend-daily-heading'))).toBe('Last 30 days (UTC): 2,345');
	});

	it('appends " (data kept for {retentionDays} days)" after the daily sum when retentionDays < 30 — D2', () => {
		render(Page, { data: pageData({ retentionDays: 7 }) as unknown as PageData });
		expect(text(screen.getByTestId('trend-daily-heading'))).toBe(
			'Last 30 days (UTC): 2,345 (data kept for 7 days)'
		);
		// The hourly heading is untouched by retention.
		expect(text(screen.getByTestId('trend-hourly-heading'))).toBe('Last 24 hours: 1,234');
		// Still 30 bars: always 30 days (decision 16), the leading ones zero by construction.
		expect(rects('trend-daily').length).toBe(30);
	});

	it('labels the 30-bar axis "Last 30 days" and names a 14-day data window only in the suffix', () => {
		render(Page, { data: pageData({ retentionDays: 14 }) as unknown as PageData });
		expect(text(screen.getByTestId('trend-daily-heading'))).toBe(
			'Last 30 days (UTC): 2,345 (data kept for 14 days)'
		);
	});

	it('carries no suffix at exactly 30 days', () => {
		render(Page, { data: pageData({ retentionDays: 30 }) as unknown as PageData });
		expect(text(screen.getByTestId('trend-daily-heading'))).toBe('Last 30 days (UTC): 2,345');
	});
});

describe('/issues/{id} bars and their <title> children (contract §4.2, §3 chart clocks, flow D4)', () => {
	it('renders 24 hourly <rect>s, each titled "{count} occurrences · {start, local}", the last ending " (current hour)"', () => {
		render(Page, { data: pageData() as unknown as PageData });

		const bars = rects('trend-hourly');
		expect(bars.length).toBe(24);
		const titles = bars.map(titleOf);
		const points = hourly(HOURLY_COUNTS);
		for (let i = 0; i < 23; i++) {
			expect(titles[i]).toBe(
				`${Number(points[i].count).toLocaleString()} occurrences · ${formatDateLikeThePage(points[i].start)}`
			);
			expect(titles[i].endsWith(' (current hour)')).toBe(false);
		}
		// One format with a suffix on the last bar, not a second format for one bar.
		expect(titles[23]).toBe(
			`17 occurrences · ${formatDateLikeThePage(points[23].start)} (current hour)`
		);
		// The noun is the plural on every bar, including `1 occurrences` (§3 — intended, no singular).
		expect(titles[6].startsWith('1 occurrences · ')).toBe(true);
		// No "UTC" word on the hourly chart.
		expect(text(screen.getByTestId('trend-hourly'))).not.toContain('UTC');
	});

	it('renders 30 daily <rect>s titled "{count} occurrences · {UTC date}" — the UTC calendar date, never formatDate', () => {
		render(Page, { data: pageData() as unknown as PageData });

		const bars = rects('trend-daily');
		expect(bars.length).toBe(30);
		const titles = bars.map(titleOf);
		expect(titles[0]).toBe('0 occurrences · Aug 15, 2026');
		expect(titles[28]).toBe('1,040 occurrences · Sep 12, 2026');
		expect(titles[29]).toBe('212 occurrences · Sep 13, 2026');
		for (const t of titles) {
			// A UTC midnight rendered through `formatDate` would carry a time of day and, west of
			// UTC, the previous date. Neither may appear.
			expect(t).not.toMatch(/\d{2}:\d{2}:\d{2}/);
			expect(t).not.toMatch(/(AM|PM)/);
			expect(t).not.toContain('(current hour)');
		}
		// Every bar of a series carries a distinct date: 30 bars, 30 dates.
		expect(new Set(titles.map((t) => t.split(' · ')[1])).size).toBe(30);
	});

	it('renders the daily date as the UTC date even for a viewer west of UTC (the formatDate trap)', () => {
		// The trap `formatDate` would fall into: local rendering of a UTC midnight. The chart's
		// titles must be the same string regardless — this asserts the literal, which is
		// timezone-independent by construction.
		render(Page, {
			data: pageData({
				trend: {
					hourly: hourly(new Array(24).fill(0)),
					daily: daily([...new Array(29).fill(0), 5]),
				},
			}) as unknown as PageData,
		});
		expect(titleOf(rects('trend-daily')[29])).toBe('5 occurrences · Sep 13, 2026');
		expect(titleOf(rects('trend-daily')[0])).toBe('0 occurrences · Aug 15, 2026');
	});
});

describe('/issues/{id} hidden tables and aria-hidden drawings (contract §4.2, flow D5)', () => {
	it('carries a visually-hidden table per chart with one row per point, and an aria-hidden SVG', () => {
		render(Page, { data: pageData() as unknown as PageData });

		for (const [id, length] of [
			['trend-hourly', 24],
			['trend-daily', 30],
		] as const) {
			const figure = screen.getByTestId(id);
			const svg = figure.querySelector('svg');
			expect(svg?.getAttribute('aria-hidden')).toBe('true');
			const table = screen.getByTestId(`${id}-table`);
			expect(table.tagName.toLowerCase()).toBe('table');
			expect(table.classList.contains('visually-hidden')).toBe(true);
			expect(table.querySelectorAll('tbody tr').length).toBe(length);
		}

		// The rows carry the start and the count, in the chart's clock.
		const dailyRows = Array.from(
			screen.getByTestId('trend-daily-table').querySelectorAll('tbody tr')
		).map((tr) => Array.from(tr.querySelectorAll('td')).map(text));
		expect(dailyRows[0]).toEqual(['Aug 15, 2026', '0']);
		expect(dailyRows[28]).toEqual(['Sep 12, 2026', '1,040']);
		expect(dailyRows[29]).toEqual(['Sep 13, 2026', '212']);

		const hourlyRows = Array.from(
			screen.getByTestId('trend-hourly-table').querySelectorAll('tbody tr')
		).map((tr) => Array.from(tr.querySelectorAll('td')).map(text));
		const points = hourly(HOURLY_COUNTS);
		expect(hourlyRows[0]).toEqual([formatDateLikeThePage(points[0].start), '0']);
		expect(hourlyRows[23]).toEqual([
			`${formatDateLikeThePage(points[23].start)} (current hour)`,
			'17',
		]);
	});
});

describe('/issues/{id} all zeros (contract §4.2, flow D3)', () => {
	it('renders both charts with ": 0" headings, every bar present at zero height, and the section still there', () => {
		render(Page, {
			data: pageData({
				trend: { hourly: hourly(new Array(24).fill(0)), daily: daily(new Array(30).fill(0)) },
			}) as unknown as PageData,
		});

		expect(screen.getByTestId('issue-trend')).toBeTruthy();
		expect(text(screen.getByTestId('trend-hourly-heading'))).toBe('Last 24 hours: 0');
		expect(text(screen.getByTestId('trend-daily-heading'))).toBe('Last 30 days (UTC): 0');
		expect(rects('trend-hourly').length).toBe(24);
		expect(rects('trend-daily').length).toBe(30);
		for (const r of [...rects('trend-hourly'), ...rects('trend-daily')]) {
			expect(Number(r.getAttribute('height'))).toBe(0);
		}
		// The flat baseline remains in each drawing.
		expect(screen.getByTestId('trend-hourly').querySelector('svg line')).not.toBeNull();
		expect(screen.getByTestId('trend-daily').querySelector('svg line')).not.toBeNull();
		// Every title still reads `0 occurrences · …`.
		for (const r of rects('trend-daily')) expect(titleOf(r).startsWith('0 occurrences · ')).toBe(true);
	});
});
