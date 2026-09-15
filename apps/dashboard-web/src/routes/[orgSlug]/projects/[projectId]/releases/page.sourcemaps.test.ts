import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor, within } from '@testing-library/svelte';
import type { Component } from 'svelte';
import Page from './+page.svelte';

// sentinel-sourcemaps fe.md tasks 1, 2 and 6 — contract §4.6 (the Maps column) and §4.2 (what the
// expand control fetches), flows `R1`–`R3`: the sixth column renders each row's `sourcemapCount`
// and a `0` renders `0` (never a dash); the first expand calls `fetch` exactly once with
// `data.org.id`, the project id and the `encodeURIComponent`ed version, then renders path, size and
// uploaded; `files: []` renders the *No sourcemaps* sentence; a rejected fetch renders the error
// line; expand → collapse → expand leaves the mock's call count at one — that assertion IS the
// no-refetch proof, not a network tab.
//
// Page data is a LITERAL typed by hand from contract §4.6 (never `$types`), the §4.2 response a
// literal from the contract's own excerpt; `fetch` is a mock. No DB, no loader, no dev server.
// Placement of the column and the arrangement of the expanded region are the delegated visual
// decision (screenshot owed, item 8) and are NOT asserted.

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

// §4.2's `SourcemapRow`, camelCase — no `storageKey`, nothing that is a map byte (contract §1).
interface SourcemapRow {
	id: string;
	minifiedPath: string;
	sizeBytes: number;
	sha256: string;
	uploadedAt: string;
}

const ReleasesPage = Page as unknown as Component<{ data: ReleasesPageData }>;

const ORG_ID = '0a9e0000-0000-4000-8000-00000000000a';
const PROJECT_ID = '0b1f0000-0000-4000-8000-000000000001';

// A version with characters that MUST be encoded on the query string (a `+` build tag and a `/`),
// so the test can tell `encodeURIComponent` from string concatenation.
const MAPPED_VERSION = '2026.09.14-3f2a1c+build/7';
const UNMAPPED_VERSION = '2026.09.13-11aa22';

const RELEASES: ReleaseRow[] = [
	{
		version: MAPPED_VERSION,
		firstSeen: new Date('2026-09-14T09:02:11Z'),
		lastSeen: new Date('2026-09-14T12:40:00Z'),
		occurrenceCount: 1204,
		issuesIntroduced: 3,
		sourcemapCount: 2,
	},
	{
		version: UNMAPPED_VERSION,
		firstSeen: new Date('2026-09-13T09:00:00Z'),
		lastSeen: new Date('2026-09-14T09:01:59Z'),
		occurrenceCount: 88,
		issuesIntroduced: 0,
		sourcemapCount: 0,
	},
];

// §4.2's own excerpt: two maps, `minified_path ASC`.
const MAPPED_FILES: SourcemapRow[] = [
	{
		id: '0f2c0000-0000-4000-8000-000000000001',
		minifiedPath: '/assets/index-3f2a1c.js',
		sizeBytes: 4193821,
		sha256: '9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08',
		uploadedAt: '2026-09-14T08:12:44Z',
	},
	{
		id: '1a7b0000-0000-4000-8000-000000000002',
		minifiedPath: '/assets/vendor-9a0d4e.js',
		sizeBytes: 11820044,
		sha256: 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
		uploadedAt: '2026-09-14T08:12:44Z',
	},
];

function pageData(overrides: Partial<ReleasesPageData> = {}): ReleasesPageData {
	return {
		org: { id: ORG_ID, slug: 'acme', name: 'Acme' },
		project: { id: PROJECT_ID, name: 'checkout' },
		releases: RELEASES,
		truncated: false,
		...overrides,
	};
}

