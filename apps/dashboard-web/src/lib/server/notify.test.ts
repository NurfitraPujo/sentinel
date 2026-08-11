import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8, Q7, Q11) -- co-located, shuffle-safe
 * unit tests for notify.ts: notifyIssueEvent's actor-exclusion + agent-skip, and
 * sendIssueNotificationEmails' email policy (only commented|claimed|status_changed|resolved,
 * throttled) and blocking-question throttle bypass (Q11).
 */

const listSubscribers = vi.fn();
vi.mock('$lib/db/queries/subscriptions', () => ({ listSubscribers }));

const sendIssueNotificationEmail = vi.fn(() => Promise.resolve(true));
vi.mock('$lib/server/email', () => ({ sendIssueNotificationEmail }));

const logError = vi.fn();
const logInfo = vi.fn();
vi.mock('$lib/server/observability/log', () => ({ log: { error: logError, info: logInfo } }));

function makeQueueableDb() {
	const resultQueue: unknown[] = [];
	const chain: any = {};
	const methods = ['select', 'from', 'innerJoin', 'where'];
	for (const m of methods) {
		chain[m] = vi.fn(() => chain);
	}
	chain.then = vi.fn((resolve: any) => resolve(resultQueue.shift() ?? []));
	return { db: chain, resultQueue };
}

const { db: dbMock, resultQueue } = makeQueueableDb();
vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	issues: { id: 'id', projectId: 'projectId', message: 'message', issueType: 'issueType' },
	projects: { id: 'id', organizationId: 'organizationId' },
	organizations: { id: 'id', slug: 'slug' },
	notifications: {
		id: 'id',
		userId: 'userId',
		issueId: 'issueId',
		kind: 'kind',
		createdAt: 'createdAt',
	},
	users: { id: 'id', email: 'email' },
}));

const { notifyIssueEvent, sendIssueNotificationEmails } = await import('./notify');

beforeEach(() => {
	vi.clearAllMocks();
	resultQueue.length = 0;
});

function makeTx() {
	const insertCalls: unknown[] = [];
	const tx: any = {
		insert: vi.fn(() => ({
			values: vi.fn((values: unknown) => {
				insertCalls.push(values);
				return Promise.resolve(undefined);
			}),
		})),
	};
	return { tx, insertCalls };
}

describe('notifyIssueEvent', () => {
	it('excludes the actor from the notified list', async () => {
		listSubscribers.mockResolvedValueOnce([
			{ subscriberType: 'user', subscriberId: 'user-1' },
			{ subscriberType: 'user', subscriberId: 'actor-1' },
		]);
		const { tx, insertCalls } = makeTx();

		const notified = await notifyIssueEvent(tx, {
			issueId: 'issue-1',
			kind: 'commented',
			actorType: 'user',
			actorId: 'actor-1',
		});

		expect(notified).toEqual([{ userId: 'user-1', kind: 'commented' }]);
		expect(insertCalls).toHaveLength(1);
		expect((insertCalls[0] as unknown[])).toHaveLength(1);
	});

	it('never notifies agent subscribers (M4: they poll, no notifications row)', async () => {
		listSubscribers.mockResolvedValueOnce([{ subscriberType: 'agent', subscriberId: 'agent-1' }]);
		const { tx, insertCalls } = makeTx();

		const notified = await notifyIssueEvent(tx, {
			issueId: 'issue-1',
			kind: 'commented',
			actorType: 'system',
			actorId: 'system',
		});

		expect(notified).toEqual([]);
		expect(insertCalls).toHaveLength(0);
	});

	it('is a no-op (no insert call at all) when there are no eligible subscribers', async () => {
		listSubscribers.mockResolvedValueOnce([]);
		const { tx, insertCalls } = makeTx();

		await notifyIssueEvent(tx, { issueId: 'issue-1', kind: 'linked', actorType: 'user', actorId: 'user-1' });

		expect(insertCalls).toHaveLength(0);
	});
});

const ISSUE_LINK_ROW = { title: 'Login broken', issueType: 'user_report', projectId: 'proj-1', orgSlug: 'acme' };

describe('sendIssueNotificationEmails', () => {
	it('never emails for "linked" or "progress_update" kinds', async () => {
		await sendIssueNotificationEmails(
			[
				{ userId: 'user-1', kind: 'linked' },
				{ userId: 'user-1', kind: 'progress_update' },
			],
			{ issueId: 'issue-1', origin: 'https://app.example' }
		);

		expect(sendIssueNotificationEmail).not.toHaveBeenCalled();
	});

	it('emails an emailable kind when not throttled', async () => {
		resultQueue.push(
			[ISSUE_LINK_ROW], // getIssueLinkInfo
			[{ count: 1 }], // isThrottled: only this one emailable row in the window
			[{ email: 'reporter@example.com' }] // user email lookup
		);

		await sendIssueNotificationEmails([{ userId: 'user-1', kind: 'commented' }], {
			issueId: 'issue-1',
			origin: 'https://app.example',
		});

		expect(sendIssueNotificationEmail).toHaveBeenCalledWith(
			'reporter@example.com',
			'https://app.example/acme/reports/issue-1',
			'commented',
			'Login broken'
		);
	});

	it('Q7: throttles a second emailable notification for the same (user, issue) within 15 minutes', async () => {
		resultQueue.push(
			[ISSUE_LINK_ROW],
			[{ count: 2 }] // isThrottled: this row plus an earlier one already in the window
		);

		await sendIssueNotificationEmails([{ userId: 'user-1', kind: 'claimed' }], {
			issueId: 'issue-1',
			origin: 'https://app.example',
		});

		expect(sendIssueNotificationEmail).not.toHaveBeenCalled();
	});

	it('Q11: "question_asked" ALWAYS emails, bypassing the throttle even when already at the cap', async () => {
		resultQueue.push(
			[ISSUE_LINK_ROW],
			// No isThrottled query pushed at all for question_asked -- proves the bypass skips the
			// throttle check entirely rather than checking-and-ignoring it.
			[{ email: 'watcher@example.com' }]
		);

		await sendIssueNotificationEmails([{ userId: 'user-1', kind: 'question_asked' }], {
			issueId: 'issue-1',
			origin: 'https://app.example',
		});

		expect(sendIssueNotificationEmail).toHaveBeenCalledWith(
			'watcher@example.com',
			'https://app.example/acme/reports/issue-1',
			'question_asked',
			'Login broken'
		);
	});

	it('builds a service-issue URL (/[orgSlug]/projects/[projectId]/issues/[issueId]) for issue_type=system_error', async () => {
		resultQueue.push(
			[{ ...ISSUE_LINK_ROW, issueType: 'system_error' }],
			[{ count: 1 }],
			[{ email: 'dev@example.com' }]
		);

		await sendIssueNotificationEmails([{ userId: 'user-1', kind: 'resolved' }], {
			issueId: 'issue-2',
			origin: 'https://app.example',
		});

		expect(sendIssueNotificationEmail).toHaveBeenCalledWith(
			'dev@example.com',
			'https://app.example/acme/projects/proj-1/issues/issue-2',
			'resolved',
			'Login broken'
		);
	});

	it('is a no-op when the notified list is empty', async () => {
		await sendIssueNotificationEmails([], { issueId: 'issue-1', origin: 'https://app.example' });
		expect(sendIssueNotificationEmail).not.toHaveBeenCalled();
	});
});
