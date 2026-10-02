import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';

// sentinel-cross-repo-trace-view TASK-01 (D3/D4): the trace page's states. What the rows ARE (the
// guards) is trace-view.integration.test.ts's boundary; this file owns how a loader result renders.

const gotoMock = vi.fn();
vi.mock('$app/navigation', () => ({ goto: gotoMock }));

const { default: Page } = await import('./+page.svelte');

const TRACE = 'a'.repeat(32);
const row = (n: number, over: Record<string, unknown> = {}) => ({
	id: `00000000-0000-4000-8000-00000000000${n}`,
	issueId: `11111111-0000-4000-8000-00000000000${n}`,
	projectId: `p${n}`,
	projectName: n % 2 ? 'web' : 'core',
	environment: 'production',
	traceId: TRACE,
	spanId: null,
	level: 'error',
	errorClass: 'TypeError',
	message: `boom ${n}`,
	serverName: null,
	releaseVersion: null,
	createdAt: new Date(`2026-09-25T02:00:0${n}.000Z`),
	receivedAt: new Date(`2026-09-25T02:00:0${n}.000Z`),
	skewSeconds: 0,
	...over,
});

function data(over: Record<string, unknown> = {}) {
	const occurrences = (over.occurrences ?? [row(1), row(2)]) as any[];
	return {
		orgSlug: 'acme',
		traceId: TRACE,
		occurrences,
		nextCursor: null,
		total: occurrences.length,
		totalCapped: false,
		projects: [{ id: 'p2', name: 'core' }, { id: 'p1', name: 'web' }],
		projectCount: 2,
		retentionDays: 30,
		spans: [],
		spansTruncated: false,
		spanRetentionDays: 14,
		...over,
	} as any;
}

beforeEach(() => {
	gotoMock.mockReset();
});

describe('the trace page', () => {
	it('lists the rows in the loader’s order, each linking to its issue and occurrence', () => {
		const { container, getByTestId } = render(Page, { data: data() });
		const rows = container.querySelectorAll('[data-testid="trace-row"]');
		expect(rows).toHaveLength(2);
		expect(rows[0].textContent).toContain('boom 1');
		expect(rows[1].textContent).toContain('boom 2');
		expect(rows[0].querySelector('[data-testid="trace-issue-link"]')?.getAttribute('href')).toBe(
			'/issues/11111111-0000-4000-8000-000000000001?occurrence=00000000-0000-4000-8000-000000000001'
		);
		expect(rows[0].querySelector('[data-testid="trace-project"]')?.textContent).toBe('web');
		expect(getByTestId('trace-id').textContent).toBe(TRACE);
		expect(getByTestId('trace-summary').textContent).toMatch(/2\s+errors across\s+2\s+projects/);
		expect(getByTestId('trace-projects').textContent).toBe('core, web');
	});

	it('says "no errors in this trace" and renders no list when there is nothing visible', () => {
		const { queryAllByTestId, getByTestId } = render(Page, {
			data: data({ occurrences: [], total: 0, projects: [] }),
		});
		expect(getByTestId('trace-empty').textContent).toContain('No errors in this trace');
		expect(queryAllByTestId('trace-row')).toHaveLength(0);
	});

	it('draws the span waterfall above the list when the trace has spans, pinning the loaded errors', () => {
		const span = (spanId: string, over: Record<string, unknown> = {}) => ({
			spanId,
			parentSpanId: '',
			name: `span ${spanId}`,
			op: 'http.server',
			kind: 'server',
			source: 'pulled',
			serviceName: 'core-api',
			projectName: null,
			environment: 'production',
			startUs: Date.parse('2026-09-25T02:00:00.000Z') * 1000,
			durationUs: 5_000_000,
			status: 'ok',
			attributes: {},
			...over,
		});
		const { getByTestId, getAllByTestId, container } = render(Page, {
			data: data({ occurrences: [row(1, { spanId: 'b'.repeat(16) })], spans: [span('a'.repeat(16)), span('b'.repeat(16), { parentSpanId: 'a'.repeat(16) })] }),
		});
		const wf = getByTestId('waterfall');
		expect(wf.compareDocumentPosition(getByTestId('trace-summary')) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
		expect(getAllByTestId('waterfall-row')).toHaveLength(2);
		expect(getByTestId('waterfall-error').closest('[data-testid="waterfall-row"]')?.getAttribute('data-span-id')).toBe('b'.repeat(16));
		expect(container.querySelectorAll('[data-testid="trace-row"]')).toHaveLength(1); // the list is unchanged
	});

	it('has no waterfall for a trace with neither spans nor errors', () => {
		const { queryByTestId } = render(Page, { data: data({ occurrences: [], total: 0, projects: [] }) });
		expect(queryByTestId('waterfall')).toBeNull();
	});

	it('states the retention window, as the issue page does', () => {
		const { getByTestId } = render(Page, { data: data({ retentionDays: 14 }) });
		expect(getByTestId('trace-retention').textContent).toContain('older than 14 days');
	});

	it('shows the total capped as "1,000+"', () => {
		const { getByTestId } = render(Page, { data: data({ total: 1000, totalCapped: true }) });
		expect(getByTestId('trace-total').textContent).toBe('1,000+');
	});

	it('shows a skew hint only when the DB-computed skew exceeds a minute (either direction)', () => {
		const skewed = row(1, { skewSeconds: 120 });
		const early = row(2, { skewSeconds: -120 });
		const close = row(3, { skewSeconds: 30 });
		const unknown = row(4, { skewSeconds: null, receivedAt: null });
		const { container } = render(Page, { data: data({ occurrences: [skewed, early, close, unknown] }) });
		const rows = container.querySelectorAll('[data-testid="trace-row"]');
		expect(rows[0].querySelector('[data-testid="trace-skew"]')).not.toBeNull();
		expect(rows[1].querySelector('[data-testid="trace-skew"]')).not.toBeNull();
		expect(rows[2].querySelector('[data-testid="trace-skew"]')).toBeNull();
		expect(rows[3].querySelector('[data-testid="trace-skew"]')).toBeNull();
	});

	it('reports the TRUE project count, and marks a truncated name list', () => {
		const { getByTestId } = render(Page, { data: data({ projectCount: 57 }) });
		expect(getByTestId('trace-summary').textContent).toMatch(/across\s+57\s+projects/);
		expect(getByTestId('trace-projects').textContent).toBe('core, web, …');
	});

	it('offers "Load more" only with a cursor, and asks for the next page by it', async () => {
		const without = render(Page, { data: data() });
		expect(without.queryByTestId('trace-load-more')).toBeNull();
		without.unmount();

		const { getByTestId } = render(Page, { data: data({ nextCursor: 'abc/+=' }) });
		await fireEvent.click(getByTestId('trace-load-more'));
		expect(gotoMock).toHaveBeenCalledWith('?cursor=abc%2F%2B%3D', expect.objectContaining({ noScroll: true }));
	});

	it('appends a loaded page to the rows already shown', async () => {
		const { container, rerender } = render(Page, { data: data({ occurrences: [row(1)], nextCursor: 'c' }) });
		await rerender({ data: data({ occurrences: [row(2)], nextCursor: null }) });
		expect(Array.from(container.querySelectorAll('[data-testid="trace-row"]')).map((r) => r.textContent)).toEqual([
			expect.stringContaining('boom 1'),
			expect.stringContaining('boom 2'),
		]);
	});
});
