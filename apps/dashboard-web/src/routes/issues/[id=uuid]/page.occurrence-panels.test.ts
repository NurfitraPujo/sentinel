import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, fireEvent, screen, within } from '@testing-library/svelte';
import Page from './+page.svelte';
import {
	pageData,
	flutterDetailRow,
	FLUTTER_OCCURRENCE,
	V2_BARE_OCCURRENCE,
	V2_BARE_DETAIL,
	V2_JS_OCCURRENCE,
	V2_JS_DETAIL,
	OCCURRED_AT,
	loaded,
} from '$lib/components/issues/occurrence-fixtures';

// sentinel-occurrence-detail-panels: the occurrence panels on the issue page (§3.2 selection, §4.5
// placement, TASK-04 frames on the page).

const PANEL_TITLES = ['Tags', 'Exception Chain', 'Breadcrumbs', 'Request', 'User', 'Contexts'];
const PANEL_TEST_IDS = ['tags-table', 'chain-value', 'breadcrumbs', 'request-panel', 'user-panel', 'context-card'];

function cards(container: HTMLElement) {
	return Array.from(container.querySelectorAll<HTMLElement>('.occurrence-card'));
}

function panelHeadings(card: HTMLElement) {
	return Array.from(card.querySelectorAll('h4'))
		.map((h) => h.textContent?.trim())
		.filter((t) => PANEL_TITLES.includes(t ?? ''));
}

function panelTestIds(card: HTMLElement) {
	return PANEL_TEST_IDS.filter((id) => card.querySelector(`[data-testid="${id}"]`) !== null);
}

const lineText = (frame: Element) => frame.querySelector('.frame-line .expandable-text')!.firstChild!.textContent;

beforeEach(() => {
	vi.restoreAllMocks();
	vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({}) }));
});

describe('F6: a v2 JavaScript card keeps the badge and raw toggle on the new frame renderer', () => {
	it('swaps frames 0 and 1 to their raw forms and back; frame 2 and the in-app marks are unchanged', async () => {
		const { container } = render(Page, {
			data: pageData({ occurrences: [V2_JS_OCCURRENCE], occurrenceDetail: V2_JS_DETAIL }),
		});
		const card = cards(container)[0];
		const stack = card.querySelector('.stacktrace')!;
		expect(stack.querySelector('pre')).toBeNull();
		expect(within(stack as HTMLElement).getByTestId('frames')).toBeTruthy();
		expect(within(stack as HTMLElement).getByTestId('symbolication-badge').textContent).toBe(
			'Partially symbolicated (2 of 3)',
		);
		const toggle = within(stack as HTMLElement).getByTestId('raw-frames-toggle');

		const frames = () => within(stack as HTMLElement).getAllByTestId('frame');
		const marks = () => frames().map((f) => f.getAttribute('data-in-app'));
		const symbolicated = [
			'at submitOrder (src/cart/checkout.ts:42:7)',
			'at postJson (src/cart/api.ts:17:3)',
			'at r (https://cdn.example.com/assets/vendor.min.js:2:1044)',
		];
		expect(frames().map(lineText)).toEqual(symbolicated);
		const marksBefore = marks();

		await fireEvent.click(toggle);
		expect(frames().map(lineText)).toEqual([
			'at n (https://cdn.example.com/assets/app.3f9a1c.min.js:1:18234)',
			'at t (https://cdn.example.com/assets/app.3f9a1c.min.js:1:9021)',
			'at r (https://cdn.example.com/assets/vendor.min.js:2:1044)',
		]);
		expect(marks()).toEqual(marksBefore);

		await fireEvent.click(toggle);
		expect(frames().map(lineText)).toEqual(symbolicated);
	});
});

