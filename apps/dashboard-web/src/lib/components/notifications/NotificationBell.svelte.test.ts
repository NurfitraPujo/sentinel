import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import NotificationBell from './NotificationBell.svelte';

// Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8/§10) -- co-located, shuffle-safe
// component tests for the header bell: badge rendering, dropdown list, and mark-read fetch.

const NOTIF_A = {
	id: 'notif-1',
	kind: 'commented',
	actorType: 'user',
	actorId: 'user-2',
	actorName: 'Alice',
	payload: null,
	readAt: null,
	createdAt: new Date().toISOString(),
	issueId: 'issue-1',
	issueTitle: 'Checkout button broken',
	issueType: 'user_report',
	projectId: 'proj-1',
	orgSlug: 'acme',
};

function makeFetchMock(overrides: Record<string, unknown> = {}) {
	return vi.fn(async (url: string, opts?: any) => {
		if (url === '/api/notifications?count=unread') {
			return { ok: true, json: async () => ({ count: overrides.unreadCount ?? 2 }) };
		}
		if (url === '/api/notifications?limit=10') {
			return { ok: true, json: async () => ({ notifications: overrides.items ?? [NOTIF_A] }) };
		}
		if (url === '/api/notifications' && opts?.method === 'PATCH') {
			return { ok: true, json: async () => ({ success: true }) };
		}
		throw new Error(`Unexpected fetch: ${url}`);
	});
}

beforeEach(() => {
	vi.restoreAllMocks();
});

describe('NotificationBell', () => {
	it('renders the unread count badge from the poll', async () => {
		vi.stubGlobal('fetch', makeFetchMock({ unreadCount: 3 }));

		render(NotificationBell, { props: { pollIntervalMs: 60_000 } });

		await waitFor(() => {
			expect(screen.getByTestId('unread-badge').textContent).toBe('3');
		});
	});

	it('hides the badge when the unread count is 0', async () => {
		vi.stubGlobal('fetch', makeFetchMock({ unreadCount: 0 }));

		render(NotificationBell, { props: { pollIntervalMs: 60_000 } });

		await waitFor(() => {
			expect(screen.queryByTestId('unread-badge')).toBeNull();
		});
	});

	it('opens the dropdown and lists the latest notifications', async () => {
		vi.stubGlobal('fetch', makeFetchMock());

		render(NotificationBell, { props: { pollIntervalMs: 60_000 } });

		await fireEvent.click(screen.getByLabelText('Notifications'));

		await waitFor(() => {
			expect(screen.getByTestId('notification-dropdown')).toBeTruthy();
			expect(screen.getByText('Checkout button broken')).toBeTruthy();
		});
	});

	it('marks a notification read and decrements the badge on click', async () => {
		const fetchMock = makeFetchMock({ unreadCount: 1 });
		vi.stubGlobal('fetch', fetchMock);

		render(NotificationBell, { props: { pollIntervalMs: 60_000 } });

		await waitFor(() => expect(screen.getByTestId('unread-badge').textContent).toBe('1'));

		await fireEvent.click(screen.getByLabelText('Notifications'));
		await waitFor(() => screen.getByTestId('notification-notif-1'));

		await fireEvent.click(screen.getByTestId('notification-notif-1'));

		function findPatchCall() {
			return fetchMock.mock.calls.find(
				(call: any) => call[0] === '/api/notifications' && call[1]?.method === 'PATCH'
			);
		}

		await waitFor(() => {
			expect(findPatchCall()).toBeTruthy();
		});

		const patchCall = findPatchCall();
		expect(JSON.parse((patchCall as any)[1].body)).toEqual({ id: 'notif-1' });

		await waitFor(() => {
			expect(screen.queryByTestId('unread-badge')).toBeNull();
		});
	});

	it('mark all read sends the all:true PATCH and clears the badge', async () => {
		const fetchMock = makeFetchMock({ unreadCount: 2 });
		vi.stubGlobal('fetch', fetchMock);

		render(NotificationBell, { props: { pollIntervalMs: 60_000 } });

		await waitFor(() => expect(screen.getByTestId('unread-badge').textContent).toBe('2'));

		await fireEvent.click(screen.getByLabelText('Notifications'));
		await waitFor(() => screen.getByText('Mark all read'));
		await fireEvent.click(screen.getByText('Mark all read'));

		await waitFor(() => {
			const call = fetchMock.mock.calls.find(
				(c: any) => c[0] === '/api/notifications' && c[1]?.method === 'PATCH'
			);
			expect(call).toBeTruthy();
			expect(JSON.parse((call as any)[1].body)).toEqual({ all: true });
		});

		await waitFor(() => {
			expect(screen.queryByTestId('unread-badge')).toBeNull();
		});
	});
});
