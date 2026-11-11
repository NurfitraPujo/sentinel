import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import Page from './+page.svelte';
import type { PageData } from './$types';

// sentinel-retention-resolved-ignored-kept contract §5.2: retention keeps resolved / ignored /
// assigned / commented issues after their occurrences have aged out. The page must show the
// lifetime count and an explicit empty state, never 0 or a bare empty table.

function pageData(overrides: Record<string, unknown> = {}, issue: Record<string, unknown> = {}) {
	return {
		issue: {
			id: 'issue-1',
			projectId: 'proj-1',
			errorClass: 'TimeoutError',
			message: 'upstream timed out',
			status: 'resolved',
			issueType: 'system_error',
			environment: 'production',
			count: 4321,
			firstSeen: '2026-01-01T10:00:00.000Z',
			lastSeen: '2026-01-12T10:00:00.000Z',
			affectedUserCount: 0,
			affectedTenantCount: 0,
			...issue,
		},
		project: { id: 'proj-1', name: 'Checkout API' },
		environmentDisplayName: 'Production',
		occurrences: [],
		occurrenceTotal: 0,
		relations: [],
		retentionDays: 7,
		trend: { hourly: [], daily: [] },
		...overrides,
	};
}

describe('/issues/{id} for a kept issue whose occurrences have aged out', () => {
	it('shows the lifetime count in the heading and the explicit empty state', () => {
		render(Page, { data: pageData() as unknown as PageData });
		expect(screen.getByText(/Occurrence History \(4,321 total\)/)).toBeTruthy();
		expect(screen.getByTestId('occurrences-aged-out').textContent).toContain(
			'Occurrences older than 7 days have been removed.'
		);
	});

	it('labels the windowed figure when some occurrences remain', () => {
		render(Page, { data: pageData({ occurrenceTotal: 12 }) as unknown as PageData });
		expect(screen.queryByTestId('occurrences-aged-out')).toBeNull();
		expect(screen.getByTestId('occurrences-partially-aged-out').textContent).toContain(
			'12 retained from the last 7 days, of 4,321 total.'
		);
	});

	it('shows neither note when the retained total equals the lifetime count', () => {
		render(Page, { data: pageData({ occurrenceTotal: 4321 }) as unknown as PageData });
		expect(screen.queryByTestId('occurrences-aged-out')).toBeNull();
		expect(screen.queryByTestId('occurrences-partially-aged-out')).toBeNull();
	});

	it('never shows the aged-out note for a user_report issue', () => {
		render(Page, {
			data: pageData({}, { issueType: 'user_report' }) as unknown as PageData,
		});
		expect(screen.queryByTestId('occurrences-aged-out')).toBeNull();
	});
});