describe('X4: the chain on the page', () => {
	it('the selected Flutter card keeps its top-level Stack Trace block, with the chain section below it', () => {
		const { container } = render(Page, {
			data: pageData({ occurrences: [FLUTTER_OCCURRENCE], occurrenceDetail: flutterDetailRow() }),
		});
		const card = cards(container)[0];
		const stack = card.querySelector('.stacktrace')!;
		expect(stack.querySelector('h4')!.textContent).toBe('Stack Trace');
		expect(within(stack as HTMLElement).getByTestId('frames')).toBeTruthy();
		const chain = within(card).getByTestId('exception-chain');
		expect(stack.compareDocumentPosition(chain) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
		expect(within(chain).getAllByTestId('chain-value')).toHaveLength(3);
		expect(stack.contains(chain)).toBe(false);
	});
});

describe('selection on the page (§3.2)', () => {
	it('P2: a non-selected v2 card links to its own selection and shows no panel; the selected one is marked', () => {
		const { container } = render(Page, {
			data: pageData({
				occurrences: [FLUTTER_OCCURRENCE, V2_BARE_OCCURRENCE],
				occurrenceDetail: flutterDetailRow(),
			}),
		});
		const [selected, other] = cards(container);
		expect(selected.getAttribute('aria-current')).toBe('true');
		for (const id of ['tags-table', 'request-panel', 'user-panel']) {
			expect(within(selected).getByTestId(id), id).toBeTruthy();
		}
		expect(within(selected).getByTestId('selected-occurrence').textContent).toBe('Event details below');
		expect(within(selected).queryByTestId('select-occurrence')).toBeNull();

		const link = within(other).getByTestId('select-occurrence');
		expect(link.tagName).toBe('A');
		expect(link.getAttribute('href')).toBe(`?occurrence=${V2_BARE_OCCURRENCE.id}`);
		expect(link.textContent).toBe('Show event details');
		expect(link.hasAttribute('data-sveltekit-noscroll')).toBe(true);
		expect(other.hasAttribute('aria-current')).toBe(false);
		expect(within(other).queryByTestId('selected-occurrence')).toBeNull();
		expect(panelHeadings(other)).toEqual([]);
		expect(panelTestIds(other)).toEqual([]);
		// every v2 card gets its header facts, selected or not
		expect(within(other).getByTestId('occurrence-facts')).toBeTruthy();
	});

	it('P3: a bare v2 occurrence selected renders its message and facts, and no panel at all', () => {
		const { container } = render(Page, {
			data: pageData({ occurrences: [V2_BARE_OCCURRENCE], occurrenceDetail: V2_BARE_DETAIL }),
		});
		const card = cards(container)[0];
		expect(within(card).getByTestId('occurrence-message').textContent).toBe('Issue-level message');
		expect(within(card).getByTestId('occurrence-facts').textContent).toContain('Level');
		expect(panelHeadings(card)).toEqual([]);
		expect(panelTestIds(card)).toEqual([]);
		expect(card.querySelectorAll('section')).toHaveLength(0);
	});

	it('P7: an S3a selection renders in its own block, after the heading and before the first date group', () => {
		const third = flutterDetailRow('0b7e3f2a-5c1d-4e8f-9a6b-000000000003');
		const { container } = render(Page, {
			data: pageData({
				occurrences: [V2_BARE_OCCURRENCE, V2_JS_OCCURRENCE],
				selectedOccurrenceId: third.id,
				occurrenceDetail: third,
			}),
		});
		const block = screen.getByTestId('selected-outside-window');
		const section = block.closest('section.occurrences')!;
		const heading = section.querySelector('h2')!;
		const firstGroup = section.querySelector('.date-group')!;
		expect(heading.compareDocumentPosition(block) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
		expect(block.compareDocumentPosition(firstGroup) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
		expect(block.querySelector('p.occurrence-window')!.textContent).toBe(
			'This event is older than the 2 most recent shown below.',
		);

		const card = block.querySelector<HTMLElement>('.occurrence-card')!;
		expect(card.getAttribute('aria-current')).toBe('true');
		expect(within(card).getByTestId('selected-occurrence')).toBeTruthy();
		expect(within(card).getByTestId('occurrence-message').textContent).toBe(FLUTTER_OCCURRENCE.message);
		for (const id of ['tags-table', 'request-panel', 'user-panel']) {
			expect(within(card).getByTestId(id), id).toBeTruthy();
		}

		const listed = Array.from(section.querySelectorAll<HTMLElement>('.date-group .occurrence-card'));
		expect(listed).toHaveLength(2);
		expect(listed.every((c) => !c.hasAttribute('aria-current'))).toBe(true);
		expect(listed.every((c) => within(c).queryByTestId('select-occurrence') !== null)).toBe(true);
	});

	it('P8: a listed selection, or no detail, renders no outside-window block', () => {
		const { unmount } = render(Page, {
			data: pageData({ occurrences: [FLUTTER_OCCURRENCE], occurrenceDetail: flutterDetailRow() }),
		});
		expect(screen.queryByTestId('selected-outside-window')).toBeNull();
		unmount();

		render(Page, { data: pageData({ occurrences: [FLUTTER_OCCURRENCE], occurrenceDetail: null }) });
		expect(screen.queryByTestId('selected-outside-window')).toBeNull();
	});
});

describe('P1: the contract §1 hypothesis, in the shape the real load returns', () => {
	it('every panel on the selected Flutter card, in §4.5 order, with createdAt a Date', () => {
		const occurrence = loaded(FLUTTER_OCCURRENCE);
		const detail = loaded(flutterDetailRow());
		expect(detail.createdAt).toBeInstanceOf(Date);
		const { container } = render(Page, { data: pageData({ occurrences: [occurrence], occurrenceDetail: detail }) });
		const card = cards(container)[0];

		const order = [
			'occurrence-message', 'occurrence-facts', 'tags-table', 'frames', 'exception-chain',
			'breadcrumbs', 'request-panel', 'user-panel', 'context-card',
		].map((id) => {
			const el = card.querySelector(`[data-testid="${id}"]`);
			expect(el, id).not.toBeNull();
			return el!;
		});
		for (let i = 1; i < order.length; i++) {
			expect(order[i - 1].compareDocumentPosition(order[i]) & Node.DOCUMENT_POSITION_FOLLOWING, `${i}`).toBeTruthy();
		}

		const expectedDate = new Date(OCCURRED_AT).toLocaleString('en-US', {
			year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit',
		});
		const timestamp = card.querySelector('.timestamp')!.textContent;
		expect(timestamp).toBe(expectedDate);
		expect(timestamp).not.toBe('Unknown');
		expect(timestamp).not.toBe('Invalid Date');

		const crumbs = within(within(card).getByTestId('breadcrumbs')).getAllByTestId('crumb');
		expect(within(crumbs[96]).getByTestId('crumb-time').textContent).toBe('-3.6s');
		expect(within(crumbs[99]).getByTestId('crumb-time').textContent).toBe('0.0s');
	});
});

describe('P9: panel state never bleeds from one occurrence to another (review odp-rv1-panel-state-bleeds-across-occurrences)', () => {
	// Two ids for the listed pair plus a newer one, N, ingested between the two navigations: the list
	// shifts by one, so the newly selected B lands on the card index A's panels occupied.
	const A = '0b7e3f2a-5c1d-4e8f-9a6b-00000000000a';
	const B = '0b7e3f2a-5c1d-4e8f-9a6b-00000000000b';
	const N = '0b7e3f2a-5c1d-4e8f-9a6b-00000000000c';
	const crumbs = (category: string) =>
		Array.from({ length: 5 }, (_, i) => ({
			timestamp: new Date(Date.parse(OCCURRED_AT) - (5 - i) * 1000).toISOString(),
			category,
			level: 'info',
			message: `${category} ${i}`,
		}));
	const row = (id: string) => ({ ...FLUTTER_OCCURRENCE, id });
	const detail = (id: string, category: string) => ({ ...flutterDetailRow(id), breadcrumbs: crumbs(category) });

	const timeline = (container: HTMLElement) => {
		const card = container.querySelector<HTMLElement>('.occurrence-card[aria-current="true"]')!;
		return within(card).getByTestId('breadcrumbs');
	};

	it('a listed selection that lands on a reused card index starts with every crumb shown', async () => {
		const { container, rerender } = render(Page, {
			data: pageData({ occurrences: [row(B), row(A)], selectedOccurrenceId: A, occurrenceDetail: detail(A, 'http') }),
		});
		const select = within(timeline(container)).getByTestId('crumb-category-filter') as HTMLSelectElement;
		select.value = 'http';
		await fireEvent.change(select);
		expect(within(timeline(container)).getByTestId('crumb-count').textContent).toBe('Showing 5 of 5');

		await rerender({
			data: pageData({
				occurrences: [row(N), row(B), row(A)],
				selectedOccurrenceId: B,
				occurrenceDetail: detail(B, 'navigation'),
			}),
		});
		const shown = timeline(container);
		expect(within(shown).queryByTestId('crumb-count')).toBeNull();
		expect(within(shown).getAllByTestId('crumb')).toHaveLength(5);
		expect((within(shown).getByTestId('crumb-category-filter') as HTMLSelectElement).value).toBe('');
	});

	it('an outside-window selection replaced by another one starts with every crumb shown', async () => {
		const listed = [V2_BARE_OCCURRENCE];
		const { container, rerender } = render(Page, {
			data: pageData({ occurrences: listed, selectedOccurrenceId: A, occurrenceDetail: detail(A, 'http') }),
		});
		const select = within(timeline(container)).getByTestId('crumb-category-filter') as HTMLSelectElement;
		select.value = 'http';
		await fireEvent.change(select);
		expect(within(timeline(container)).getByTestId('crumb-count').textContent).toBe('Showing 5 of 5');

		await rerender({
			data: pageData({ occurrences: listed, selectedOccurrenceId: B, occurrenceDetail: detail(B, 'navigation') }),
		});
		const shown = timeline(container);
		expect(within(shown).queryByTestId('crumb-count')).toBeNull();
		expect(within(shown).getAllByTestId('crumb')).toHaveLength(5);
	});
});
