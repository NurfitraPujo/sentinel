import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render } from '@testing-library/svelte';
import Page from './+page.svelte';
import {
	pageData,
	FLUTTER_OCCURRENCE,
	PRE_V2_OCCURRENCE,
} from '$lib/components/issues/occurrence-fixtures';

// sentinel-cross-repo-trace-view TASK-02 (contract D5): the issue page's links into the trace view.
// The pre-v2 card's frozen markup is page.occurrence-compat.test.ts's golden; this file asserts the
// v2 side and that a pre-v2 card gains NO link.

const TRACE = FLUTTER_OCCURRENCE.traceId as string;
const HREF = `/acme/traces/${TRACE}`;

beforeEach(() => {
	vi.restoreAllMocks();
	vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({}) }));
});

function renderPage(overrides: Record<string, unknown> = {}) {
	return render(Page, {
		data: {
			...pageData({ occurrences: [FLUTTER_OCCURRENCE] }),
			traceOrgSlug: 'acme',
			relatedTraceErrors: null,
			...overrides,
		},
	});
}

describe('a v2 occurrence’s trace id', () => {
	it('links to /{org}/traces/{id}', () => {
		const { container } = renderPage();
		const link = container.querySelector<HTMLAnchorElement>('[data-testid="trace-link"]');
		expect(link?.getAttribute('href')).toBe(HREF);
		expect(link?.textContent).toBe(TRACE);
	});

	it('stays plain text when the loader supplied no org slug', () => {
		const { container } = renderPage({ traceOrgSlug: null });
		expect(container.querySelector('[data-testid="trace-link"]')).toBeNull();
		expect(container.querySelector('.trace-id')?.textContent).toContain(TRACE);
	});

	it('stays plain text when the stored id is not 32-hex (the page could only answer 404)', () => {
		const { container } = renderPage({
			...pageData({ occurrences: [{ ...FLUTTER_OCCURRENCE, traceId: 'not-a-trace-id' }] }),
		});
		expect(container.querySelector('[data-testid="trace-link"]')).toBeNull();
	});
});

describe('"N related errors in other projects"', () => {
	it('shows the loader’s count on the SELECTED card', () => {
		const { container } = renderPage({ relatedTraceErrors: { count: 3, capped: false } });
		expect(container.querySelector('[data-testid="related-trace-errors"]')?.textContent).toBe(
			'3 related errors in other projects'
		);
	});

	it('is singular for 1 and reads 1,000+ when capped', () => {
		expect(
			renderPage({ relatedTraceErrors: { count: 1, capped: false } }).container.querySelector(
				'[data-testid="related-trace-errors"]'
			)?.textContent
		).toBe('1 related error in other projects');
		expect(
			renderPage({ relatedTraceErrors: { count: 1000, capped: true } }).container.querySelector(
				'[data-testid="related-trace-errors"]'
			)?.textContent
		).toBe('1,000+ related errors in other projects');
	});

	it('is absent when N is 0 or the loader had none', () => {
		expect(
			renderPage({ relatedTraceErrors: { count: 0, capped: false } }).container.querySelector(
				'[data-testid="related-trace-errors"]'
			)
		).toBeNull();
		expect(renderPage().container.querySelector('[data-testid="related-trace-errors"]')).toBeNull();
	});

	it('is shown on the selected card only, not on every card', () => {
		const other = { ...FLUTTER_OCCURRENCE, id: 'aaaaaaaa-0000-4000-8000-0000000000ff' };
		const { container } = renderPage({
			...pageData({ occurrences: [FLUTTER_OCCURRENCE, other] }),
			relatedTraceErrors: { count: 2, capped: false },
		});
		expect(container.querySelectorAll('[data-testid="trace-link"]')).toHaveLength(2);
		expect(container.querySelectorAll('[data-testid="related-trace-errors"]')).toHaveLength(1);
	});
});

describe('a pre-v2 card', () => {
	it('gets no trace link and no related count, whatever the loader supplied', () => {
		const { container } = renderPage({
			...pageData({ occurrences: [PRE_V2_OCCURRENCE] }),
			relatedTraceErrors: { count: 5, capped: false },
		});
		expect(container.querySelector('.trace-info')?.textContent).toContain(PRE_V2_OCCURRENCE.traceId);
		expect(container.querySelector('[data-testid="trace-link"]')).toBeNull();
		expect(container.querySelector('[data-testid="related-trace-errors"]')).toBeNull();
	});
});

describe('the issue header', () => {
	it('links View trace to the LATEST occurrence’s trace', () => {
		const { container } = renderPage();
		expect(container.querySelector('[data-testid="view-trace"]')?.getAttribute('href')).toBe(HREF);
	});

	it('has no View trace when the latest occurrence has no trace id', () => {
		const { container } = renderPage({
			...pageData({ occurrences: [{ ...FLUTTER_OCCURRENCE, traceId: null }] }),
		});
		expect(container.querySelector('[data-testid="view-trace"]')).toBeNull();
	});
});

describe('a stored trace id that is not canonical (m3)', () => {
	// Ingest stores trace_id raw; the trace page matches the lower-cased id by equality, so a link
	// (or a count) for an upper-case or padded stored value would lead to a page that cannot show it.
	it.each([['upper-case', TRACE.toUpperCase()], ['padded', ` ${TRACE}`]])(
		'%s: plain text on the card, no related count, no View trace',
		(_name, stored) => {
			const { container } = renderPage({
				...pageData({ occurrences: [{ ...FLUTTER_OCCURRENCE, traceId: stored }] }),
				relatedTraceErrors: { count: 4, capped: false },
			});
			expect(container.querySelector('[data-testid="trace-link"]')).toBeNull();
			expect(container.querySelector('[data-testid="view-trace"]')).toBeNull();
			expect(container.querySelector('[data-testid="related-trace-errors"]')).toBeNull();
			expect(container.querySelector('.trace-id')?.textContent).toContain(stored.trim());
		}
	);
});
