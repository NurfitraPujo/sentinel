import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, within, cleanup } from '@testing-library/svelte';
import CommentThread from './CommentThread.svelte';

// Manual Issues M3 (docs/plans/MANUAL_ISSUES_DESIGN.md §5): component tests for thread
// rendering, reply expansion, and composer submit against a mocked `fetch` -- the polling
// merge/timestamp logic itself is covered separately and exhaustively in comment-poll.test.ts
// (pure functions, no component needed), per the same "extracted for testability" split the
// design calls for.

function rootComment(overrides: Partial<Record<string, unknown>> = {}) {
	return {
		id: 'root-1',
		issueId: 'issue-1',
		parentId: null,
		authorType: 'user',
		authorId: 'user-1',
		authorName: 'Alice',
		authorEmail: 'alice@example.com',
		blocking: false,
		bodyMd: 'Hello from Alice',
		createdAt: '2026-01-01T00:00:00.000Z',
		editedAt: null,
		attachments: [],
		replies: [],
		...overrides,
	};
}

function agentReply(overrides: Partial<Record<string, unknown>> = {}) {
	return {
		id: 'reply-1',
		issueId: 'issue-1',
		parentId: 'root-1',
		authorType: 'agent',
		authorId: 'agent-1',
		authorName: 'AutoFix Agent',
		authorEmail: null,
		blocking: false,
		bodyMd: 'I looked into it.',
		createdAt: '2026-01-01T01:00:00.000Z',
		editedAt: null,
		attachments: [],
		replies: [],
		...overrides,
	};
}

function jsonResponse(body: unknown, status = 200) {
	return { ok: status < 300, status, json: async () => body };
}

describe('CommentThread', () => {
	// A very large interval so the background poll doesn't fire mid-test and race assertions.
	const NO_POLL = 10 ** 8;

	beforeEach(() => {
		vi.restoreAllMocks();
	});

	afterEach(() => {
		cleanup();
	});

	it('loads and renders root comments chronologically with author + agent badge', async () => {
		const fetchMock = vi.fn().mockResolvedValue(
			jsonResponse({ comments: [rootComment(), rootComment({ id: 'root-2', authorType: 'agent', authorId: 'agent-1', authorName: 'AutoFix Agent', bodyMd: 'Agent post' })] })
		);
		vi.stubGlobal('fetch', fetchMock);

		render(CommentThread, {
			issueId: 'issue-1',
			organizationId: 'org-1',
			currentUserId: 'user-1',
			pollIntervalMs: NO_POLL,
		});

		expect(await screen.findByText('Hello from Alice')).toBeTruthy();
		expect(screen.getByText('Agent post')).toBeTruthy();
		expect(screen.getByText('Agent')).toBeTruthy(); // agent badge
		expect(fetchMock).toHaveBeenCalledWith('/api/issues/issue-1/comments');
	});

	it('shows an empty state with no comments', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ comments: [] })));

		render(CommentThread, {
			issueId: 'issue-1',
			organizationId: 'org-1',
			currentUserId: 'user-1',
			pollIntervalMs: NO_POLL,
		});

		expect(await screen.findByText(/no comments yet/i)).toBeTruthy();
	});

	it('starts replies collapsed behind a "N replies" expander and reveals them on click', async () => {
		vi.stubGlobal(
			'fetch',
			vi.fn().mockResolvedValue(jsonResponse({ comments: [rootComment({ replies: [agentReply()] })] }))
		);

		render(CommentThread, {
			issueId: 'issue-1',
			organizationId: 'org-1',
			currentUserId: 'user-1',
			pollIntervalMs: NO_POLL,
		});

		await screen.findByText('Hello from Alice');
		expect(screen.queryByText('I looked into it.')).toBeNull();

		const expander = screen.getByRole('button', { name: /1 reply/i });
		await fireEvent.click(expander);

		expect(screen.getByText('I looked into it.')).toBeTruthy();
		expect(screen.getByRole('button', { name: /hide replies/i })).toBeTruthy();
	});

	it('submits the root composer, POSTing bodyMd + attachmentIds and refreshing via poll', async () => {
		const fetchMock = vi
			.fn()
			.mockResolvedValueOnce(jsonResponse({ comments: [] })) // initial load
			.mockResolvedValueOnce(jsonResponse({ comment: rootComment({ id: 'new-1', bodyMd: 'New comment' }) }, 201)) // POST
			.mockResolvedValueOnce(jsonResponse({ comments: [rootComment({ id: 'new-1', bodyMd: 'New comment' })] })); // poll after POST

		vi.stubGlobal('fetch', fetchMock);

		render(CommentThread, {
			issueId: 'issue-1',
			organizationId: 'org-1',
			currentUserId: 'user-1',
			pollIntervalMs: NO_POLL,
		});

		await screen.findByText(/no comments yet/i);

		const textarea = screen.getByLabelText('Write a comment…');
		await fireEvent.input(textarea, { target: { value: 'New comment' } });

		const submitButtons = screen.getAllByRole('button', { name: 'Comment' });
		await fireEvent.click(submitButtons[submitButtons.length - 1]);

		await screen.findByText('New comment');

		const postCall = fetchMock.mock.calls.find(([, init]) => init && init.method === 'POST');
		expect(postCall).toBeTruthy();
		expect(postCall![0]).toBe('/api/issues/issue-1/comments');
		expect(JSON.parse(postCall![1].body)).toEqual({ bodyMd: 'New comment', attachmentIds: [], parentId: undefined });
	});

	it('shows Edit/Delete for the current user\'s own comment but only Delete-by-moderator for others', async () => {
		vi.stubGlobal(
			'fetch',
			vi.fn().mockResolvedValue(
				jsonResponse({
					comments: [
						rootComment({ id: 'own', authorId: 'user-1', bodyMd: 'Mine' }),
						rootComment({ id: 'other', authorId: 'user-2', bodyMd: 'Not mine' }),
					],
				})
			)
		);

		render(CommentThread, {
			issueId: 'issue-1',
			organizationId: 'org-1',
			currentUserId: 'user-1',
			currentUserRole: 'admin',
			pollIntervalMs: NO_POLL,
		});

		await screen.findByText('Mine');

		const ownRow = screen.getByTestId('comment-own');
		expect(within(ownRow).getByRole('button', { name: 'Edit' })).toBeTruthy();
		expect(within(ownRow).getByRole('button', { name: 'Delete' })).toBeTruthy();

		const otherRow = screen.getByTestId('comment-other');
		expect(within(otherRow).queryByRole('button', { name: 'Edit' })).toBeNull();
		// admin role -> can still delete others' comments (§9 moderator rule)
		expect(within(otherRow).getByRole('button', { name: 'Delete' })).toBeTruthy();
	});

	it('does not offer delete on another user\'s comment for a non-moderator', async () => {
		vi.stubGlobal(
			'fetch',
			vi.fn().mockResolvedValue(jsonResponse({ comments: [rootComment({ id: 'other', authorId: 'user-2', bodyMd: 'Not mine' })] }))
		);

		render(CommentThread, {
			issueId: 'issue-1',
			organizationId: 'org-1',
			currentUserId: 'user-1',
			currentUserRole: 'engineer',
			pollIntervalMs: NO_POLL,
		});

		await screen.findByText('Not mine');
		const otherRow = screen.getByTestId('comment-other');
		expect(within(otherRow).queryByRole('button', { name: 'Delete' })).toBeNull();
	});
});
