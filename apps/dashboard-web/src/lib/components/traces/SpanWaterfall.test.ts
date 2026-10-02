import { describe, it, expect } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import SpanWaterfall from './SpanWaterfall.svelte';
import type { WaterfallError, WaterfallSpan } from '$lib/spans/waterfall';

// sentinel-span-ingest D6/D9: how the waterfall renders — nesting, orphans as roots, errors pinned to a span or in
// the errors lane, the 2,000 cap notice, the 14-day notice, the empty state and the span detail.

const T0 = Date.parse('2026-10-02T10:00:00.000Z') * 1000;
const span = (spanId: string, over: Partial<WaterfallSpan> = {}): WaterfallSpan => ({
	spanId,
	parentSpanId: '',
	name: `span ${spanId}`,
	op: 'http.server',
	kind: 'server',
	source: 'pulled',
	serviceName: 'core-api',
	projectName: null,
	environment: 'production',
	startUs: T0,
	durationUs: 2000,
	status: 'ok',
	attributes: { 'http.route': '/orders' },
	...over,
});
const error = (id: string, spanId: string | null, atUs = T0 + 500): WaterfallError => ({
	id,
	spanId,
	createdAt: new Date(atUs / 1000).toISOString(),
	errorClass: 'TypeError',
	message: 'boom',
	issueId: `issue-${id}`,
});

describe('SpanWaterfall', () => {
	it('nests by parent and draws an orphan as a root', () => {
		const { getAllByTestId } = render(SpanWaterfall, {
			spans: [
				span('a'),
				span('b', { parentSpanId: 'a', startUs: T0 + 10 }),
				span('orphan', { parentSpanId: 'ffffffffffffffff', startUs: T0 + 20 }),
			],
			errors: [],
			retentionDays: 14,
		});
		const depth = Object.fromEntries(getAllByTestId('waterfall-row').map((r) => [r.getAttribute('data-span-id'), r.getAttribute('data-depth')]));
		expect(depth).toEqual({ a: '0', b: '1', orphan: '0' });
	});

	it('pins an error to its span as a link to the occurrence, and puts unmatched ones in the errors lane', () => {
		const { container, getByTestId, getAllByTestId } = render(SpanWaterfall, {
			spans: [span('a'), span('b', { parentSpanId: 'a', startUs: T0 + 400 })],
			errors: [error('e1', 'b'), error('e2', null, T0 + 1500)],
			retentionDays: 14,
		});
		const pinned = getAllByTestId('waterfall-error');
		expect(pinned).toHaveLength(1);
		expect(pinned[0].closest('[data-testid="waterfall-row"]')?.getAttribute('data-span-id')).toBe('b');
		expect(pinned[0].getAttribute('href')).toBe('/issues/issue-e1?occurrence=e1');
		const lane = getByTestId('waterfall-errors-lane');
		expect(lane.querySelectorAll('[data-testid="waterfall-unmatched-error"]')).toHaveLength(1);
		expect(container.querySelectorAll('[data-testid="waterfall-error"]')).toHaveLength(1);
	});

	it('marks a failed span and shows the cap and retention notices', () => {
		const { getAllByTestId, getByTestId, queryByTestId, rerender } = render(SpanWaterfall, {
			spans: [span('a', { status: 'error' })],
			errors: [],
			truncated: true,
			retentionDays: 14,
		});
		expect(getAllByTestId('waterfall-bar')[0].classList.contains('failed')).toBe(true);
		expect(getByTestId('waterfall-truncated').textContent).toContain('2,000');
		expect(getByTestId('waterfall-retention').textContent).toContain('14 days');
		rerender({ spans: [span('a')], errors: [], truncated: false, retentionDays: 14 });
		expect(queryByTestId('waterfall-truncated')).toBeNull();
	});

	it('says there are no spans, without drawing a chart, and still states the retention', () => {
		const { getByTestId, queryAllByTestId } = render(SpanWaterfall, { spans: [], errors: [error('e', null)], retentionDays: 14 });
		expect(getByTestId('waterfall-empty').textContent).toContain('14 days');
		expect(queryAllByTestId('waterfall-row')).toHaveLength(0);
	});

	it('opens the span detail on focus and keeps it open when activated, showing a few attributes as text', async () => {
		const { getAllByTestId, queryByTestId, getByTestId } = render(SpanWaterfall, {
			spans: [span('a', { name: '<img src=x onerror=alert(1)>', attributes: { 'http.route': '/orders', n: 5 } })],
			errors: [],
			retentionDays: 14,
		});
		expect(queryByTestId('waterfall-detail')).toBeNull();
		const bar = getAllByTestId('waterfall-bar')[0];
		await fireEvent.focus(bar);
		const detail = getByTestId('waterfall-detail');
		expect(detail.textContent).toContain('/orders');
		expect(detail.textContent).toContain('2 ms');
		expect(detail.textContent).toContain('<img src=x onerror=alert(1)>'); // text, never markup
		expect(detail.querySelector('img')).toBeNull();
		await fireEvent.blur(bar);
		expect(queryByTestId('waterfall-detail')).toBeNull();
		await fireEvent.click(bar);
		await fireEvent.blur(bar);
		expect(queryByTestId('waterfall-detail')).not.toBeNull();
		expect(bar.getAttribute('aria-expanded')).toBe('true');
	});

	it('gives every bar an accessible name with the span, its duration and its errors', () => {
		const { getAllByTestId } = render(SpanWaterfall, {
			spans: [span('a', { status: 'error' })],
			errors: [error('e1', 'a')],
			retentionDays: 14,
		});
		expect(getAllByTestId('waterfall-bar')[0].getAttribute('aria-label')).toBe('span a, 2 ms, status error, 1 error');
	});
});
