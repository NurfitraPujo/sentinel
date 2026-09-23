import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import Page from './+page.svelte';
import type { PageData } from './$types';

// sentinel-issue-trend fe.md task 8, contract §11's `@testing-library/svelte` row — flows `L1`/`L10`
// (the sparkline and its summed label, the flat baseline), `L2`/`L3` (two more sort anchors),
// `L4`–`L7` (the `new` anchors, `href()` carrying `new` and dropping it on `all`), `L8` (the
// empty-state copy) and `L9` (the `NEW` and `REGRESSED` badges, decisions 12 and 22).
//
// Every case is a contract §4.1 claim about what the TEMPLATE does with page data, so it is
// asserted against a literal built from §4.1's page-data table: `trend24h` is 24 numbers,
// `filters.new` is the loader's COERCED value (the loader's coercion is BE's, pinned in
// `page.server.test.ts`), `regressionStatus` / `lastRegressedAt` are the two projection fields.
// No DB, no loader.

const HOUR = 60 * 60 * 1000;
const NOW = new Date('2026-09-13T12:00:00.000Z').getTime();

function ago(ms: number): string {
	return new Date(NOW - ms).toISOString();
}

// A 24-point series summing to 1,234 so the label's thousands separator is exercised.
const TREND_1234 = [
	0, 0, 0, 0, 0, 0, 1, 2, 3, 4, 5, 6, 10, 20, 30, 40, 50, 60, 100, 200, 300, 200, 150, 53,
];

function issue(overrides: Record<string, unknown> = {}) {
	return {
		id: 'issue-1',
		errorClass: 'TimeoutError',
		message: 'upstream timed out',
		count: 12,
		firstSeen: '2026-09-01T10:00:00.000Z',
		lastSeen: '2026-09-12T10:00:00.000Z',
		projectName: 'Checkout API',
		environment: 'production',
		status: 'open',
		affectedUserCount: 1234,
		affectedTenantCount: 3,
		regressionStatus: 'none',
		lastRegressedAt: null,
		trend24h: TREND_1234,
		...overrides,
	};
}

function pageData(overrides: Record<string, unknown> = {}) {
	return {
		orgSlug: 'acme',
		issues: [issue()],
		projects: [
			{ id: 'proj-1', name: 'Checkout API' },
			{ id: 'proj-2', name: 'Billing' },
		],
		environments: [],
		filters: { status: 'all', project: 'proj-1', environment: 'all', sort: 'last_seen', new: 'all' },
		retentionDays: 30,
		pagination: { page: 1, limit: 20, total: 1, totalPages: 1 },
		...overrides,
	};
}

function filters(overrides: Record<string, unknown> = {}) {
	return { status: 'all', project: 'proj-1', environment: 'all', sort: 'last_seen', new: 'all', ...overrides };
}

