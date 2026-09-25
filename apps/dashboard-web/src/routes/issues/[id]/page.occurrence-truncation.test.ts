import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render } from '@testing-library/svelte';
import Page from './+page.svelte';
import {
	pageData,
	LONG_OCCURRENCE,
	LONG_DETAIL,
	LONG_EXPANDERS,
} from '$lib/components/issues/occurrence-fixtures';
import { BLOCK_LIMIT, INLINE_LIMIT } from '$lib/components/issues/occurrence-display';

// sentinel-occurrence-detail-panels §4.3 TR1: every event string the selected card renders goes through
// ExpandableText (or clip, where a button cannot go). LONG_* overfills all 38 inventory fields; the
// selected card must show exactly one expander per rendered inventory instance, counted per panel so a
// miss names its panel.

const codePoints = (s: string | null | undefined) => Array.from(s ?? '').length;
const expanders = (el: Element | null) => (el ? el.querySelectorAll('[data-testid="expand-text"]').length : 0);

beforeEach(() => {
	vi.restoreAllMocks();
	vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({}) }));
});

describe('TR1: the truncation inventory on the selected card', () => {
	it(`shows exactly ${LONG_EXPANDERS.total} expanders, each panel its own count`, () => {
		const { container } = render(Page, {
			data: pageData({ occurrences: [LONG_OCCURRENCE], occurrenceDetail: { ...LONG_OCCURRENCE, ...LONG_DETAIL } }),
		});
		const card = container.querySelector('.occurrence-card')!;
		const q = (sel: string) => card.querySelector(sel);

		const perPanel = {
			OccurrenceHeader: expanders(q('[data-testid="occurrence-message"]')) + expanders(q('[data-testid="occurrence-facts"]')),
			StackFrames: expanders(q('.stacktrace [data-testid="frames"]')),
			TagsTable: expanders(q('[data-testid="tags-table"]')),
			ExceptionChain: expanders(q('[data-testid="exception-chain"]')),
			BreadcrumbTimeline: expanders(q('[data-testid="breadcrumbs"]')),
			RequestPanel: expanders(q('[data-testid="request-panel"]')),
			UserPanel: expanders(q('[data-testid="user-panel"]')),
			ContextCards: expanders(q('[data-testid="contexts"]')),
		};
		const { total, ...expected } = LONG_EXPANDERS;
		expect(perPanel).toEqual(expected);
		expect(expanders(card)).toBe(total);
	});

	it('bounds every text node, every filter option and the tag link', () => {
		const { container } = render(Page, {
			data: pageData({ occurrences: [LONG_OCCURRENCE], occurrenceDetail: { ...LONG_OCCURRENCE, ...LONG_DETAIL } }),
		});
		const card = container.querySelector('.occurrence-card')!;

		const walker = document.createTreeWalker(card, NodeFilter.SHOW_TEXT);
		let longest = 0;
		for (let node = walker.nextNode(); node; node = walker.nextNode()) {
			longest = Math.max(longest, codePoints(node.textContent));
		}
		expect(longest).toBeLessThanOrEqual(BLOCK_LIMIT + 1);

		const options = Array.from(card.querySelectorAll('[data-testid="crumb-category-filter"] option, [data-testid="crumb-level-filter"] option'));
		expect(options.length).toBeGreaterThan(2);
		for (const option of options) expect(codePoints(option.textContent)).toBeLessThanOrEqual(INLINE_LIMIT + 1);

		const link = card.querySelector('[data-testid="tag-link"]')!;
		expect(codePoints(link.textContent)).toBe(INLINE_LIMIT + 1);
		expect(link.textContent!.endsWith('…')).toBe(true);
	});
});
