import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render } from '@testing-library/svelte';
import Page from './+page.svelte';
import { pageData, FLUTTER_OCCURRENCE, PRE_V2_OCCURRENCE } from '$lib/components/issues/occurrence-fixtures';

// sentinel-occurrence-spans TASK-01/02: the issue page's spans badge, the issue-level trace link and the "with spans only"
// toggle. The loader's side is page.server.spans.test.ts; the badge/button's behaviour is SpanPullControl.test.ts. A pre-v2
// card's frozen markup is page.occurrence-compat.test.ts's golden, so it must gain nothing.

const ORG = '11111111-1111-4111-8111-111111111111';
const TRACE = FLUTTER_OCCURRENCE.traceId as string;
const OTHER_TRACE = 'c'.repeat(32);

beforeEach(() => {
	vi.restoreAllMocks();
	vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({}) }));
});

const filter = (over: Record<string, unknown> = {}) => ({ active: false, scanned: 0, bound: 2000, exhausted: false, ...over });

function renderPage(overrides: Record<string, unknown> = {}, occurrences: any[] = [FLUTTER_OCCURRENCE]) {
	return render(Page, {
		data: {
			...pageData({ occurrences }),
			traceOrgSlug: 'acme',
			relatedTraceErrors: null,
			organizationId: ORG,
			spanPullEnabled: true,
			spansStatus: {},
			latestSpansTrace: null,
			spansFilter: filter(),
			...overrides,
		},
	});
}

describe('the per-occurrence badge', () => {
	it.each([
		['available', 'spans'],
		['pending', 'spans pending'],
		['none', 'no spans'],
	])('%s -> "%s"', (status, text) => {
		const { container } = renderPage({ spansStatus: { [FLUTTER_OCCURRENCE.id]: status } });
		expect(container.querySelector('[data-testid="spans-badge"]')?.textContent).toBe(text);
	});

	it('offers "Pull spans" on an occurrence with no spans, and only when the organization allows it', () => {
		const status = { spansStatus: { [FLUTTER_OCCURRENCE.id]: 'none' } };
		expect(renderPage(status).container.querySelector('[data-testid="pull-spans"]')).not.toBeNull();
		expect(renderPage({ ...status, spanPullEnabled: false }).container.querySelector('[data-testid="pull-spans"]')).toBeNull();
	});

	it('is absent for an occurrence with no status (no canonical trace id) and for a loader that sent none', () => {
		expect(renderPage({ spansStatus: {} }).container.querySelector('[data-testid="spans-badge"]')).toBeNull();
		expect(renderPage({ spansStatus: undefined }).container.querySelector('[data-testid="spans-badge"]')).toBeNull();
	});

	it('a pre-v2 card gains nothing', () => {
		const { container } = renderPage({ spansStatus: { [PRE_V2_OCCURRENCE.id]: 'none' } }, [PRE_V2_OCCURRENCE]);
		expect(container.querySelector('[data-testid="spans-control"]')).toBeNull();
	});
});

describe('the issue-level trace link', () => {
	it('goes to the latest occurrence WITH spans, unlabelled', () => {
		const { container } = renderPage({ latestSpansTrace: { occurrenceId: 'x', traceId: OTHER_TRACE } });
		expect(container.querySelector('[data-testid="view-trace"]')?.getAttribute('href')).toBe(`/acme/traces/${OTHER_TRACE}`);
		expect(container.querySelector('[data-testid="view-trace-no-spans"]')).toBeNull();
	});

	it('falls back to the latest traced occurrence and says no spans have been pulled yet', () => {
		const { container } = renderPage({ spansStatus: { [FLUTTER_OCCURRENCE.id]: 'none' } });
		expect(container.querySelector('[data-testid="view-trace"]')?.getAttribute('href')).toBe(`/acme/traces/${TRACE}`);
		expect(container.querySelector('[data-testid="view-trace-no-spans"]')?.textContent).toMatch(/no spans pulled yet/);
	});

	it('keeps the old behaviour (latest trace, unlabelled) for a loader that sends no spans data', () => {
		const { container } = renderPage({ spansStatus: undefined, latestSpansTrace: undefined, spansFilter: null });
		expect(container.querySelector('[data-testid="view-trace"]')?.getAttribute('href')).toBe(`/acme/traces/${TRACE}`);
		expect(container.querySelector('[data-testid="view-trace-no-spans"]')).toBeNull();
	});

	it('the fallback offers "find an occurrence with spans" (?spans=1), the only place the bounded walk runs', () => {
		const { container } = renderPage({ spansStatus: {}, latestSpansTrace: null });
		expect(container.querySelector('[data-testid="find-spans-occurrence"]')?.getAttribute('href')).toBe('?spans=1');
	});

	it('the fallback skips a newest occurrence that has no trace and finds the latest traced one', () => {
		const noTrace = { ...FLUTTER_OCCURRENCE, id: 'aaaaaaaa-0000-4000-8000-0000000000aa', traceId: null };
		const { container } = renderPage({ spansStatus: {} }, [noTrace, FLUTTER_OCCURRENCE]);
		expect(container.querySelector('[data-testid="view-trace"]')?.getAttribute('href')).toBe(`/acme/traces/${TRACE}`);
	});
});

describe('the "with spans only" toggle', () => {
	it('is a plain link to ?spans=1 (a shareable URL) while the filter is off', () => {
		const { container } = renderPage();
		expect(container.querySelector('[data-testid="spans-filter-on"]')?.getAttribute('href')).toBe('?spans=1');
		expect(container.querySelector('[data-testid="spans-filter-off"]')).toBeNull();
	});

	it('when on, links back to the full list and states the search bound honestly', () => {
		const { container } = renderPage({ spansFilter: filter({ active: true, scanned: 2000, exhausted: false }) });
		expect(container.querySelector('[data-testid="spans-filter-off"]')?.getAttribute('href')).toBe('?');
		expect(container.querySelector('[data-testid="spans-filter"]')?.textContent).toMatch(/newest 2,000.*stops at 2,000/s);
	});

	it('when on and the walk reached the end of the issue, does not claim a bound', () => {
		const { container } = renderPage({ spansFilter: filter({ active: true, scanned: 140, exhausted: true }) });
		expect(container.querySelector('[data-testid="spans-filter"]')?.textContent).not.toMatch(/stops at/);
	});

	it('says so when nothing in the searched window has spans', () => {
		const { container } = renderPage({ spansFilter: filter({ active: true, scanned: 2000 }) }, []);
		expect(container.querySelector('[data-testid="spans-filter-empty"]')?.textContent).toMatch(/No occurrences with spans among the newest 2,000/);
	});

	it('is absent for a loader that sends no filter state', () => {
		expect(renderPage({ spansFilter: undefined }).container.querySelector('[data-testid="spans-filter"]')).toBeNull();
	});
});
