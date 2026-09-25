import { describe, it, expect } from 'vitest';
import { render, fireEvent, screen, within } from '@testing-library/svelte';
import BreadcrumbTimeline from './BreadcrumbTimeline.svelte';
import { FLUTTER_DETAIL, LONG_DETAIL, OCCURRED_AT } from './occurrence-fixtures';

// sentinel-occurrence-detail-panels §8 (TASK-02): the breadcrumbs timeline, in stored order, with
// relative time anchored on the occurrence, category/level filters and deferred `data`.

const CRUMBS = FLUTTER_DETAIL.breadcrumbs as Record<string, any>[];
const codePoints = (s: string | null | undefined) => Array.from(s ?? '').length;
const crumbs = () => screen.queryAllByTestId('crumb');
const timeOf = (i: number) => within(crumbs()[i]).getByTestId('crumb-time');

async function choose(testId: string, value: string) {
	const select = screen.getByTestId(testId) as HTMLSelectElement;
	select.value = value;
	await fireEvent.change(select);
}

function assertNoBadTime(container: HTMLElement) {
	expect(container.textContent).not.toContain('NaN');
	expect(container.textContent).not.toContain('Invalid Date');
}

describe('BreadcrumbTimeline', () => {
	it('B1: 100 crumbs in stored order, and no data rendered before an expand', () => {
		render(BreadcrumbTimeline, { breadcrumbs: CRUMBS, occurredAt: OCCURRED_AT });
		expect(screen.getByRole('heading', { level: 4 }).textContent).toBe('Breadcrumbs');
		expect(crumbs()).toHaveLength(100);
		expect(crumbs()[0].querySelector('.crumb-message')!.textContent).toBe(CRUMBS[0].message);
		expect(crumbs()[99].querySelector('.crumb-message')!.textContent).toBe(CRUMBS[99].message);
		expect(screen.queryAllByTestId('crumb-data')).toHaveLength(0);
		expect(screen.getAllByTestId('crumb-data-toggle')).toHaveLength(75);
		expect(screen.queryByTestId('crumb-count')).toBeNull();
	});

	it('B2: filters narrow by level, then by category AND level, and restore', async () => {
		render(BreadcrumbTimeline, { breadcrumbs: CRUMBS, occurredAt: OCCURRED_AT });
		const levelOptions = Array.from(screen.getByTestId('crumb-level-filter').querySelectorAll('option')).map(
			(o) => o.textContent,
		);
		expect(levelOptions).toEqual(['All levels', 'error', 'info', 'debug']);
		const categoryOptions = Array.from(screen.getByTestId('crumb-category-filter').querySelectorAll('option')).map(
			(o) => o.textContent,
		);
		expect(categoryOptions).toEqual(['All categories', 'console', 'http', 'navigation', 'ui.click']);

		await choose('crumb-level-filter', 'error');
		expect(crumbs()).toHaveLength(13);
		expect(screen.getByTestId('crumb-count').textContent).toBe('Showing 13 of 100');
		await choose('crumb-category-filter', 'http');
		expect(crumbs()).toHaveLength(13);
		expect(crumbs().every((c) => c.querySelector('.crumb-category')!.textContent === 'http')).toBe(true);
		await choose('crumb-level-filter', '');
		expect(crumbs()).toHaveLength(25);
		expect(screen.getByTestId('crumb-count').textContent).toBe('Showing 25 of 100');
		await choose('crumb-category-filter', '');
		expect(crumbs()).toHaveLength(100);
		expect(screen.queryByTestId('crumb-count')).toBeNull();
	});

	it('B2: crumb levels carry their tone; a crumb with no level has no level element', () => {
		const { unmount } = render(BreadcrumbTimeline, { breadcrumbs: CRUMBS, occurredAt: OCCURRED_AT });
		const level = (i: number) => crumbs()[i].querySelector('.crumb-level')!;
		expect(level(1).classList.contains('tone-critical')).toBe(true);
		expect(level(3).classList.contains('tone-debug')).toBe(true);
		expect(level(0).classList.contains('tone-info')).toBe(true);
		unmount();

		render(BreadcrumbTimeline, {
			breadcrumbs: [{ category: 'x', message: 'no level', timestamp: OCCURRED_AT }],
			occurredAt: OCCURRED_AT,
		});
		expect(crumbs()[0].querySelector('.crumb-level')).toBeNull();
	});

	it('B3: a data toggle renders the pretty JSON and relabels the button', async () => {
		render(BreadcrumbTimeline, { breadcrumbs: CRUMBS, occurredAt: OCCURRED_AT });
		const crumb = crumbs()[1];
		const toggle = within(crumb).getByTestId('crumb-data-toggle');
		expect(toggle.textContent).toBe('Show data');
		expect(toggle.getAttribute('aria-expanded')).toBe('false');
		await fireEvent.click(toggle);
		const data = within(crumb).getByTestId('crumb-data');
		expect(data.querySelector('pre')!.textContent).toBe(JSON.stringify(CRUMBS[1].data, null, 2));
		expect(toggle.textContent).toBe('Hide data');
		expect(toggle.getAttribute('aria-expanded')).toBe('true');
		expect(screen.getAllByTestId('crumb-data')).toHaveLength(1);
		expect(data.querySelector('a')).toBeNull();
	});

	it('B4: relative to the occurrence; an unparseable timestamp reads raw; an absent one reads —', () => {
		const first = render(BreadcrumbTimeline, { breadcrumbs: CRUMBS, occurredAt: OCCURRED_AT });
		expect(timeOf(96).textContent).toBe('-3.6s');
		expect(timeOf(96).getAttribute('title')).toBe(CRUMBS[96].timestamp);
		expect(timeOf(96).getAttribute('datetime')).toBe(CRUMBS[96].timestamp);
		expect(timeOf(99).textContent).toBe('0.0s');
		assertNoBadTime(first.container);
		first.unmount();

		const breadcrumbs = [{ message: 'raw', timestamp: 'yesterday-ish' }, { message: 'none' }, { message: 'e', timestamp: '' }];
		const { container } = render(BreadcrumbTimeline, { breadcrumbs, occurredAt: OCCURRED_AT });
		expect(timeOf(0).textContent).toBe('yesterday-ish');
		expect(timeOf(0).hasAttribute('title')).toBe(false);
		expect(timeOf(0).hasAttribute('datetime')).toBe(false);
		expect(timeOf(1).textContent).toBe('—');
		expect(timeOf(2).textContent).toBe('—');
		assertNoBadTime(container);
	});

	it('B4b: the production shape, occurredAt a Date', () => {
		const { container, unmount } = render(BreadcrumbTimeline, {
			breadcrumbs: CRUMBS,
			occurredAt: new Date(OCCURRED_AT),
		});
		expect(timeOf(96).textContent).toBe('-3.6s');
		expect(timeOf(96).getAttribute('title')).toBe(CRUMBS[96].timestamp);
		expect(timeOf(99).textContent).toBe('0.0s');
		assertNoBadTime(container);
		unmount();

		for (const occurredAt of [new Date('nope'), null]) {
			const r = render(BreadcrumbTimeline, { breadcrumbs: CRUMBS, occurredAt });
			expect(timeOf(96).textContent).toBe(CRUMBS[96].timestamp);
			expect(timeOf(96).hasAttribute('title')).toBe(false);
			expect(timeOf(96).getAttribute('datetime')).toBe(CRUMBS[96].timestamp);
			assertNoBadTime(r.container);
			r.unmount();
		}
	});

	it('B6: more than 100 crumbs renders the last 100 and a guard line', () => {
		const many = Array.from({ length: 150 }, (_, i) => ({ message: `m${i}`, timestamp: OCCURRED_AT }));
		render(BreadcrumbTimeline, { breadcrumbs: many, occurredAt: OCCURRED_AT });
		expect(crumbs()).toHaveLength(100);
		expect(crumbs()[0].querySelector('.crumb-message')!.textContent).toBe('m50');
		expect(crumbs()[99].querySelector('.crumb-message')!.textContent).toBe('m149');
		expect(screen.getByTestId('crumb-guard').textContent).toBe('Showing the 100 most recent of 150 breadcrumbs.');
	});

	it.each([[null], [[]], [[1, 'x']], ['crumbs'], [{ 0: {} }]])('B7: %j renders nothing', (breadcrumbs) => {
		const { container } = render(BreadcrumbTimeline, { breadcrumbs, occurredAt: OCCURRED_AT });
		expect(screen.queryByTestId('breadcrumbs')).toBeNull();
		expect(container.querySelector('h4')).toBeNull();
	});

	it('non-object crumbs are skipped among objects', () => {
		render(BreadcrumbTimeline, { breadcrumbs: [null, { message: 'a' }, 'x', { message: 'b' }], occurredAt: null });
		expect(crumbs().map((c) => c.querySelector('.crumb-message')!.textContent)).toEqual(['a', 'b']);
	});

	it('TR5: every long crumb field collapses; its data collapses once opened', async () => {
		render(BreadcrumbTimeline, { breadcrumbs: LONG_DETAIL.breadcrumbs, occurredAt: OCCURRED_AT });
		const crumb = crumbs()[0];
		for (const cls of ['crumb-time', 'crumb-level', 'crumb-category', 'crumb-type', 'crumb-message']) {
			const el = crumb.querySelector(`.${cls}`)!;
			const text = el.querySelector('.expandable-text')!.firstChild!.textContent!;
			expect(codePoints(text), cls).toBe(257);
			expect(el.querySelector('[data-testid="expand-text"]'), cls).not.toBeNull();
		}
		expect(within(crumb).getAllByTestId('expand-text')).toHaveLength(5);
		await fireEvent.click(within(crumb).getByTestId('crumb-data-toggle'));
		const pre = within(crumb).getByTestId('crumb-data').querySelector('pre')!;
		expect(codePoints(pre.textContent)).toBe(2049);
		for (const option of Array.from(document.querySelectorAll('option'))) {
			expect(codePoints(option.textContent)).toBeLessThanOrEqual(257);
		}
	});

	it('level options: the five by severity, then any other value (prototype names included) in code-unit order', () => {
		const breadcrumbs = ['zeta', 'constructor', 'debug', 'toString', 'error', 'warning', 'fatal', 'info'].map(
			(level, i) => ({ level, message: `m${i}` }),
		);
		render(BreadcrumbTimeline, { breadcrumbs, occurredAt: null });
		const options = Array.from(screen.getByTestId('crumb-level-filter').querySelectorAll('option')).map((o) =>
			o.getAttribute('value'),
		);
		expect(options).toEqual(['', 'fatal', 'error', 'warning', 'info', 'debug', 'constructor', 'toString', 'zeta']);
	});

	it('U4: a category constructor and a level toString are ordinary options', async () => {
		const breadcrumbs = [
			{ category: 'constructor', level: 'info', message: 'c1' },
			{ category: 'http', level: 'toString', message: 't1' },
			{ category: 'constructor', level: 'toString', message: 'both' },
			{ category: 'http', level: 'info', message: 'plain' },
		];
		const { container } = render(BreadcrumbTimeline, { breadcrumbs, occurredAt: null });
		const options = (id: string) =>
			Array.from(screen.getByTestId(id).querySelectorAll('option')).map((o) => o.getAttribute('value'));
		expect(options('crumb-category-filter')).toEqual(['', 'constructor', 'http']);
		expect(options('crumb-level-filter')).toEqual(['', 'info', 'toString']);

		await choose('crumb-category-filter', 'constructor');
		expect(crumbs().map((c) => c.querySelector('.crumb-message')!.textContent)).toEqual(['c1', 'both']);
		expect(screen.getByTestId('crumb-count').textContent).toBe('Showing 2 of 4');
		await choose('crumb-category-filter', '');
		await choose('crumb-level-filter', 'toString');
		expect(crumbs().map((c) => c.querySelector('.crumb-message')!.textContent)).toEqual(['t1', 'both']);
		expect(screen.getByTestId('crumb-count').textContent).toBe('Showing 2 of 4');
		expect(crumbs()[0].querySelector('.crumb-level')!.classList.contains('tone-neutral')).toBe(true);
		expect(container.textContent).not.toContain('function');
		expect(container.textContent).not.toContain('[native code]');
	});
});
