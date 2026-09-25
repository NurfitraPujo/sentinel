import { describe, it, expect } from 'vitest';
import { render, screen, cleanup } from '@testing-library/svelte';
import { afterEach } from 'vitest';
import Page from './+page.svelte';

// sentinel-issue-tag-facets spec §6.7 (tfr-search-page-behaviour-unproven): the page-level tag
// behaviours removing which left every existing test green — the paging links carrying `tag`
// params, the hidden `tag` inputs on the filter form, and no {@html} for a tag's key/value.

afterEach(() => cleanup());

function makePageData(overrides: Record<string, unknown> = {}) {
	return {
		results: [],
		environments: [],
		projects: [],
		filters: {
			userId: undefined,
			tenantId: undefined,
			traceId: undefined,
			spanId: undefined,
			requestId: undefined,
			projectId: undefined,
			environment: undefined,
			platform: undefined,
			search: undefined,
			tags: [],
		},
		ignoredTagTerms: [],
		pagination: { page: 1, limit: 50, total: 0, totalPages: 1 },
		...overrides,
		// eslint-disable-next-line @typescript-eslint/no-explicit-any
	} as any;
}

const activeTag = (key: string, value: string, param: string, removeHref: string) => ({
	key,
	value,
	param,
	removeHref,
});

describe('search page — active tag chips and links (§6.7)', () => {
	it('carries every active tag on the pagination links, not just the other filters', () => {
		render(Page, {
			data: makePageData({
				filters: {
					tags: [activeTag('release', '1.0', 'release:1.0', '/search'), activeTag('os.name', 'linux', 'os.name:linux', '/search')],
				},
				pagination: { page: 1, limit: 50, total: 200, totalPages: 3 },
			}),
		});
		const next = screen.getByText('Next').closest('a') as HTMLAnchorElement;
		const url = new URL(next.getAttribute('href')!, 'http://x');
		expect(url.searchParams.getAll('tag')).toEqual(['release:1.0', 'os.name:linux']);
	});

	it('carries a hidden tag input for every active tag, so resubmitting the form keeps them', () => {
		const { container } = render(Page, {
			data: makePageData({
				filters: { tags: [activeTag('release', '1.0', 'release:1.0', '/search'), activeTag('user.id', 'u-1', 'user.id:u-1', '/search')] },
			}),
		});
		const hidden = Array.from(container.querySelectorAll('input[type="hidden"][name="tag"]')) as HTMLInputElement[];
		expect(hidden.map((i) => i.value)).toEqual(['release:1.0', 'user.id:u-1']);
	});

	it('renders a tag chip for every active tag, plain text only — never {@html}', () => {
		const poison = '<img src=x onerror=alert(1)>';
		render(Page, {
			data: makePageData({
				filters: { tags: [activeTag('note', poison, 'note:poison', '/search')] },
			}),
		});
		// The chip's own text node carries the raw string; {@html} would instead have parsed it into
		// a real <img> element (jsdom executes onerror handlers it parses).
		const chip = screen.getByTestId('search-tag-chip');
		expect(chip.querySelector('img')).toBeNull();
		expect(chip.textContent).toContain(poison);
	});

	it('renders ignoredTagTerms as plain text, never {@html} (tfr2-search-html-test-decoration)', () => {
		const poison = '<img src=x onerror=alert(1)>:v';
		render(Page, { data: makePageData({ ignoredTagTerms: [poison] }) });
		// {@html} would parse this into a real <img> element (jsdom executes onerror handlers it parses).
		const el = screen.getByTestId('search-ignored-terms');
		expect(el.querySelector('img')).toBeNull();
		expect(el.textContent).toContain(poison);
	});

	it('renders one chip per active tag and none when there are no active tags', async () => {
		const { rerender } = render(Page, { data: makePageData() });
		expect(screen.queryAllByTestId('search-tag-chip')).toHaveLength(0);

		await rerender({
			data: makePageData({
				filters: { tags: [activeTag('a', '1', 'a:1', '/search'), activeTag('b', '2', 'b:2', '/search')] },
			}),
		});
		expect(screen.queryAllByTestId('search-tag-chip')).toHaveLength(2);
	});
});
