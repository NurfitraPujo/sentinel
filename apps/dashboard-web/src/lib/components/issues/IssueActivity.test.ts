import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, fireEvent, screen, waitFor } from '@testing-library/svelte';
import IssueActivity from './IssueActivity.svelte';
import type { ActivityEntry, ActivityPage } from '$lib/issue-activity';

// sentinel-issue-activity TASK-01: the issue page's Activity section. Collapsed, it fetches NOTHING (the page's load
// carries no activity query either); opened, it reads /api/issues/{id}/activity; "Load older" follows nextCursor. Actors
// render by name -- a personal CLI agent as "<owner> (CLI)" -- never by raw id.

const ISSUE = '42a26f64-1b2c-4d5e-8f90-123456789012';
const now = new Date();
const ago = (min: number) => new Date(now.getTime() - min * 60_000).toISOString();

const entry = (over: Partial<ActivityEntry>): ActivityEntry => ({
	id: crypto.randomUUID(),
	seq: 1,
	eventType: 'commented',
	actorType: 'user',
	actorId: 'u-raw-id',
	actor: { type: 'user', id: 'u-raw-id', name: 'Grace Hopper', personal: false },
	oldValue: null,
	newValue: null,
	createdAt: ago(5),
	...over,
});

const PAGE_1: ActivityPage = {
	entries: [
		entry({
			eventType: 'claimed',
			actorType: 'agent',
			actorId: 'agent-raw-id',
			actor: { type: 'agent', id: 'agent-raw-id', name: 'Ada Lovelace (CLI)', personal: true },
			createdAt: ago(3),
		}),
		entry({
			eventType: 'occurrence_burst',
			actorType: 'system',
			actorId: 'sentinel-processor',
			actor: { type: 'system', id: 'sentinel-processor', name: 'Sentinel', personal: false },
			createdAt: ago(60),
			burst: { rows: 170, firstAt: ago(60 * 24 * 3), lastAt: ago(60), occurrencesFrom: 300, occurrencesTo: 1469 },
		}),
	],
	nextCursor: 'CURSOR-1',
	hasMore: true,
};
const PAGE_2: ActivityPage = {
	entries: [
		entry({
			eventType: 'status_changed',
			oldValue: { status: 'unresolved' },
			newValue: { status: 'resolved', resolvedInVersion: '1.4.0' },
			createdAt: ago(60 * 24 * 4),
		}),
	],
	nextCursor: null,
	hasMore: false,
};

function mockFetch(...pages: ActivityPage[]) {
	const fetchMock = vi.fn();
	for (const p of pages) fetchMock.mockResolvedValueOnce({ ok: true, status: 200, json: async () => p });
	vi.stubGlobal('fetch', fetchMock);
	return fetchMock;
}

// What a click on the summary does: the element opens and the browser (here jsdom) fires `toggle` itself.
async function open() {
	const details = screen.getByTestId('issue-activity') as HTMLDetailsElement;
	details.open = true;
	await new Promise((r) => setTimeout(r, 0));
}

beforeEach(() => {
	vi.restoreAllMocks();
	vi.unstubAllGlobals();
});

describe('IssueActivity', () => {
	it('collapsed, it fetches nothing', async () => {
		const fetchMock = mockFetch(PAGE_1);
		render(IssueActivity, { issueId: ISSUE });
		await Promise.resolve();
		expect(fetchMock).not.toHaveBeenCalled();
		expect(screen.queryByTestId('activity-entry')).toBeNull();
	});

	it('opened, it reads the first page and renders names, verbs, the burst window and relative times', async () => {
		const fetchMock = mockFetch(PAGE_1);
		render(IssueActivity, { issueId: ISSUE });
		await open();
		await waitFor(() => expect(screen.getAllByTestId('activity-entry')).toHaveLength(2));
		expect(fetchMock).toHaveBeenCalledTimes(1);
		expect(fetchMock.mock.calls[0][0]).toBe(`/api/issues/${ISSUE}/activity?limit=50`);

		const actors = screen.getAllByTestId('activity-actor').map((n) => n.textContent);
		expect(actors).toEqual(['Ada Lovelace (CLI)', 'Sentinel']);
		expect(document.body.textContent).not.toContain('agent-raw-id');

		const [claim, burst] = screen.getAllByTestId('activity-entry');
		expect(claim.textContent).toContain('claimed this issue');
		expect(claim.querySelector('time')!.textContent).toBe('3m ago');
		expect(claim.querySelector('time')!.getAttribute('title')).toMatch(/\d{4}/); // the absolute time on hover
		expect(burst.textContent).toContain('saw new occurrences');
		expect(burst.querySelector('[data-testid="activity-detail"]')!.textContent).toBe('170 bursts, 300 occurrences → 1,469');
		expect([...burst.querySelectorAll('time')].map((t) => t.textContent)).toEqual(['3d ago', '1h ago']);
	});

	it('"Load older" appends the next page with the cursor, then disappears at the start of the history', async () => {
		const fetchMock = mockFetch(PAGE_1, PAGE_2);
		render(IssueActivity, { issueId: ISSUE });
		await open();
		const more = await screen.findByTestId('activity-load-older');
		await fireEvent.click(more);
		await waitFor(() => expect(screen.getAllByTestId('activity-entry')).toHaveLength(3));
		expect(fetchMock.mock.calls[1][0]).toBe(`/api/issues/${ISSUE}/activity?limit=50&before=CURSOR-1`);
		expect(screen.getAllByTestId('activity-entry')[2].textContent).toContain('unresolved → resolved in 1.4.0');
		expect(screen.queryByTestId('activity-load-older')).toBeNull();
	});

	it('an empty history and a failed read each say so (and a failure does not retry by itself)', async () => {
		mockFetch({ entries: [], nextCursor: null, hasMore: false });
		const { unmount } = render(IssueActivity, { issueId: ISSUE });
		await open();
		expect(await screen.findByTestId('activity-empty')).toBeTruthy();
		unmount();

		const failing = vi.fn().mockResolvedValue({ ok: false, status: 500, json: async () => ({}) });
		vi.stubGlobal('fetch', failing);
		render(IssueActivity, { issueId: ISSUE });
		await open();
		expect((await screen.findByTestId('activity-error')).textContent).toContain('500');
		await new Promise((r) => setTimeout(r, 20));
		expect(failing).toHaveBeenCalledTimes(1);
	});
});
