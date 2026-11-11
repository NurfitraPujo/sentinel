import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import Page from './+page.svelte';
import type { PageData } from './$types';

// sentinel-affected-users fe.md task 7, contract §11's `@testing-library/svelte` row — flow `D1`:
// the two `.meta-item`s after `Occurrences:`, their labels carrying the window from page data, and
// the zero shape shared with the list card (decision 16). Page data is a literal, including
// `retentionDays` (decision 21); nothing here touches the loader.

function pageData(overrides: Record<string, unknown> = {}) {
	return {
		issue: {
			id: 'issue-1',
			projectId: 'proj-1',
			errorClass: 'TimeoutError',
			message: 'upstream timed out',
			status: 'open',
			environment: 'production',
			count: 12,
			firstSeen: '2026-09-01T10:00:00.000Z',
			lastSeen: '2026-09-12T10:00:00.000Z',
			affectedUserCount: 1234,
			affectedTenantCount: 3,
		},
		project: { id: 'proj-1', name: 'Checkout API' },
		environmentDisplayName: 'Production',
		occurrences: [],
		occurrenceTotal: 12,
		relations: [],
		retentionDays: 30,
		// sentinel-issue-trend §4.2 — the two dense series the template now reads (never absent). Not
		// this file's subject; see `page.issue-trend.test.ts` beside it.
		trend: {
			hourly: Array.from({ length: 24 }, (_, i) => ({
				start: new Date(Date.UTC(2026, 8, 12, 15 + i)).toISOString().slice(0, 19) + 'Z',
				count: 0,
			})),
			daily: Array.from({ length: 30 }, (_, i) => ({
				start: new Date(Date.UTC(2026, 7, 15 + i)).toISOString().slice(0, 19) + 'Z',
				count: 0,
			})),
		},
		...overrides,
	};
}

function text(el: Element | null | undefined): string {
	return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

describe('/issues/{id} affected users and tenants (contract §4.2, flow D1)', () => {
	it('renders the two meta items after Occurrences with the window in each label', () => {
		render(Page, { data: pageData({ retentionDays: 45 }) as unknown as PageData });

		const users = screen.getByTestId('issue-affected-users');
		const tenants = screen.getByTestId('issue-affected-tenants');
		expect(text(users)).toBe('Affected users (last 45 days): 1,234');
		expect(text(tenants)).toBe('Affected tenants (last 45 days): 3');
		expect(users.querySelector('.meta-label')?.textContent).toBe('Affected users (last 45 days):');
		expect(tenants.querySelector('.meta-label')?.textContent).toBe(
			'Affected tenants (last 45 days):'
		);

		// Order inside `.meta-info`: Occurrences, then users, then tenants.
		const items = Array.from(document.querySelectorAll('.meta-info .meta-item')).map(text);
		const occurrences = items.findIndex((t) => t.startsWith('Occurrences:'));
		expect(occurrences).toBeGreaterThanOrEqual(0);
		expect(items[occurrences + 1]).toBe('Affected users (last 45 days): 1,234');
		expect(items[occurrences + 2]).toBe('Affected tenants (last 45 days): 3');
		// Not the zero shape.
		expect(users.querySelector('[title]')).toBeNull();
		expect(tenants.querySelector('[title]')).toBeNull();
	});

	it('renders a 0 as a dash on its own span with the decided title strings — decision 16', () => {
		render(Page, {
			data: pageData({
				issue: {
					...pageData().issue,
					affectedUserCount: 0,
					affectedTenantCount: 0,
				},
				retentionDays: 30,
			}) as unknown as PageData,
		});

		const users = screen.getByTestId('issue-affected-users');
		const tenants = screen.getByTestId('issue-affected-tenants');
		expect(text(users)).toBe('Affected users (last 30 days): —');
		expect(text(tenants)).toBe('Affected tenants (last 30 days): —');

		const userDash = users.querySelector('span[title]');
		expect(userDash?.textContent).toBe('—');
		expect(userDash?.getAttribute('title')).toBe('no user id reported in the last 30 days');
		const tenantDash = tenants.querySelector('span[title]');
		expect(tenantDash?.textContent).toBe('—');
		expect(tenantDash?.getAttribute('title')).toBe('no tenant id reported in the last 30 days');
	});
});
