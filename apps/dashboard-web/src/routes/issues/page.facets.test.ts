import { describe, it, expect, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/svelte';
import { render, EMPTY_FACETS } from './page-fixtures';
import Page from './+page.svelte';
import type { PageData } from './$types';

// fix-sentinel-issues-dashboard §4.1, §4.3, D9 — what the page does with `data.facets` and the new
// server filters. Fixture-driven like its siblings: the loader and the queries have their own tests.

const gotoMock = vi.hoisted(() => vi.fn());
vi.mock('$app/navigation', () => ({ goto: gotoMock }));

function text(el: Element | null | undefined) {
	return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

function pageData(overrides: Record<string, unknown> = {}) {
	return {
		orgSlug: 'acme',
		issues: [],
		projects: [
			{ id: 'proj-1', name: 'Checkout' },
			{ id: 'proj-2', name: 'Billing' },
		],
		environments: [
			{ id: 'e1', name: 'production', displayName: 'Production' },
			{ id: 'e2', name: 'qa', displayName: 'QA' },
		],
		filters: { project: 'proj-1', environment: 'all', status: 'all' },
		retentionDays: 30,
		pagination: { page: 1, limit: 20, total: 1234, totalPages: 62 },
		currentUser: { id: 'u1', email: 'u@x' },
		...overrides,
	};
}

const facets = {
	...EMPTY_FACETS,
	status: { all: 1234, unresolved: 1000, resolved: 200, ignored: 34 },
	age: { all: 1234, new24h: 56 },
	environment: { all: 1234, byEnvironment: { production: 900, qa: 7 } },
	project: { all: 1237, byProject: { 'proj-1': 1234, 'proj-2': 3 } },
	claim: { ...EMPTY_FACETS.claim, total: 1234, unclaimed: 1000, claimedMe: 5, claimedAgent: 2 },
};

describe('/ sidebar counts come from data.facets, never the page (row 1)', () => {
	it('renders the database counts on a one-row page under a 1,234-issue total', () => {
		render(Page, { data: pageData({ issues: [], facets }) as unknown as PageData });
		const link = (name: string) => text(screen.getByRole('link', { name }));
		// The Status and Age groups both start with an `All` anchor; the Status one is first.
		expect(text(screen.getAllByRole('link', { name: 'All' })[0])).toBe('All 1234');
		expect(link('Unresolved')).toBe('Unresolved 1000');
		expect(link('Resolved')).toBe('Resolved 200');
		expect(link('Ignored')).toBe('Ignored 34');
		expect(link('All Projects')).toBe('All Projects 1237');
		expect(link('Billing')).toBe('Billing 3');
		expect(text(screen.getByTestId('new-24h'))).toBe('New in the last 24 hours 56');
		// Claim group's All agrees with the status group's All.
		expect(text(document.querySelector('.facet-list a.facet-btn'))).toContain('1234');
	});

	it('shows the Age 24h badge INSIDE its anchor, and a count for EVERY listed environment (D9)', () => {
		render(Page, { data: pageData({ facets }) as unknown as PageData });
		expect(screen.getByTestId('new-24h').querySelector('.count-badge')).not.toBeNull();
		expect(text(screen.getByRole('link', { name: 'Production' }))).toBe('Production 900');
		expect(text(screen.getByRole('link', { name: 'QA' }))).toBe('QA 7');
		expect(text(screen.getByRole('link', { name: 'All Environments' }))).toBe('All Environments 1234');
	});

	it('has no Open status, no Platform group and no heuristic quick chips', () => {
		render(Page, { data: pageData({ facets }) as unknown as PageData });
		expect(screen.queryByRole('link', { name: 'Open' })).toBeNull();
		const headings = Array.from(document.querySelectorAll('.filters h2')).map((h) => text(h));
		expect(headings).not.toContain('Platform');
		const body = document.body.textContent ?? '';
		for (const gone of ['Unhandled Only', 'Has Trace ID', '> 100 Events', 'Node.js / TypeScript']) {
			expect(body).not.toContain(gone);
		}
		expect(text(screen.getByTestId('volume-high'))).toBe('100+ events');
	});

	it('Showing N reads pagination.total, not the page', () => {
		render(Page, { data: pageData({ facets }) as unknown as PageData });
		expect(text(document.querySelector('.filter-count-readout'))).toBe('Showing 1,234 matching issues');
	});

	it('claimant rows: a user counts by id only; a legacy email row is its own row (D7)', () => {
		render(Page, {
			data: pageData({
				facets: {
					...facets,
					claim: {
						...facets.claim,
						byClaimant: { u2: 3, 'bob@x.test': 2 },
						claimantTypes: { u2: 'user', 'bob@x.test': 'user' },
					},
				},
				orgUsers: [{ id: 'u2', name: 'Bob', email: 'bob@x.test' }],
				usersById: {
					u2: { id: 'u2', name: 'Bob', email: 'bob@x.test' },
					'bob@x.test': { id: 'u2', name: 'Bob', email: 'bob@x.test' },
				},
			}) as unknown as PageData,
		});
		const rows = Array.from(document.querySelectorAll('.facet-list a.facet-btn')).map((a) => ({
			text: text(a),
			href: a.getAttribute('href'),
		}));
		const bob = rows.filter((r) => r.text.startsWith('Bob'));
		expect(bob.map((r) => r.text).sort()).toEqual(['Bob 2', 'Bob 3']);
		expect(bob.find((r) => r.text === 'Bob 3')!.href).toContain('claim=u2');
		expect(bob.find((r) => r.text === 'Bob 2')!.href).toContain('claim=bob%40x.test');
	});
});

describe('/ server filters ride the URL (rows 4, D3)', () => {
	it('carries volume, seen and a custom range on every anchor, and resets paging', () => {
		render(Page, {
			data: pageData({
				facets,
				filters: { project: 'proj-1', environment: 'all', status: 'all', volume: 'high', seen: '7d' },
			}) as unknown as PageData,
		});
		expect(screen.getByRole('link', { name: 'Resolved' }).getAttribute('href')).toBe(
			'?status=resolved&project=proj-1&volume=high&seen=7d'
		);
		expect(screen.getByTestId('volume-high').classList.contains('active')).toBe(true);
		// The date trigger is labelled from the echo, not component state.
		expect(text(document.querySelector('.date-text'))).toBe('Last 7 Days');
	});

	it('emits seen_from/seen_to only beside seen=all, and labels the trigger from them', () => {
		render(Page, {
			data: pageData({
				facets,
				filters: {
					project: 'proj-1',
					environment: 'all',
					status: 'all',
					seen_from: '2026-09-01T00:00:00.000Z',
					seen_to: '2026-09-02T00:00:00.000Z',
				},
			}) as unknown as PageData,
		});
		const href = screen.getByRole('link', { name: 'Resolved' }).getAttribute('href')!;
		expect(href).toContain('seen_from=2026-09-01T00%3A00%3A00.000Z');
		expect(href).toContain('seen_to=2026-09-02T00%3A00%3A00.000Z');
		expect(text(document.querySelector('.date-text'))).toContain('→');
	});

	it('Reset all filters keeps project, sort and limit and drops every other filter, incl. claim', () => {
		render(Page, {
			data: pageData({
				facets,
				filters: {
					project: 'proj-1',
					environment: 'production',
					status: 'resolved',
					sort: 'count',
					new: '24h',
					claim: 'unclaimed',
					q: 'boom',
					volume: 'high',
					seen: '24h',
				},
				pagination: { page: 3, limit: 50, total: 1234, totalPages: 25 },
			}) as unknown as PageData,
		});
		expect(screen.getByTestId('reset-filters').getAttribute('href')).toBe(
			'?status=all&project=proj-1&sort=count&limit=50'
		);
	});

	it('shows Reset when only the claim filter is active (it used to miss that), and not when nothing is', () => {
		const { unmount } = render(Page, {
			data: pageData({
				facets,
				filters: { project: 'proj-1', environment: 'all', status: 'all', claim: 'claimed-me' },
			}) as unknown as PageData,
		});
		expect(screen.queryByTestId('reset-filters')).not.toBeNull();
		unmount();
		render(Page, { data: pageData({ facets }) as unknown as PageData });
		expect(screen.queryByTestId('reset-filters')).toBeNull();
	});

	it('Cancel closes the date popover without navigating; a preset navigates', async () => {
		gotoMock.mockClear();
		render(Page, { data: pageData({ facets }) as unknown as PageData });
		screen.getByRole('button', { name: /Date Range/ }).click();
		await screen.findByRole('dialog');
		screen.getByRole('button', { name: '7 Days' }).click();
		expect(gotoMock).toHaveBeenCalledWith('?status=all&project=proj-1&seen=7d');

		gotoMock.mockClear();
		screen.getByRole('button', { name: /Date Range/ }).click();
		await screen.findByRole('dialog');
		screen.getByRole('button', { name: 'Cancel' }).click();
		expect(gotoMock).not.toHaveBeenCalled();
		await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
	});
});

describe('/ cards show the status once, with the real value (D2, D9)', () => {
	it('renders one UNRESOLVED pill and no platform label', () => {
		render(Page, {
			data: pageData({
				facets,
				issues: [
					{
						id: 'i1',
						projectId: 'proj-1',
						projectName: 'Checkout',
						message: 'upstream timeout from node',
						errorClass: 'TimeoutError',
						environment: 'production',
						status: 'unresolved',
						firstSeen: new Date('2026-01-01'),
						lastSeen: new Date('2026-01-02'),
						count: 3,
						affectedUserCount: 0,
						affectedTenantCount: 0,
						regressionStatus: 'none',
						lastRegressedAt: null,
						assignedTo: null,
						assigneeType: null,
						trend24h: new Array(24).fill(0),
					},
				],
			}) as unknown as PageData,
		});
		expect(document.querySelectorAll('.status-pill')).toHaveLength(1);
		expect(text(document.querySelector('.status-pill'))).toBe('UNRESOLVED');
		expect(text(document.querySelector('.meta .project'))).toBe('Checkout');
	});
});
