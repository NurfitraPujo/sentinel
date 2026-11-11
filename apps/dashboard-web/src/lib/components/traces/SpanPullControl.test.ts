import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, fireEvent, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';
import { SPAN_POLL_MAX_MS, SPAN_POLL_MS } from '$lib/spans/status';

// sentinel-occurrence-spans TASK-01/02: the badge, the "Pull spans" button and its bounded poll.

const invalidateAll = vi.fn(async () => {});
vi.mock('$app/navigation', () => ({ invalidateAll }));

const { default: SpanPullControl } = await import('./SpanPullControl.svelte');

const ORG = '11111111-1111-4111-8111-111111111111';
const TRACE = 'a'.repeat(32);
const fetchMock = vi.fn();

const reply = (status: number, body: unknown, headers: Record<string, string> = {}) =>
	Promise.resolve(new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json', ...headers } }));

function mount(props: Record<string, unknown> = {}) {
	return render(SpanPullControl, { organizationId: ORG, traceId: TRACE, status: 'none', reason: 'not_pulled', enabled: true, ...props } as any);
}
/** Lets the awaited fetch + json() settle. */
const settle = async () => {
	for (let i = 0; i < 6; i++) await Promise.resolve();
	await tick();
};

beforeEach(() => {
	vi.useFakeTimers();
	fetchMock.mockReset();
	invalidateAll.mockClear();
	vi.stubGlobal('fetch', fetchMock);
});
afterEach(() => {
	vi.useRealTimers();
	vi.unstubAllGlobals();
});

describe('the badge', () => {
	it.each([
		['available', 'spans'],
		['pending', 'spans pending'],
		['none', 'no spans'],
	])('%s reads "%s"', (status, text) => {
		const { getByTestId } = mount({ status });
		expect(getByTestId('spans-badge').textContent).toBe(text);
	});

	it('offers the button only for `none` and only when pull is enabled for the organization', () => {
		const offered = (props: Record<string, unknown>) => {
			cleanup();
			return mount(props).queryByTestId('pull-spans') !== null;
		};
		expect(offered({ status: 'none' })).toBe(true);
		expect(offered({ status: 'available' })).toBe(false);
		expect(offered({ status: 'pending' })).toBe(false);
		expect(offered({ status: 'none', enabled: false })).toBe(false);
	});
});

describe('the pull', () => {
	it('POSTs the organization and trace as JSON, shows pending, polls every 5 s, and refreshes the page when spans arrive', async () => {
		fetchMock
			.mockImplementationOnce(() => reply(200, { status: 'pending', reason: null, action: 'queued' }))
			.mockImplementationOnce(() => reply(200, { status: 'pending', reason: null }))
			.mockImplementationOnce(() => reply(200, { status: 'available', reason: null }));
		const { getByTestId, queryByTestId } = mount();
		await fireEvent.click(getByTestId('pull-spans'));
		await settle();

		const [url, init] = fetchMock.mock.calls[0];
		expect(url).toBe('/api/spans/pull');
		expect(init.method).toBe('POST');
		expect(init.headers['content-type']).toBe('application/json');
		expect(JSON.parse(init.body)).toEqual({ organizationId: ORG, traceId: TRACE });
		expect(getByTestId('spans-badge').textContent).toBe('spans pending');
		expect(queryByTestId('pull-spans')).toBeNull();

		await vi.advanceTimersByTimeAsync(SPAN_POLL_MS);
		expect(fetchMock.mock.calls[1][0]).toBe(`/api/spans/pull?${new URLSearchParams({ organizationId: ORG, traceId: TRACE })}`);
		expect(invalidateAll).not.toHaveBeenCalled();
		await vi.advanceTimersByTimeAsync(SPAN_POLL_MS);
		await settle();
		expect(getByTestId('spans-badge').textContent).toBe('spans');
		expect(invalidateAll).toHaveBeenCalledTimes(1);

		await vi.advanceTimersByTimeAsync(SPAN_POLL_MAX_MS);
		expect(fetchMock).toHaveBeenCalledTimes(3); // polling stopped
	});

	it('a worker that finishes with nothing ends the poll with the reason, and offers the button again', async () => {
		fetchMock
			.mockImplementationOnce(() => reply(200, { status: 'pending', reason: null, action: 'queued' }))
			.mockImplementationOnce(() => reply(200, { status: 'none', reason: 'not_found' }));
		const { getByTestId } = mount({ variant: 'panel' });
		await fireEvent.click(getByTestId('pull-spans'));
		await settle();
		expect(getByTestId('spans-empty-reason').textContent).toMatch(/being pulled/);
		await vi.advanceTimersByTimeAsync(SPAN_POLL_MS);
		await settle();
		expect(getByTestId('spans-empty-reason').textContent).toMatch(/trace store has no spans/);
		expect(getByTestId('pull-spans')).toBeTruthy();
		expect(invalidateAll).not.toHaveBeenCalled();
		await vi.advanceTimersByTimeAsync(SPAN_POLL_MAX_MS);
		expect(fetchMock).toHaveBeenCalledTimes(2);
	});

	it('gives up after SPAN_POLL_MAX_MS and says the pull is still pending', async () => {
		fetchMock.mockImplementation((_u: string, init?: RequestInit) => reply(200, { status: 'pending', reason: null, action: init?.method === 'POST' ? 'queued' : undefined }));
		const { getByTestId } = mount();
		await fireEvent.click(getByTestId('pull-spans'));
		await settle();
		await vi.advanceTimersByTimeAsync(SPAN_POLL_MAX_MS + SPAN_POLL_MS);
		await settle();
		const polls = fetchMock.mock.calls.length - 1;
		expect(polls).toBeGreaterThanOrEqual(SPAN_POLL_MAX_MS / SPAN_POLL_MS - 1);
		expect(polls).toBeLessThanOrEqual(SPAN_POLL_MAX_MS / SPAN_POLL_MS + 1);
		expect(getByTestId('spans-notice').textContent).toMatch(/Still pending/);
		const after = fetchMock.mock.calls.length;
		await vi.advanceTimersByTimeAsync(SPAN_POLL_MAX_MS);
		expect(fetchMock.mock.calls.length).toBe(after);
	});

	it.each([
		[429, { message: 'This organization has used its 30 on-demand span pulls for the hour' }, /used its 30/],
		[403, { message: 'x' }, /not enabled/],
		[404, { message: 'x' }, /not available to you/],
		[500, {}, /could not be queued/],
	])('a %s answer is shown and nothing is polled', async (code, body, text) => {
		fetchMock.mockImplementation(() => reply(code as number, body));
		const { getByTestId } = mount();
		await fireEvent.click(getByTestId('pull-spans'));
		await settle();
		expect(getByTestId('spans-notice').textContent).toMatch(text as RegExp);
		await vi.advanceTimersByTimeAsync(SPAN_POLL_MS * 3);
		expect(fetchMock).toHaveBeenCalledTimes(1);
	});
});

describe('the empty-waterfall panel', () => {
	it.each([
		['not_pulled', /have not been pulled yet/],
		['not_found', /trace store has no spans/],
		['too_large', /too large/],
		['failed', /failed repeatedly/],
	])('says why for %s', (reason, text) => {
		const { getByTestId } = mount({ variant: 'panel', reason });
		expect(getByTestId('spans-empty-reason').textContent).toMatch(text as RegExp);
	});
});
