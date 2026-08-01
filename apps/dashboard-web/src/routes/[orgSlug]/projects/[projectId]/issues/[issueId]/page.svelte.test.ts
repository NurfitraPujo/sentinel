import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import Page from './+page.svelte';

// D03: the PATCH at the bottom of this page used to `window.location.reload()` unconditionally,
// with no res.ok check — so a failed status update was invisible; the page just reloaded and
// silently kept the old status, and any error was lost to the console. This test drives the
// mutation through the real UI path (search -> link as duplicate_of a resolved issue -> the
// resulting "mark resolved" prompt -> confirm) and asserts a failed PATCH surfaces an error banner
// instead of reloading.
describe('issue detail page status mutation error handling (D03)', () => {
	const DATA = {
		issue: {
			id: 'issue-1',
			projectId: 'proj-1',
			errorClass: 'TypeError',
			message: 'boom',
			status: 'unresolved',
			firstSeen: new Date('2026-01-01T00:00:00Z'),
			assigneeType: null as string | null,
			assignedTo: null as string | null,
		},
		project: { id: 'proj-1', name: 'My Project' },
		occurrences: [],
		relations: [],
	};

	let reloadSpy: ReturnType<typeof vi.fn>;

	beforeEach(() => {
		vi.restoreAllMocks();
		reloadSpy = vi.fn();
		vi.stubGlobal('location', { ...window.location, reload: reloadSpy });
	});

	it('surfaces an error banner and does NOT reload when the status PATCH fails', async () => {
		const resolvedDup = {
			id: 'dup-1',
			errorClass: 'TypeError',
			message: 'dup issue',
			status: 'resolved',
			fingerprint: 'fp-dup',
		};

		const fetchMock = vi.fn(async (url: string, opts?: any) => {
			if (url.startsWith('/api/issues/search')) {
				return { ok: true, json: async () => ({ issues: [resolvedDup] }) };
			}
			if (url === `/api/issues/${DATA.issue.id}/relations` && opts?.method === 'POST') {
				return {
					ok: true,
					status: 201,
					json: async () => ({
						id: 'rel-new',
						sourceIssueId: DATA.issue.id,
						targetIssueId: resolvedDup.id,
						relationType: 'duplicate_of',
					}),
				};
			}
			if (url === `/api/issues/${DATA.issue.id}/status` && opts?.method === 'PATCH') {
				return { ok: false, status: 500, json: async () => ({ message: 'db exploded' }) };
			}
			throw new Error(`Unexpected fetch: ${url}`);
		});
		vi.stubGlobal('fetch', fetchMock);

		render(Page, { data: DATA as any });

		// 1. Select "Duplicate of" as the relation type.
		const relationSelect = screen.getByLabelText(/relation type/i) as HTMLSelectElement;
		await fireEvent.change(relationSelect, { target: { value: 'duplicate_of' } });

		// 2. Type a search query to trigger the debounced search.
		const searchInput = screen.getByLabelText(/search issue to link/i);
		await fireEvent.input(searchInput, { target: { value: 'dup' } });
		await new Promise((r) => setTimeout(r, 300)); // clear the 250ms debounce

		// 3. Click the search result to link it — this is a duplicate_of a resolved issue, so the
		// component surfaces the "mark resolved" prompt.
		const resultItem = await screen.findByText(resolvedDup.message);
		await fireEvent.click(resultItem);

		const markResolvedBtn = await screen.findByRole('button', { name: /mark resolved/i });

		// 4. Confirming the prompt calls onStatusChangeRequest -> the page's PATCH handler, which
		// fails.
		await fireEvent.click(markResolvedBtn);

		await waitFor(() => {
			expect(screen.getByRole('alert').textContent).toMatch(/db exploded/i);
		});
		expect(reloadSpy).not.toHaveBeenCalled();
	});
});
