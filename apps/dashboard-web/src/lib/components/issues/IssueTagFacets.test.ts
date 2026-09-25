import { describe, it, expect } from 'vitest';
import { render, screen, within } from '@testing-library/svelte';
import IssueTagFacets from './IssueTagFacets.svelte';
import { buildTagFacets, type TagFacetRow } from '$lib/tag-facets';
import { tagSearchHref } from '$lib/tag-filter';

// sentinel-issue-tag-facets spec §7 (TASK-02): the issue page's tag distribution panel.

const row = (key: string, value: string, count: number, keyTotal: number, derivedOrd: number | null = null): TagFacetRow => ({
	key,
	value,
	count,
	keyTotal,
	derivedOrd,
});

const FIXTURE: TagFacetRow[] = [
	row('release', '1.0', 6, 10, 1),
	row('release', '1.1', 4, 10, 1),
	row('user.id', 'u-1', 2, 9, 7),
	row('user.id', '', 7, 9, 7),
	row('a:b', 'c:d e/%', 3, 3),
	row('tenant', '<script>alert(1)</script>', 1, 1),
];

describe('IssueTagFacets', () => {
	it('renders nothing when there are no blocks (F6)', () => {
		const { container } = render(IssueTagFacets, { facets: [] });
		expect(container.querySelector('[data-testid="issue-tag-facets"]')).toBeNull();
		expect(container.textContent?.trim()).toBe('');
	});

	it('renders the blocks in order, each value with its count and percentage, and Other without a link', () => {
		render(IssueTagFacets, { facets: buildTagFacets(FIXTURE) });
		const panel = screen.getByTestId('issue-tag-facets');
		const blocks = within(panel).getAllByTestId('tag-facet-block');
		expect(blocks.map((b) => b.getAttribute('data-key'))).toEqual(['release', 'user.id', 'a:b', 'tenant']);

		const release = within(blocks[0]).getAllByTestId('tag-facet-value');
		expect(release).toHaveLength(2);
		expect(release[0].textContent).toContain('1.0');
		expect(release[0].textContent).toContain('6');
		expect(release[0].textContent).toContain('60.0%');
		expect(release[1].textContent).toContain('40.0%');
		expect(within(blocks[0]).queryByTestId('tag-facet-other')).toBeNull();

		const other = within(blocks[1]).getByTestId('tag-facet-other');
		expect(other.textContent).toContain('Other');
		expect(other.textContent).toContain('7');
		expect(other.textContent).toContain('77.8%');
		expect(other.querySelector('a')).toBeNull();
		expect(other.closest('a')).toBeNull();
	});

	it("each value links to tagSearchHref(key, value), the ':' key included", () => {
		render(IssueTagFacets, { facets: buildTagFacets(FIXTURE) });
		const links = screen.getAllByTestId('tag-facet-value').map((li) => li.querySelector('a'));
		const hrefs = links.map((a) => a?.getAttribute('href'));
		expect(hrefs).toContain(tagSearchHref('release', '1.0'));
		expect(hrefs).toContain(tagSearchHref('user.id', 'u-1'));
		expect(hrefs).toContain(tagSearchHref('a:b', 'c:d e/%'));
		expect(hrefs).toContain('/search?tag=a%253Ab%3Ac%253Ad%2520e%252F%2525');
	});

	it('a caller-supplied value renders as text, never markup', () => {
		const { container } = render(IssueTagFacets, { facets: buildTagFacets(FIXTURE) });
		expect(container.querySelector('script')).toBeNull();
		expect(screen.getByText('<script>alert(1)</script>')).toBeTruthy();
	});

	it('a long value carries its full text in title', () => {
		const long = 'x'.repeat(300);
		render(IssueTagFacets, { facets: buildTagFacets([row('k', long, 1, 1)]) });
		expect(screen.getByTitle(long)).toBeTruthy();
	});
});
