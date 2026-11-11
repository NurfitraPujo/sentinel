import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import { readFileSync } from 'node:fs';
import TagsTable from './TagsTable.svelte';
import { tagSearchHref } from '$lib/tag-filter';
import { FLUTTER_DETAIL, LONG_DETAIL } from './occurrence-fixtures';
import { clip, INLINE_LIMIT } from './occurrence-display';

// sentinel-occurrence-detail-panels §6.3 (TASK-03), contract decisions 6 and 7: each part is encoded
// before the join, the joined string is encoded again, and URLSearchParams never builds the href.

// The spec's pinned table, computed with Node 24 from the normative form.
const PINNED: [string, string, string][] = [
	['browser', 'Chrome 120', '/search?tag=browser%3AChrome%2520120'],
	['os.name', 'Android', '/search?tag=os.name%3AAndroid'],
	['release', '2.14.0+2140', '/search?tag=release%3A2.14.0%252B2140'],
	['url', '/a?b=1&c=2#x', '/search?tag=url%3A%252Fa%253Fb%253D1%2526c%253D2%2523x'],
	['k:v', 'x', '/search?tag=k%253Av%3Ax'],
	['a:b', 'c:d e/%', '/search?tag=a%253Ab%3Ac%253Ad%2520e%252F%2525'],
	['k', "it's (x)!~", "/search?tag=k%3Ait's%2520(x)!~"],
];

const codePoints = (s: string | null | undefined) => Array.from(s ?? '').length;

function rows() {
	return Array.from(screen.getByTestId('tags-table').querySelectorAll('tbody > tr')).map((tr) => ({
		key: tr.querySelector('th')!.textContent,
		href: tr.querySelector('a')!.getAttribute('href'),
		text: tr.querySelector('a')!.textContent,
	}));
}

describe('TagsTable', () => {
	it('TG1: rows sorted by key, each href exactly the pinned one', () => {
		render(TagsTable, { tags: Object.fromEntries(PINNED.map(([k, v]) => [k, v])) });
		const sorted = [...PINNED].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
		expect(rows()).toEqual(sorted.map(([key, value, href]) => ({ key, href, text: value })));
		expect(screen.getByTestId('tags-table').querySelector(':scope > tbody')).not.toBeNull();
		expect(screen.getAllByTestId('tag-link')).toHaveLength(7);
	});

	it('TG1: tagSearchHref returns the same seven strings', () => {
		for (const [key, value, href] of PINNED) {
			expect(tagSearchHref(key, value), `${key} / ${value}`).toBe(href);
		}
	});

	it('TG1: TagsTable imports tagSearchHref from $lib/tag-filter and defines no encoder of its own', () => {
		const source = readFileSync('src/lib/components/issues/TagsTable.svelte', 'utf8');
		expect(source).toMatch(/^\s*import \{ tagSearchHref \} from '\$lib\/tag-filter';$/m);
		expect(source).not.toMatch(/encodeURIComponent|URLSearchParams/);
	});

	it('the Flutter tags render with their title', () => {
		render(TagsTable, { tags: FLUTTER_DETAIL.tags });
		expect(screen.getByRole('heading', { level: 4 }).textContent).toBe('Tags');
		expect(rows().map((r) => r.key)).toEqual(Object.keys(FLUTTER_DETAIL.tags).sort());
	});

	it('only string values make rows', () => {
		render(TagsTable, { tags: { a: 'x', b: 3, c: null, d: { e: 1 } } });
		expect(rows().map((r) => r.key)).toEqual(['a']);
	});

	it.each([[null], [{}], [{ a: 1 }], ['env:prod'], [['a']]])('TG2: %j renders nothing', (tags) => {
		const { container } = render(TagsTable, { tags });
		expect(screen.queryByTestId('tags-table')).toBeNull();
		expect(container.querySelector('h4')).toBeNull();
	});

	it('TG3: the round trip of contract decision 6', () => {
		const href = tagSearchHref('a:b', 'c:d e/%');
		const raw = new URL(href, 'http://x').searchParams.get('tag')!;
		expect(raw.split(':')).toHaveLength(2);
		const [key, value] = raw.split(':').map(decodeURIComponent);
		expect([key, value]).toEqual(['a:b', 'c:d e/%']);
	});

	it('TR8: a long key collapses; a long value is clipped link text', () => {
		render(TagsTable, { tags: LONG_DETAIL.tags });
		const tr = screen.getByTestId('tags-table').querySelector('tbody > tr')!;
		const key = tr.querySelector('th .expandable-text')!.firstChild!.textContent!;
		expect(codePoints(key)).toBe(257);
		const [, value] = Object.entries(LONG_DETAIL.tags as Record<string, string>)[0];
		const link = tr.querySelector('a')!;
		expect(link.textContent).toBe(clip(value, INLINE_LIMIT));
		expect(codePoints(link.textContent)).toBe(257);
		expect(link.querySelector('button')).toBeNull();
		expect(screen.getAllByTestId('expand-text')).toHaveLength(1);
	});
});