function text(el: Element | null | undefined): string {
	return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

function hrefs(): string[] {
	return Array.from(document.querySelectorAll('a[href^="?"]')).map(
		(a) => a.getAttribute('href') ?? ''
	);
}

afterEach(() => {
	vi.useRealTimers();
});

describe('/ sparkline on every card (contract §4.1, flows L1, L10, decisions 7, 10, 13)', () => {
	it('renders 24 marks with role=img and the summed aria-label through toLocaleString', () => {
		render(Page, { data: pageData() as unknown as PageData });

		const spark = screen.getByRole('img', { name: '1,234 occurrences in the last 24 hours' });
		expect(spark.tagName.toLowerCase()).toBe('svg');
		expect(spark.getAttribute('aria-label')).toBe('1,234 occurrences in the last 24 hours');
		expect(spark.querySelectorAll('rect').length).toBe(24);
		// Inline SVG in the card, between the message and the footer (decision 13; L1's placement).
		const card = document.querySelector('.issue-card');
		const children = Array.from(card?.children ?? []);
		const message = children.findIndex((c) => c.classList.contains('message'));
		const footer = children.findIndex((c) => c.classList.contains('card-footer'));
		const svg = children.findIndex((c) => c.tagName.toLowerCase() === 'svg');
		expect(message).toBeGreaterThanOrEqual(0);
		expect(svg).toBe(message + 1);
		expect(footer).toBe(svg + 1);
		// No numbers on the shape: the SVG carries no text.
		expect(spark.querySelectorAll('text').length).toBe(0);
	});

	it('scales each card to its OWN maximum: the tallest bar is full height, the rest proportional', () => {
		const values = new Array(24).fill(0);
		values[3] = 5;
		values[10] = 1;
		values[23] = 10;
		render(Page, {
			data: pageData({ issues: [issue({ trend24h: values })] }) as unknown as PageData,
		});

		const spark = screen.getByRole('img', { name: '16 occurrences in the last 24 hours' });
		const rects = Array.from(spark.querySelectorAll('rect'));
		const viewBoxHeight = Number(spark.getAttribute('viewBox')?.split(' ')[3]);
		const height = (i: number) => Number(rects[i].getAttribute('height'));
		expect(height(23)).toBe(viewBoxHeight);
		expect(height(3)).toBeCloseTo(viewBoxHeight / 2, 6);
		expect(height(10)).toBeCloseTo(viewBoxHeight / 10, 6);
		expect(height(0)).toBe(0);
	});

	it('scales a second card independently of the first — shape per card, not magnitude across cards', () => {
		const small = new Array(24).fill(0);
		small[23] = 2;
		const large = new Array(24).fill(0);
		large[23] = 2000;
		render(Page, {
			data: pageData({
				issues: [issue({ id: 'a', trend24h: small }), issue({ id: 'b', trend24h: large })],
			}) as unknown as PageData,
		});

		const sparks = screen.getAllByRole('img');
		expect(sparks.length).toBe(2);
		const last = (svg: Element) => {
			const rects = svg.querySelectorAll('rect');
			return Number(rects[rects.length - 1].getAttribute('height'));
		};
		// Both cards' last bar is full height: 2 is that card's maximum, 2000 is the other's.
		expect(last(sparks[0])).toBe(last(sparks[1]));
		expect(sparks[0].getAttribute('aria-label')).toBe('2 occurrences in the last 24 hours');
		expect(sparks[1].getAttribute('aria-label')).toBe('2,000 occurrences in the last 24 hours');
	});

	it('still renders for an all-zero series: 24 zero-height marks over a flat baseline, label 0 — L10', () => {
		render(Page, {
			data: pageData({ issues: [issue({ trend24h: new Array(24).fill(0) })] }) as unknown as PageData,
		});

		const spark = screen.getByRole('img', { name: '0 occurrences in the last 24 hours' });
		const rects = Array.from(spark.querySelectorAll('rect'));
		expect(rects.length).toBe(24);
		for (const r of rects) expect(Number(r.getAttribute('height'))).toBe(0);
		// The flat baseline is what remains: a line across the full width at the bottom.
		const baseline = spark.querySelector('line');
		expect(baseline).not.toBeNull();
		expect(baseline?.getAttribute('y1')).toBe(baseline?.getAttribute('y2'));
		expect(baseline?.getAttribute('x1')).toBe('0');
		expect(baseline?.getAttribute('x2')).toBe('24');
	});
});

describe('/ sort group gains Occurrences and First seen (contract §4.1, flows L2, L3, decision 5)', () => {
	it('renders four anchors, the two new ones after Affected users, with data-testids sort-count and sort-first_seen', () => {
		render(Page, { data: pageData() as unknown as PageData });

		const anchors = Array.from(screen.getByTestId('sort-control').querySelectorAll('a'));
		expect(anchors.map((a) => a.getAttribute('data-testid'))).toEqual([
			'sort-last_seen',
			'sort-affected_users',
			'sort-count',
			'sort-first_seen',
		]);
		expect(anchors.map(text)).toEqual(['Last seen', 'Affected users', 'Occurrences', 'First seen']);
		expect(screen.getByTestId('sort-count').getAttribute('href')).toBe(
			'?status=all&project=proj-1&sort=count'
		);
		expect(screen.getByTestId('sort-first_seen').getAttribute('href')).toBe(
			'?status=all&project=proj-1&sort=first_seen'
		);
		expect(screen.getByTestId('sort-last_seen').classList.contains('active')).toBe(true);
		expect(screen.getByTestId('sort-count').classList.contains('active')).toBe(false);
	});

	it('marks the new anchors active from filters.sort', () => {
		const count = render(Page, {
			data: pageData({ filters: filters({ sort: 'count' }) }) as unknown as PageData,
		});
		expect(screen.getByTestId('sort-count').classList.contains('active')).toBe(true);
		expect(screen.getByTestId('sort-last_seen').classList.contains('active')).toBe(false);
		count.unmount();

		render(Page, {
			data: pageData({ filters: filters({ sort: 'first_seen' }) }) as unknown as PageData,
		});
		expect(screen.getByTestId('sort-first_seen').classList.contains('active')).toBe(true);
		expect(screen.getByTestId('sort-count').classList.contains('active')).toBe(false);
	});
});

describe('/ the new filter anchors and href() (contract §4.1, flows L4–L7, decision 6)', () => {
	it('renders new-all and new-24h, the active class from filters.new, 24h carried and all omitted', () => {
		const all = render(Page, { data: pageData() as unknown as PageData });
		expect(text(screen.getByTestId('new-all'))).toBe('All');
		expect(text(screen.getByTestId('new-24h'))).toBe('New in the last 24 hours');
		expect(screen.getByTestId('new-all').classList.contains('active')).toBe(true);
		expect(screen.getByTestId('new-24h').classList.contains('active')).toBe(false);
		expect(screen.getByTestId('new-24h').getAttribute('href')).toBe(
			'?status=all&project=proj-1&new=24h'
		);
		// The default is the absence: no `new=all` on the anchor that selects it.
		expect(screen.getByTestId('new-all').getAttribute('href')).toBe('?status=all&project=proj-1');
		all.unmount();

		render(Page, { data: pageData({ filters: filters({ new: '24h' }) }) as unknown as PageData });
		expect(screen.getByTestId('new-24h').classList.contains('active')).toBe(true);
		expect(screen.getByTestId('new-all').classList.contains('active')).toBe(false);
	});

	it('places the group after Status and before Sort in the aside', () => {
		render(Page, { data: pageData() as unknown as PageData });
		const headings = Array.from(document.querySelectorAll('aside.filters h2')).map(text);
		const status = headings.indexOf('Status');
		expect(status).toBeGreaterThanOrEqual(0);
		expect(headings[status + 1]).toBe('Age');
		expect(headings[status + 2]).toBe('Sort');
	});

	it('keeps new=24h across paging, status, sort AND project anchors — L6', () => {
		render(Page, {
			data: pageData({
				environments: [{ id: 'env-1', name: 'production', displayName: 'Production' }],
				filters: filters({ status: 'open', environment: 'production', sort: 'count', new: '24h' }),
				pagination: { page: 2, limit: 20, total: 60, totalPages: 3 },
			}) as unknown as PageData,
		});

		// Paging.
		expect(screen.getByRole('link', { name: 'Next' }).getAttribute('href')).toBe(
			'?status=open&project=proj-1&environment=production&sort=count&new=24h&page=3'
		);
		expect(screen.getByRole('link', { name: 'Previous' }).getAttribute('href')).toBe(
			'?status=open&project=proj-1&environment=production&sort=count&new=24h'
		);
		// Status.
		expect(screen.getByRole('link', { name: 'Resolved' }).getAttribute('href')).toBe(
			'?status=resolved&project=proj-1&environment=production&sort=count&new=24h'
		);
		// Sort.
		expect(screen.getByTestId('sort-first_seen').getAttribute('href')).toBe(
			'?status=open&project=proj-1&environment=production&sort=first_seen&new=24h'
		);
		// Environment.
		expect(screen.getByRole('link', { name: 'Production' }).getAttribute('href')).toBe(
			'?status=open&project=proj-1&environment=production&sort=count&new=24h'
		);
		// Project — `new` survives; only `environment` resets.
		expect(screen.getByRole('link', { name: 'Billing' }).getAttribute('href')).toBe(
			'?status=open&project=proj-2&sort=count&new=24h'
		);
		expect(screen.getByRole('link', { name: 'All Projects' }).getAttribute('href')).toBe(
			'?status=open&project=all&sort=count&new=24h'
		);
		// The `new` anchors themselves: the other value, and no `page` — a filter change starts at 1.
		expect(screen.getByTestId('new-all').getAttribute('href')).toBe(
			'?status=open&project=proj-1&environment=production&sort=count'
		);
		expect(screen.getByTestId('new-24h').getAttribute('href')).not.toContain('page=');
	});

	// Flow `L7`: `/?new=banana` — the loader echoes `all`, and the page builds every anchor from
	// that echo, so nothing on the page can carry the bad string, or `new=all`.
	it('carries no new at all when filters.new is the coerced all — L7', () => {
		render(Page, {
			data: pageData({
				environments: [{ id: 'env-1', name: 'production', displayName: 'Production' }],
				filters: filters({ new: 'all' }),
				pagination: { page: 1, limit: 20, total: 60, totalPages: 3 },
			}) as unknown as PageData,
		});

		expect(screen.getByTestId('new-all').classList.contains('active')).toBe(true);
		const all = hrefs();
		expect(all.length).toBeGreaterThan(5);
		for (const h of all) {
			expect(h).not.toContain('banana');
			expect(h).not.toContain('new=all');
		}
		// Only the one anchor that selects the filter carries `new`.
		expect(all.filter((h) => h.includes('new=')).length).toBe(1);
		expect(screen.getByRole('link', { name: 'Next' }).getAttribute('href')).toBe(
			'?status=all&project=proj-1&page=2'
		);
	});
});

describe('/ empty state with new=24h (contract §4.1, flow L8)', () => {
	it('reads "No new issues in the last 24 hours." with no environment active', () => {
		render(Page, {
			data: pageData({
				issues: [],
				filters: filters({ new: '24h' }),
				pagination: { page: 1, limit: 20, total: 0, totalPages: 0 },
			}) as unknown as PageData,
		});
		expect(text(document.querySelector('.empty'))).toBe('No new issues in the last 24 hours.');
	});

	it('composes the environment note: "No new issues in the last 24 hours in {environment}."', () => {
		render(Page, {
			data: pageData({
				issues: [],
				environments: [{ id: 'env-1', name: 'production', displayName: 'Production' }],
				filters: filters({ environment: 'production', new: '24h' }),
				pagination: { page: 1, limit: 20, total: 0, totalPages: 0 },
			}) as unknown as PageData,
		});
		expect(text(document.querySelector('.empty'))).toBe(
			'No new issues in the last 24 hours in Production.'
		);
	});

	it('keeps the existing copy when new is all', () => {
		render(Page, {
			data: pageData({
				issues: [],
				pagination: { page: 1, limit: 20, total: 0, totalPages: 0 },
			}) as unknown as PageData,
		});
		expect(text(document.querySelector('.empty'))).toBe('No issues found.');
	});
});

describe('/ NEW and REGRESSED badges (contract §4.1, flow L9, decisions 12 and 22)', () => {
	function renderAt(overrides: Record<string, unknown>) {
		vi.useFakeTimers();
		vi.setSystemTime(NOW);
		render(Page, { data: pageData({ issues: [issue(overrides)] }) as unknown as PageData });
	}

	it('renders NEW for a firstSeen within 24 hours', () => {
		renderAt({ firstSeen: ago(HOUR), lastSeen: ago(HOUR) });
		expect(text(screen.getByTestId('badge-new'))).toBe('NEW');
		expect(screen.queryByTestId('badge-regressed')).toBeNull();
	});

	it('does NOT render NEW for an old firstSeen with a fresh lastSeen — the badge follows first_seen, not last_seen', () => {
		renderAt({ firstSeen: ago(30 * 24 * HOUR), lastSeen: ago(HOUR) });
		expect(screen.queryByTestId('badge-new')).toBeNull();
		expect(screen.queryByTestId('badge-regressed')).toBeNull();
	});

	it('renders REGRESSED for regressionStatus regressed with lastRegressedAt within 24 hours', () => {
		renderAt({
			firstSeen: ago(30 * 24 * HOUR),
			lastSeen: ago(HOUR),
			regressionStatus: 'regressed',
			lastRegressedAt: ago(HOUR),
		});
		expect(text(screen.getByTestId('badge-regressed'))).toBe('REGRESSED');
		expect(screen.queryByTestId('badge-new')).toBeNull();
	});

	it('does NOT render REGRESSED for an older lastRegressedAt', () => {
		renderAt({
			firstSeen: ago(30 * 24 * HOUR),
			lastSeen: ago(HOUR),
			regressionStatus: 'regressed',
			lastRegressedAt: ago(25 * HOUR),
		});
		expect(screen.queryByTestId('badge-regressed')).toBeNull();
	});

	it('does NOT render REGRESSED for regressionStatus none, even with a recent lastRegressedAt', () => {
		renderAt({
			firstSeen: ago(30 * 24 * HOUR),
			regressionStatus: 'none',
			lastRegressedAt: ago(HOUR),
		});
		expect(screen.queryByTestId('badge-regressed')).toBeNull();
	});

	it('renders both, NEW first, when an issue first fired, was resolved and re-fired inside one day', () => {
		renderAt({
			firstSeen: ago(20 * HOUR),
			regressionStatus: 'regressed',
			lastRegressedAt: ago(HOUR),
		});
		const badges = Array.from(document.querySelectorAll('.card-header .badge')).map(text);
		expect(badges).toEqual(['NEW', 'REGRESSED']);
	});
});

describe('/ everything else on the card is unchanged (flow L11)', () => {
	it('keeps the lifetime count, the affected figures, Last seen, the environment chip and the status', () => {
		render(Page, { data: pageData() as unknown as PageData });
		expect(text(document.querySelector('.card-footer .count'))).toBe('12 occurrences');
		expect(text(screen.getByTestId('issue-affected'))).toBe(
			'1,234 users · 3 tenants in the last 30 days'
		);
		expect(text(document.querySelector('.last-seen'))).toMatch(/^Last seen: /);
		expect(text(screen.getByTestId('issue-environment'))).toBe('production');
		expect(text(document.querySelector('.meta .status'))).toBe('open');
	});
});

describe('/ true claimant detection and dynamic claimers', () => {
	it('renders Unclaimed when assignedTo is null', () => {
		render(Page, {
			data: pageData({
				issues: [issue({ assignedTo: null, assigneeType: null })],
			}) as unknown as PageData,
		});
		expect(text(document.querySelector('.claim-badge'))).toBe('Unclaimed');
	});

	it('renders Claimed by Me when assignedTo matches currentUser email or id', () => {
		render(Page, {
			data: {
				...pageData({
					issues: [issue({ assignedTo: 'fitrapujo@gmail.com', assigneeType: 'user' })],
				}),
				currentUser: { id: 'usr-1', email: 'fitrapujo@gmail.com' },
			} as unknown as PageData,
		});
		expect(text(document.querySelector('.claim-badge'))).toBe('Claimed by Me');
	});

	it('renders Claimed: <assignee> when assigned to another user', () => {
		render(Page, {
			data: {
				...pageData({
					issues: [issue({ assignedTo: 'alice@example.com', assigneeType: 'user' })],
				}),
				currentUser: { id: 'usr-1', email: 'fitrapujo@gmail.com' },
			} as unknown as PageData,
		});
		expect(text(document.querySelector('.claim-badge'))).toBe('Claimed: alice@example.com');
	});

	it('renders 🤖 <agent> when assigneeType is agent', () => {
		render(Page, {
			data: pageData({
				issues: [issue({ assignedTo: 'sentinel-bot', assigneeType: 'agent' })],
			}) as unknown as PageData,
		});
		expect(text(document.querySelector('.claim-badge'))).toBe('🤖 sentinel-bot');
	});
});