function text(el: Element | null | undefined): string {
	return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

// The page's own `formatDate`, reproduced so `uploaded` is asserted in the viewer's local time
// whatever TZ the test runs under.
function formatDateLikeThePage(date: string): string {
	return new Date(date).toLocaleString('en-US', {
		year: 'numeric',
		month: 'short',
		day: 'numeric',
		hour: '2-digit',
		minute: '2-digit',
		second: '2-digit',
	});
}

function jsonResponse(body: unknown, status = 200): Response {
	return new Response(JSON.stringify(body), {
		status,
		headers: { 'Content-Type': 'application/json' },
	});
}

function stubFetch(impl: (...args: unknown[]) => Promise<unknown>) {
	const fetchMock = vi.fn(impl);
	vi.stubGlobal('fetch', fetchMock);
	return fetchMock;
}

afterEach(() => {
	vi.unstubAllGlobals();
});

function releaseRows(): HTMLTableRowElement[] {
	return Array.from(
		screen.getByTestId('releases-table').querySelectorAll<HTMLTableRowElement>('tbody tr')
	).filter((tr) => !tr.matches('[data-testid="release-sourcemaps"]'));
}

function rowFor(version: string): HTMLTableRowElement {
	const row = releaseRows().find((tr) => text(tr.querySelector('td')) === version);
	expect(row, `a row for ${version}`).toBeDefined();
	return row as HTMLTableRowElement;
}

function toggleOf(version: string): HTMLButtonElement {
	return within(rowFor(version)).getByTestId('release-sourcemaps-toggle') as HTMLButtonElement;
}

function expandedRegionOf(version: string): HTMLElement | null {
	const next = rowFor(version).nextElementSibling;
	return next && next.matches('[data-testid="release-sourcemaps"]') ? (next as HTMLElement) : null;
}

const EXPECTED_URL = `/api/organizations/${ORG_ID}/projects/${PROJECT_ID}/sourcemaps?release_version=${encodeURIComponent(MAPPED_VERSION)}`;

describe('/{orgSlug}/projects/{projectId}/releases the Maps column (contract §4.6, flow R1)', () => {
	it('renders Maps as the sixth header, after Issues introduced', () => {
		render(ReleasesPage, { data: pageData() });
		const headers = Array.from(
			screen.getByTestId('releases-table').querySelectorAll('thead th')
		).map(text);
		expect(headers).toHaveLength(6);
		expect(headers[5]).toBe('Maps');
		expect(headers[4]).toBe('Issues introduced');
	});

	it("renders each row's sourcemapCount in the sixth cell", () => {
		render(ReleasesPage, { data: pageData() });
		const counts = releaseRows().map((tr) =>
			text(tr.querySelector('[data-testid="release-sourcemap-count"]'))
		);
		expect(counts).toEqual(['2', '0']);
		// It is the SIXTH cell of the row, not a cell somewhere else.
		const sixthCells = releaseRows().map((tr) => tr.querySelectorAll('td')[5]);
		expect(sixthCells.map((td) => text(td.querySelector('[data-testid="release-sourcemap-count"]')))).toEqual(['2', '0']);
	});

	it('renders 0 for a release with no maps — never a dash (a release with no maps is a fact)', () => {
		render(ReleasesPage, { data: pageData() });
		const cell = within(rowFor(UNMAPPED_VERSION)).getByTestId('release-sourcemap-count');
		expect(text(cell)).toBe('0');
		expect(text(cell)).not.toContain('—');
		expect(text(cell)).not.toContain('-');
	});

	it('formats the count like its neighbours (toLocaleString)', () => {
		render(ReleasesPage, {
			data: pageData({ releases: [{ ...RELEASES[0], sourcemapCount: 1204 }] }),
		});
		expect(text(screen.getByTestId('release-sourcemap-count'))).toBe('1,204');
	});

	it('renders an expand control on every row, a 0 row included, and no expanded region until it is used', () => {
		render(ReleasesPage, { data: pageData() });
		expect(screen.getAllByTestId('release-sourcemaps-toggle')).toHaveLength(2);
		expect(screen.queryByTestId('release-sourcemaps')).toBeNull();
		expect(toggleOf(MAPPED_VERSION).getAttribute('aria-expanded')).toBe('false');
	});
});

describe('/{orgSlug}/projects/{projectId}/releases the expand control (contract §4.2, flow R2)', () => {
	it('expanding a row calls fetch exactly once with /api/organizations/{data.org.id}/projects/{projectId}/sourcemaps?release_version=<encodeURIComponent(version)>', async () => {
		const fetchMock = stubFetch(async () =>
			jsonResponse({ releaseVersion: MAPPED_VERSION, files: MAPPED_FILES })
		);
		render(ReleasesPage, { data: pageData() });

		await fireEvent.click(toggleOf(MAPPED_VERSION));

		expect(fetchMock).toHaveBeenCalledTimes(1);
		const [url] = fetchMock.mock.calls[0] as [string];
		expect(url).toBe(EXPECTED_URL);
		// The org segment is the id the loader hands over, never the slug (§4.2's path).
		expect(url).toContain(`/api/organizations/${ORG_ID}/`);
		expect(url).not.toContain('/acme/');
		// The version is encoded: `+` and `/` do not survive verbatim on the query string.
		expect(url).toContain('release_version=2026.09.14-3f2a1c%2Bbuild%2F7');
		expect(url).not.toContain('+build/7');
	});

	it('shows a loading state until the response lands', async () => {
		let resolveFetch: (r: Response) => void = () => {};
		stubFetch(() => new Promise<Response>((resolve) => (resolveFetch = resolve)));
		render(ReleasesPage, { data: pageData() });

		await fireEvent.click(toggleOf(MAPPED_VERSION));

		const region = expandedRegionOf(MAPPED_VERSION);
		expect(region).not.toBeNull();
		expect(within(region as HTMLElement).getByTestId('release-sourcemaps-loading')).toBeTruthy();
		expect(toggleOf(MAPPED_VERSION).getAttribute('aria-expanded')).toBe('true');

		resolveFetch(jsonResponse({ releaseVersion: MAPPED_VERSION, files: MAPPED_FILES }));
		await waitFor(() => expect(screen.queryByTestId('release-sourcemaps-loading')).toBeNull());
		expect(screen.getByTestId('release-sourcemaps-list')).toBeTruthy();
	});

	it('renders path, size and uploaded for every file, in the order §4.2 sent them, under the row', async () => {
		stubFetch(async () => jsonResponse({ releaseVersion: MAPPED_VERSION, files: MAPPED_FILES }));
		render(ReleasesPage, { data: pageData() });

		await fireEvent.click(toggleOf(MAPPED_VERSION));
		const list = await screen.findByTestId('release-sourcemaps-list');

		// The region is the row right after the release's own row.
		expect(expandedRegionOf(MAPPED_VERSION)?.contains(list)).toBe(true);

		const headers = Array.from(list.querySelectorAll(':scope > thead th')).map(text);
		expect(headers).toEqual(['Path', 'Size', 'Uploaded']);

		const rows = Array.from(list.querySelectorAll(':scope > tbody > tr')).map((tr) =>
			Array.from(tr.querySelectorAll('td')).map(text)
		);
		expect(rows).toEqual([
			['/assets/index-3f2a1c.js', '4.0 MB', formatDateLikeThePage('2026-09-14T08:12:44Z')],
			['/assets/vendor-9a0d4e.js', '11.3 MB', formatDateLikeThePage('2026-09-14T08:12:44Z')],
		]);

		// The path is the full string, monospace by class — the expanded region has the width.
		const pathCells = Array.from(list.querySelectorAll(':scope > tbody > tr > td:first-child'));
		expect(pathCells.every((td) => td.classList.contains('cell-path'))).toBe(true);
	});

	it('renders sizes below a mebibyte in KB, one decimal', async () => {
		stubFetch(async () =>
			jsonResponse({
				releaseVersion: MAPPED_VERSION,
				files: [{ ...MAPPED_FILES[0], sizeBytes: 15_872 }],
			})
		);
		render(ReleasesPage, { data: pageData() });

		await fireEvent.click(toggleOf(MAPPED_VERSION));
		const list = await screen.findByTestId('release-sourcemaps-list');
		expect(text(list.querySelectorAll(':scope > tbody > tr > td')[1])).toBe('15.5 KB');
	});

	it('never renders sha256, storageKey or any other key §4.2 does not put on the page', async () => {
		stubFetch(async () =>
			jsonResponse({
				releaseVersion: MAPPED_VERSION,
				files: [{ ...MAPPED_FILES[0], storageKey: 'sourcemaps/should-never-render' }],
			})
		);
		render(ReleasesPage, { data: pageData() });

		await fireEvent.click(toggleOf(MAPPED_VERSION));
		const region = (await screen.findByTestId('release-sourcemaps-list')).closest(
			'[data-testid="release-sourcemaps"]'
		);
		expect(text(region)).not.toContain('should-never-render');
		expect(text(region)).not.toContain(MAPPED_FILES[0].sha256);
	});

	it('expand → collapse → expand leaves the mock at ONE call and re-renders the kept list (no refetch)', async () => {
		const fetchMock = stubFetch(async () =>
			jsonResponse({ releaseVersion: MAPPED_VERSION, files: MAPPED_FILES })
		);
		render(ReleasesPage, { data: pageData() });

		await fireEvent.click(toggleOf(MAPPED_VERSION));
		await screen.findByTestId('release-sourcemaps-list');
		expect(fetchMock).toHaveBeenCalledTimes(1);
		expect(text(toggleOf(MAPPED_VERSION))).toBe('Hide');

		await fireEvent.click(toggleOf(MAPPED_VERSION));
		expect(expandedRegionOf(MAPPED_VERSION)).toBeNull();
		expect(screen.queryByTestId('release-sourcemaps-list')).toBeNull();
		expect(toggleOf(MAPPED_VERSION).getAttribute('aria-expanded')).toBe('false');
		expect(text(toggleOf(MAPPED_VERSION))).toBe('Show');

		await fireEvent.click(toggleOf(MAPPED_VERSION));
		// The kept data renders synchronously — no loading state, no second request.
		expect(screen.queryByTestId('release-sourcemaps-loading')).toBeNull();
		expect(screen.getByTestId('release-sourcemaps-list')).toBeTruthy();
		expect(fetchMock).toHaveBeenCalledTimes(1);
	});

	it('fetches per row: two rows expanded are two requests, each with its own version', async () => {
		const fetchMock = stubFetch(async (input: unknown) => {
			const url = String(input);
			return jsonResponse({
				releaseVersion: '',
				files: url.includes(encodeURIComponent(MAPPED_VERSION)) ? MAPPED_FILES : [],
			});
		});
		render(ReleasesPage, { data: pageData() });

		await fireEvent.click(toggleOf(MAPPED_VERSION));
		await fireEvent.click(toggleOf(UNMAPPED_VERSION));

		expect(fetchMock).toHaveBeenCalledTimes(2);
		expect(fetchMock.mock.calls.map((c) => String(c[0]))).toEqual([
			EXPECTED_URL,
			`/api/organizations/${ORG_ID}/projects/${PROJECT_ID}/sourcemaps?release_version=${encodeURIComponent(UNMAPPED_VERSION)}`,
		]);
		await screen.findByTestId('release-sourcemaps-list');
		await screen.findByTestId('release-sourcemaps-empty');
		expect(within(expandedRegionOf(MAPPED_VERSION) as HTMLElement).getByTestId('release-sourcemaps-list')).toBeTruthy();
		expect(within(expandedRegionOf(UNMAPPED_VERSION) as HTMLElement).getByTestId('release-sourcemaps-empty')).toBeTruthy();
	});
});

describe('/{orgSlug}/projects/{projectId}/releases the 0 row (contract §4.6, flow R3)', () => {
	it('files: [] renders the No sourcemaps sentence, not an empty box', async () => {
		const fetchMock = stubFetch(async () =>
			jsonResponse({ releaseVersion: UNMAPPED_VERSION, files: [] })
		);
		render(ReleasesPage, { data: pageData() });

		await fireEvent.click(toggleOf(UNMAPPED_VERSION));
		const empty = await screen.findByTestId('release-sourcemaps-empty');

		expect(text(empty)).toBe('No sourcemaps');
		expect(fetchMock).toHaveBeenCalledTimes(1);
		expect(screen.queryByTestId('release-sourcemaps-list')).toBeNull();
		expect(screen.queryByTestId('release-sourcemaps-error')).toBeNull();
		expect(expandedRegionOf(UNMAPPED_VERSION)?.contains(empty)).toBe(true);
	});
});

describe('/{orgSlug}/projects/{projectId}/releases the error line (fe.md task 2, R2-error)', () => {
	it('a rejected fetch renders the error line, no list and no sentence', async () => {
		stubFetch(async () => {
			throw new TypeError('Failed to fetch');
		});
		render(ReleasesPage, { data: pageData() });

		await fireEvent.click(toggleOf(MAPPED_VERSION));
		const line = await screen.findByTestId('release-sourcemaps-error');

		expect(text(line)).toBe('Could not load sourcemaps for this release');
		expect(screen.queryByTestId('release-sourcemaps-list')).toBeNull();
		expect(screen.queryByTestId('release-sourcemaps-empty')).toBeNull();
		expect(screen.queryByTestId('release-sourcemaps-loading')).toBeNull();
	});

	it('a non-2xx response renders the same error line', async () => {
		stubFetch(async () => jsonResponse({ message: 'Project not found' }, 404));
		render(ReleasesPage, { data: pageData() });

		await fireEvent.click(toggleOf(MAPPED_VERSION));
		const line = await screen.findByTestId('release-sourcemaps-error');
		expect(text(line)).toBe('Could not load sourcemaps for this release');
	});
});
