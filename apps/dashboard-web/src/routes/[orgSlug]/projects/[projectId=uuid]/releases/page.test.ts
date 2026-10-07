import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import type { Component } from 'svelte';
import Page from './+page.svelte';

// sentinel-releases-snooze fe.md task 1, contract §11's `@testing-library/svelte` row — flows
// `R2`–`R4`, `R6`: the table's five columns in order, one row per release in the order the loader
// sent them (never re-sorted here), the empty-state copy on `releases: []`, the truncation note
// iff `truncated`, timestamps through the detail page's `formatDate` idiom, and no link on a row.
//
// Page data is a LITERAL typed by hand from contract §4.4 (never `$types`). No DB, no loader.
// Layout — the heading, the back link, spacing — is guidance and is NOT asserted.
//
// sentinel-sourcemaps §4.6 added a sixth column, Maps (`sourcemapCount`, `0` never absent), and a
// control in its cell — so the header list is six and every row carries a sixth cell here. The
// column's own behaviour (the count, the expand control, §4.2) is `page.sourcemaps.test.ts` beside
// this file; this file only keeps the five original columns pinned.

interface ReleaseRow {
	version: string;
	firstSeen: Date;
	lastSeen: Date;
	occurrenceCount: number;
	issuesIntroduced: number;
	sourcemapCount: number;
}

interface ReleasesPageData {
	org: { id: string; slug: string; name: string };
	project: { id: string; name: string };
	releases: ReleaseRow[];
	truncated: boolean;
}

const ReleasesPage = Page as unknown as Component<{ data: ReleasesPageData }>;

// §4.4's own excerpt, newest first as the loader orders (`first_seen DESC`).
const RELEASES: ReleaseRow[] = [
	{
		version: 'a1b2c3d',
		firstSeen: new Date('2026-09-12T14:02:11Z'),
		lastSeen: new Date('2026-09-13T08:40:00Z'),
		occurrenceCount: 1204,
		issuesIntroduced: 3,
		sourcemapCount: 2,
	},
	{
		version: '9f8e7d6',
		firstSeen: new Date('2026-09-10T09:00:00Z'),
		lastSeen: new Date('2026-09-12T14:01:59Z'),
		occurrenceCount: 88,
		issuesIntroduced: 0,
		sourcemapCount: 0,
	},
];

function pageData(overrides: Partial<ReleasesPageData> = {}): ReleasesPageData {
	return {
		org: { id: '0a9e0000-0000-4000-8000-00000000000a', slug: 'acme', name: 'Acme' },
		project: { id: '0b1f0000-0000-4000-8000-000000000001', name: 'checkout' },
		releases: RELEASES,
		truncated: false,
		...overrides,
	};
}

function text(el: Element | null | undefined): string {
	return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

// The detail page's `formatDate`, reproduced so the cells are asserted in the viewer's local time
// whatever TZ the test runs under.
function formatDateLikeTheDetailPage(date: Date): string {
	return date.toLocaleString('en-US', {
		year: 'numeric',
		month: 'short',
		day: 'numeric',
		hour: '2-digit',
		minute: '2-digit',
		second: '2-digit',
	});
}

function rows(): string[][] {
	return Array.from(screen.getByTestId('releases-table').querySelectorAll('tbody tr')).map((tr) =>
		Array.from(tr.querySelectorAll('td')).map(text)
	);
}

describe('/{orgSlug}/projects/{projectId}/releases the table (contract §4.4, flows R2, R6)', () => {
	it('renders the five columns in order: Version · First seen · Last seen · Occurrences · Issues introduced (then sourcemaps\' Maps)', () => {
		render(ReleasesPage, { data: pageData() });
		const headers = Array.from(screen.getByTestId('releases-table').querySelectorAll('thead th')).map(text);
		expect(headers).toEqual(['Version', 'First seen', 'Last seen', 'Occurrences', 'Issues introduced', 'Maps']);
	});

	it('renders one row per release in the order the loader sent them — newest first, never re-sorted', () => {
		render(ReleasesPage, { data: pageData() });
		expect(rows()).toEqual([
			[
				'a1b2c3d',
				formatDateLikeTheDetailPage(RELEASES[0].firstSeen),
				formatDateLikeTheDetailPage(RELEASES[0].lastSeen),
				'1,204',
				'3',
				'2 Show',
			],
			[
				'9f8e7d6',
				formatDateLikeTheDetailPage(RELEASES[1].firstSeen),
				formatDateLikeTheDetailPage(RELEASES[1].lastSeen),
				'88',
				'0',
				'0 Show',
			],
		]);
	});

	it('keeps the loader order even when the versions would sort the other way (decision 1)', () => {
		render(ReleasesPage, {
			data: pageData({
				releases: [
					{ ...RELEASES[0], version: 'v1.9.0' },
					{ ...RELEASES[1], version: 'v1.10.0' },
				],
			}),
		});
		expect(rows().map((r) => r[0])).toEqual(['v1.9.0', 'v1.10.0']);
	});

	it('links out from nothing on a row (R6)', () => {
		render(ReleasesPage, { data: pageData() });
		expect(screen.getByTestId('releases-table').querySelectorAll('a').length).toBe(0);
	});

	it('shows no truncation note when truncated is false', () => {
		render(ReleasesPage, { data: pageData({ truncated: false }) });
		expect(screen.queryByTestId('releases-truncated')).toBeNull();
		expect(screen.queryByText('Showing the 200 most recent')).toBeNull();
	});
});

describe('/{orgSlug}/projects/{projectId}/releases the empty state (contract §4.4, flow R3)', () => {
	it('renders the empty-state copy on releases: [] and no table', () => {
		render(ReleasesPage, { data: pageData({ releases: [] }) });
		expect(text(screen.getByTestId('releases-empty'))).toBe(
			'No releases yet — send release_version with your events and they appear here'
		);
		expect(screen.queryByTestId('releases-table')).toBeNull();
		expect(screen.queryByTestId('releases-truncated')).toBeNull();
	});

	it('renders no empty-state copy when there are releases', () => {
		render(ReleasesPage, { data: pageData() });
		expect(screen.queryByTestId('releases-empty')).toBeNull();
	});
});

describe('/{orgSlug}/projects/{projectId}/releases the truncation note (contract §4.4, flow R4, decision 24)', () => {
	it('renders "Showing the 200 most recent" above the table iff truncated', () => {
		const releases = Array.from({ length: 200 }, (_, i) => ({
			version: `v0.0.${300 - i}`,
			firstSeen: new Date(Date.UTC(2026, 8, 13, 0, 0, 0) - i * 60_000),
			lastSeen: new Date(Date.UTC(2026, 8, 13, 0, 0, 0) - i * 60_000),
			occurrenceCount: 1,
			issuesIntroduced: 0,
			sourcemapCount: 0,
		}));
		render(ReleasesPage, { data: pageData({ releases, truncated: true }) });
		const note = screen.getByTestId('releases-truncated');
		expect(text(note)).toBe('Showing the 200 most recent');
		const table = screen.getByTestId('releases-table');
		expect(note.compareDocumentPosition(table) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
		expect(rows().length).toBe(200);
		expect(rows()[0][0]).toBe('v0.0.300');
		expect(rows()[199][0]).toBe('v0.0.101');
	});
});
