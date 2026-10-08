import { describe, it, expect } from 'vitest';
import { screen } from '@testing-library/svelte';
import { render } from './page-fixtures';
import Page from './+page.svelte';
import type { PageData } from './$types';

// sentinel-issue-list-filters §5 item 8 — the page half: what the strip renders and what every
// anchor carries. The loader's coercion is page.server.test.ts's; the SQL is the integration suite's.
function pageData(filters: Record<string, unknown> = {}, overrides: Record<string, unknown> = {}) {
	return {
		orgSlug: 'acme',
		issues: [],
		projects: [{ id: 'proj-1', name: 'Checkout API' }],
		environments: [],
		releases: [{ version: '2.0.0' }, { version: '1.9.0' }],
		sources: ['ingestion_sdk', 'manual_support', 'api', 'user_report_api'],
		filters: { status: 'all', project: 'proj-1', environment: 'all', ...filters },
		pagination: { page: 3, limit: 20, total: 0, totalPages: 0 },
		...overrides,
	} as unknown as PageData;
}

function hrefOf(testId: string): string {
	return (screen.getByTestId(testId) as HTMLAnchorElement).getAttribute('href')!;
}

describe('/ list filters', () => {
	it('shows the release dropdown with options when a project is selected', () => {
		render(Page, { data: pageData() });
		const select = screen.getByTestId('release-filter') as HTMLSelectElement;
		expect(Array.from(select.options).map((o) => o.value)).toEqual(['all', '2.0.0', '1.9.0']);
	});

	it('omits the release dropdown on project=all', () => {
		render(Page, { data: pageData({ project: 'all' }, { releases: [] }) });
		expect(screen.queryByTestId('release-filter')).toBeNull();
		expect(screen.getByTestId('type-filter')).toBeTruthy();
	});

	it('still shows a URL release outside the offered options as selected', () => {
		render(Page, { data: pageData({ release: '0.1.0-old' }) });
		const select = screen.getByTestId('release-filter') as HTMLSelectElement;
		expect(select.value).toBe('0.1.0-old');
	});

	it('carries every new param on the anchors, and resets page to 1', () => {
		render(Page, {
			data: pageData({
				release: '2.0.0',
				type: 'user_report',
				source: 'api',
				regressed: true,
				waiting: 'team',
				tags: [{ key: 'browser.name', value: 'Fire fox', param: 'browser.name:Fire%20fox' }],
			}),
		});
		const next = new URLSearchParams(hrefOf('regressed-chip'));
		// the Regressed chip toggles it off; every other param is carried
		expect(next.get('regressed')).toBeNull();
		expect(next.get('release')).toBe('2.0.0');
		expect(next.get('type')).toBe('user_report');
		expect(next.get('source')).toBe('api');
		expect(next.get('waiting')).toBe('team');
		expect(next.get('page')).toBeNull();
		// the tag is re-emitted as its `param`, encoded once more as a query value
		expect(hrefOf('regressed-chip')).toContain('&tag=browser.name%3AFire%2520fox');
		expect(next.getAll('tag')).toEqual(['browser.name:Fire%20fox']);
	});

	it('Reset all filters clears every new filter', () => {
		render(Page, {
			data: pageData({
				release: '2.0.0',
				type: 'user_report',
				source: 'api',
				regressed: true,
				waiting: 'any',
				tags: [{ key: 'os.name', value: 'Linux', param: 'os.name:Linux' }],
			}),
		});
		const reset = new URLSearchParams(hrefOf('reset-filters'));
		for (const k of ['release', 'type', 'source', 'regressed', 'waiting', 'tag']) {
			expect(reset.has(k)).toBe(false);
		}
		expect(reset.get('project')).toBe('proj-1');
	});

	it('a tag chip appears and its × removes only that tag', () => {
		render(Page, {
			data: pageData({
				tags: [
					{ key: 'os.name', value: 'Linux', param: 'os.name:Linux' },
					{ key: 'user.id', value: '42', param: 'user.id:42' },
				],
			}),
		});
		const chips = screen.getAllByTestId('tag-chip');
		expect(chips).toHaveLength(2);
		const removes = screen.getAllByTestId('tag-chip-remove');
		const q = new URLSearchParams(removes[0].getAttribute('href')!);
		expect(q.getAll('tag')).toEqual(['user.id:42']);
	});
});
