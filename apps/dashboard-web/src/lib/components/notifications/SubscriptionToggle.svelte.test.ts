import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import SubscriptionToggle from './SubscriptionToggle.svelte';

// Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8/§10) -- co-located, shuffle-safe
// component test for the manual subscribe/unsubscribe toggle.

beforeEach(() => {
	vi.restoreAllMocks();
});

describe('SubscriptionToggle', () => {
	it('renders Subscribe when initially unsubscribed', () => {
		render(SubscriptionToggle, { props: { issueId: 'issue-1', initialSubscribed: false } });
		expect(screen.getByRole('button').textContent).toMatch(/Subscribe/);
	});

	it('renders Unsubscribe when initially subscribed', () => {
		render(SubscriptionToggle, { props: { issueId: 'issue-1', initialSubscribed: true } });
		expect(screen.getByRole('button').textContent).toMatch(/Unsubscribe/);
	});

	it('PUTs on toggle from unsubscribed and flips to Unsubscribe', async () => {
		const fetchMock = vi.fn(async (url: string, opts?: any) => {
			expect(url).toBe('/api/issues/issue-1/subscription');
			expect(opts?.method).toBe('PUT');
			return { ok: true, json: async () => ({ subscribed: true }) };
		});
		vi.stubGlobal('fetch', fetchMock);

		render(SubscriptionToggle, { props: { issueId: 'issue-1', initialSubscribed: false } });

		await fireEvent.click(screen.getByRole('button'));

		await waitFor(() => {
			expect(screen.getByRole('button').textContent).toMatch(/Unsubscribe/);
		});
	});

	it('DELETEs on toggle from subscribed and flips to Subscribe', async () => {
		const fetchMock = vi.fn(async (url: string, opts?: any) => {
			expect(url).toBe('/api/issues/issue-1/subscription');
			expect(opts?.method).toBe('DELETE');
			return { ok: true, json: async () => ({ subscribed: false }) };
		});
		vi.stubGlobal('fetch', fetchMock);

		render(SubscriptionToggle, { props: { issueId: 'issue-1', initialSubscribed: true } });

		await fireEvent.click(screen.getByRole('button'));

		await waitFor(() => {
			expect(screen.getByRole('button').textContent).toMatch(/Subscribe/);
			expect(screen.getByRole('button').textContent).not.toMatch(/Unsubscribe/);
		});
	});

	it('shows an error message and does not flip state when the PUT fails', async () => {
		vi.stubGlobal(
			'fetch',
			vi.fn(async () => ({ ok: false, status: 500, json: async () => ({ message: 'boom' }) }))
		);

		render(SubscriptionToggle, { props: { issueId: 'issue-1', initialSubscribed: false } });

		await fireEvent.click(screen.getByRole('button'));

		await waitFor(() => {
			expect(screen.getByRole('alert').textContent).toMatch(/boom/);
		});
		expect(screen.getByRole('button').textContent).toMatch(/Subscribe/);
		expect(screen.getByRole('button').textContent).not.toMatch(/Unsubscribe/);
	});
});
