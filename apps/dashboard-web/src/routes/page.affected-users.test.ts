import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import Page from './+page.svelte';
import type { PageData } from './$types';

// sentinel-affected-users fe.md task 7, contract §11's `@testing-library/svelte` row — flows `L1`
// (the two figures and the zero shape), `L2`/`L3` (the sort control and its active state),
// `L4`–`L7` (`href()` carrying `sort`, omitting it when `last_seen`).
//
// Every case is a contract §4.1 claim about what the TEMPLATE does with page data, so it is
// asserted against a literal — including `retentionDays`, which is the loader's (decision 21) and
// which the template must never hard-code. The loader's coercion of `sort` is BE's and is pinned in
// `page.server.test.ts`; here `filters.sort` is always the coerced value, as §4.1 promises it is.

function issue(overrides: Record<string, unknown> = {}) {
	return {
		id: 'issue-1',
		errorClass: 'TimeoutError',
		message: 'upstream timed out',
		count: 12,
		lastSeen: '2026-09-12T10:00:00.000Z',
		projectName: 'Checkout API',
		environment: 'production',
		status: 'open',
		affectedUserCount: 1234,
		affectedTenantCount: 3,
		// sentinel-issue-trend §4.1 — the projection and the per-card series the template now reads
		// on every row (`trend24h` is dense, never absent). Not this file's subject; see
		// `page.issue-trend.test.ts`.
		regressionStatus: 'none',
		lastRegressedAt: null,
		trend24h: new Array(24).fill(0),
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

function text(el: Element | null | undefined): string {
	return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

describe('/ affected users and tenants on the card (contract §4.1, flow L1)', () => {
	it('renders both figures through toLocaleString() with ONE window suffix after the tenant figure', () => {
		render(Page, { data: pageData({ retentionDays: 45 }) as unknown as PageData });

		const line = screen.getByTestId('issue-affected');
		expect(text(line)).toBe('1,234 users · 3 tenants in the last 45 days');
		// The window is page data, so 45 is what the label says — and it says it once, not per
		// figure.
		expect(text(line).match(/in the last/g)?.length).toBe(1);
		expect(text(screen.getByTestId('issue-affected-users'))).toBe('1,234 users');
		expect(text(screen.getByTestId('issue-affected-tenants'))).toBe('3 tenants');
		// Not the zero shape: no dash span anywhere on the line.
		expect(line.querySelector('[title]')).toBeNull();
	});

	it('renders a 0 as a dash on its own span with the decided title — the unit words and the suffix stay (decision 16)', () => {
		render(Page, {
			data: pageData({
				issues: [issue({ affectedUserCount: 0, affectedTenantCount: 3 })],
				retentionDays: 30,
			}) as unknown as PageData,
		});

		const line = screen.getByTestId('issue-affected');
		expect(text(line)).toBe('— users · 3 tenants in the last 30 days');

		const users = screen.getByTestId('issue-affected-users');
		const dash = users.querySelector('span[title]');
		expect(dash).not.toBeNull();
		expect(dash?.textContent).toBe('—');
		expect(dash?.getAttribute('title')).toBe('no user id reported in the last 30 days');
		// The dash replaced the NUMBER only; the unit word is outside the span.
		expect(text(users)).toBe('— users');
		// The tenant figure is a number and carries no title.
		expect(screen.getByTestId('issue-affected-tenants').querySelector('[title]')).toBeNull();
	});

	it('puts the tenant string on the tenant dash, and both carry the page-data window', () => {
		render(Page, {
			data: pageData({
				issues: [issue({ affectedUserCount: 0, affectedTenantCount: 0 })],
				retentionDays: 7,
			}) as unknown as PageData,
		});

		expect(text(screen.getByTestId('issue-affected'))).toBe(
			'— users · — tenants in the last 7 days'
		);
		expect(
			screen.getByTestId('issue-affected-users').querySelector('span[title]')?.getAttribute('title')
		).toBe('no user id reported in the last 7 days');
		expect(
			screen
				.getByTestId('issue-affected-tenants')
				.querySelector('span[title]')
				?.getAttribute('title')
		).toBe('no tenant id reported in the last 7 days');
	});

	// Flow `L8`, the composition chosen: the environment scope note stays on the occurrence span,
	// and the impact line reads it once — `{count} occurrences in {env} · {n} users · {n} tenants
	// in the last N days`.
	it('keeps the environment scope note on the occurrence span so it and the window suffix each read once — flow L8', () => {
		render(Page, {
			data: pageData({
				environments: [{ id: 'env-1', name: 'production', displayName: 'Production' }],
				filters: { status: 'all', project: 'proj-1', environment: 'production', sort: 'last_seen', new: 'all' },
			}) as unknown as PageData,
		});

		const footer = document.querySelector('.card-footer .impact');
		// The scope note is the existing `.count-scope` span INSIDE `.count` (its gap is CSS, so the
		// text nodes abut), and the impact line follows it once.
		const count = footer?.querySelector('.count');
		expect(count?.querySelector('.count-scope')?.textContent).toBe('in Production');
		expect(text(count)).toBe('12 occurrencesin Production');
		expect(text(footer)).toBe(
			'12 occurrencesin Production · 1,234 users · 3 tenants in the last 30 days'
		);
		expect(text(footer).match(/in Production/g)?.length).toBe(1);
		expect(text(footer).match(/in the last/g)?.length).toBe(1);
		// And the note is not repeated on the affected line.
		expect(text(screen.getByTestId('issue-affected'))).not.toContain('Production');
	});
});

describe('/ sort control (contract §4.1, flows L2–L7, decision 17)', () => {
	it('renders Last seen and Affected users as anchors and marks the active one from filters.sort', () => {
		const lastSeen = render(Page, { data: pageData() as unknown as PageData });
		const control = screen.getByTestId('sort-control');
		expect(control).toBeTruthy();
		expect(screen.getByTestId('sort-last_seen').classList.contains('active')).toBe(true);
		expect(screen.getByTestId('sort-affected_users').classList.contains('active')).toBe(false);
		expect(text(screen.getByTestId('sort-last_seen'))).toBe('Last seen');
		expect(text(screen.getByTestId('sort-affected_users'))).toBe('Affected users');
		lastSeen.unmount();

		render(Page, {
			data: pageData({
				filters: { status: 'all', project: 'proj-1', environment: 'all', sort: 'affected_users', new: 'all' },
			}) as unknown as PageData,
		});
		expect(screen.getByTestId('sort-affected_users').classList.contains('active')).toBe(true);
		expect(screen.getByTestId('sort-last_seen').classList.contains('active')).toBe(false);
	});

	it('emits sort=affected_users on the sort anchor and omits sort on the Last seen anchor — L2, L3, L7', () => {
		render(Page, {
			data: pageData({
				pagination: { page: 2, limit: 20, total: 60, totalPages: 3 },
			}) as unknown as PageData,
		});

		expect(screen.getByTestId('sort-affected_users').getAttribute('href')).toBe(
			'?status=all&project=proj-1&sort=affected_users'
		);
		// The default is the absence: no `sort=last_seen` on the anchor that selects it.
		expect(screen.getByTestId('sort-last_seen').getAttribute('href')).toBe(
			'?status=all&project=proj-1'
		);
		// A change of order does not carry `page` — the sorted list starts from page 1.
		expect(screen.getByTestId('sort-affected_users').getAttribute('href')).not.toContain('page=');
	});

	it('carries sort=affected_users through paging, status, environment AND project anchors — L4, L5, L6', () => {
		render(Page, {
			data: pageData({
				environments: [{ id: 'env-1', name: 'production', displayName: 'Production' }],
				filters: {
					status: 'open',
					project: 'proj-1',
					environment: 'production',
					sort: 'affected_users',
					new: 'all',
				},
				pagination: { page: 2, limit: 20, total: 60, totalPages: 3 },
			}) as unknown as PageData,
		});

		// L4 — paging.
		expect(screen.getByRole('link', { name: 'Next' }).getAttribute('href')).toBe(
			'?status=open&project=proj-1&environment=production&sort=affected_users&page=3'
		);
		expect(screen.getByRole('link', { name: 'Previous' }).getAttribute('href')).toBe(
			'?status=open&project=proj-1&environment=production&sort=affected_users'
		);
		// L5 — status and environment.
		expect(screen.getByRole('link', { name: 'Resolved' }).getAttribute('href')).toBe(
			'?status=resolved&project=proj-1&environment=production&sort=affected_users'
		);
		expect(screen.getByRole('link', { name: 'Production' }).getAttribute('href')).toBe(
			'?status=open&project=proj-1&environment=production&sort=affected_users'
		);
		// L6, decision 17 — a project change keeps the sort; only `environment` resets.
		expect(screen.getByRole('link', { name: 'Billing' }).getAttribute('href')).toBe(
			'?status=open&project=proj-2&sort=affected_users'
		);
		expect(screen.getByRole('link', { name: 'All Projects' }).getAttribute('href')).toBe(
			'?status=open&project=all&sort=affected_users'
		);
	});

	// Flow `L7`: `/?sort=banana` — the loader echoes `last_seen`, and the page builds every anchor
	// from that echo, so nothing on the page can carry the bad string.
	it('carries no sort at all when filters.sort is the coerced last_seen — L7', () => {
		render(Page, {
			data: pageData({
				environments: [{ id: 'env-1', name: 'production', displayName: 'Production' }],
				filters: { status: 'all', project: 'proj-1', environment: 'all', sort: 'last_seen', new: 'all' },
				pagination: { page: 1, limit: 20, total: 60, totalPages: 3 },
			}) as unknown as PageData,
		});

		expect(screen.getByTestId('sort-last_seen').classList.contains('active')).toBe(true);
		const hrefs = Array.from(document.querySelectorAll('a[href^="?"]')).map((a) =>
			a.getAttribute('href')
		);
		expect(hrefs.length).toBeGreaterThan(5);
		for (const h of hrefs) {
			expect(h).not.toContain('banana');
			expect(h).not.toContain('sort=last_seen');
		}
		// Only the anchors that select ANOTHER order carry `sort` — one per non-default entry of
		// `sortOptions`: `affected_users`, and since sentinel-issue-trend decision 5, `count` and
		// `first_seen`.
		expect(hrefs.filter((h) => h?.includes('sort=')).length).toBe(3);
		expect(screen.getByRole('link', { name: 'Next' }).getAttribute('href')).toBe(
			'?status=all&project=proj-1&page=2'
		);
	});
});
