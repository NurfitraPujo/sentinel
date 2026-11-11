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
	chain.execute = vi.fn(() => Promise.resolve(undefined)); // R5: stampEmailedAt's raw UPDATE
	return { db: chain, resultQueue };
}

const { db: dbMock, resultQueue } = makeQueueableDb();
vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	issues: {
		id: 'id',
		projectId: 'projectId',
		message: 'message',
		issueType: 'issueType',
		// sentinel-project-environments decision 7: the issue's environment is a column on the row
		// notifyIssueEvent already reads, which is why §2.2's guard costs no extra lookup for it.
		environment: 'environment',
	},
	projectEnvironmentGrants: {
		userId: 'userId',
		projectId: 'projectId',
		environments: 'environments',
	},
	projects: { id: 'id', organizationId: 'organizationId' },
	organizations: { id: 'id', slug: 'slug' },
	organizationMembers: { organizationId: 'organizationId', userId: 'userId' },
	notifications: {
		id: 'id',
		userId: 'userId',
		issueId: 'issueId',
		kind: 'kind',
		createdAt: 'createdAt',
		emailedAt: 'emailedAt',
	},
	users: { id: 'id', email: 'email' },
}));

const { notifyIssueEvent, sendIssueNotificationEmails } = await import('./notify');

beforeEach(() => {
	vi.clearAllMocks();
	resultQueue.length = 0;
});

// R1: notifyIssueEvent, after listSubscribers (mocked separately above), runs a SECOND query
// against `tx` (select/from/innerJoin/innerJoin/where) to find current org members among the
// user subscribers. `tx` here gets its own queue so a test can supply that result independently
// of the (module-mocked) listSubscribers call.
function makeTx() {
	const insertCalls: unknown[] = [];
	const txResultQueue: unknown[] = [];
	const tx: any = {
		insert: vi.fn(() => ({
			values: vi.fn((values: unknown) => {
				insertCalls.push(values);
				return Promise.resolve(undefined);
			}),
		})),
		select: vi.fn(() => tx),
		from: vi.fn(() => tx),
		innerJoin: vi.fn(() => tx),
		where: vi.fn(() => tx),
		then: vi.fn((resolve: any) => resolve(txResultQueue.shift() ?? [])),
	};
	return { tx, insertCalls, txResultQueue };
}

describe('notifyIssueEvent', () => {
	it('excludes the actor from the notified list', async () => {
		listSubscribers.mockResolvedValueOnce([
			{ subscriberType: 'user', subscriberId: 'user-1' },
			{ subscriberType: 'user', subscriberId: 'actor-1' },
		]);
		const { tx, insertCalls, txResultQueue } = makeTx();
		// R1's org-membership check: both are current members.
		txResultQueue.push([{ userId: 'user-1' }, { userId: 'actor-1' }]);

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

	// R1 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): a subscriber row can outlive the
	// subscriber's org membership (nothing previously deleted it on removal) -- notifyIssueEvent
	// must re-check CURRENT membership, joined through the issue's project -> org, and never
	// notify someone who is subscribed but no longer a member.
	it('R1: never notifies a subscriber who is no longer a current org member, even though still subscribed', async () => {
		listSubscribers.mockResolvedValueOnce([
			{ subscriberType: 'user', subscriberId: 'current-member-1' },
			{ subscriberType: 'user', subscriberId: 'ex-member-1' },
		]);
		const { tx, insertCalls, txResultQueue } = makeTx();
		// Membership query returns only current-member-1 -- ex-member-1 was removed from the org.
		txResultQueue.push([{ userId: 'current-member-1' }]);

		const notified = await notifyIssueEvent(tx, {
			issueId: 'issue-1',
			kind: 'commented',
			actorType: 'system',
			actorId: 'system',
		});

		expect(notified).toEqual([{ userId: 'current-member-1', kind: 'commented' }]);
		expect(insertCalls).toHaveLength(1);
		expect(insertCalls[0]).toEqual([
			expect.objectContaining({ userId: 'current-member-1' }),
		]);
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

	it('emails an emailable kind when not throttled (R5: isThrottled checks emailed_at rows, not attempt rows)', async () => {
		resultQueue.push(
			[ISSUE_LINK_ROW], // getIssueLinkInfo
			[{ count: 0 }], // isThrottled: no notification for this (user, issue) has actually been emailed in the window
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
		// R5: the notification row this email was for gets stamped, via a raw UPDATE (db.execute),
		// so a later isThrottled check within the window sees an actual send, not just an attempt.
		expect(dbMock.execute).toHaveBeenCalledTimes(1);
	});

	// R5 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): the old count-based throttle counted
	// notification ROWS, including ones that were themselves throttled and never emailed --
	// `count > 1` tripped on the SECOND emailable row ever, so a sub-15-min cadence emailed
	// exactly once, ever. This proves the fix's actual contract: t0 emails (nothing sent yet in
	// the window), t10 is throttled (t0's send IS in the window), t20 emails again (t0's send has
	// now aged out of the 15-min window -- simulated here by isThrottled's query returning 0,
	// exactly as it would once 15 minutes have really passed).
	it('R5: events at t0/t10/t20 email at t0 AND t20, throttling only t10', async () => {
		// t0: nothing emailed yet in the window -> sends, and stamps emailed_at.
		resultQueue.length = 0;
		resultQueue.push([ISSUE_LINK_ROW], [{ count: 0 }], [{ email: 'watcher@example.com' }]);
		await sendIssueNotificationEmails([{ userId: 'user-1', kind: 'commented' }], {
			issueId: 'issue-1',
			origin: 'https://app.example',
		});
		expect(sendIssueNotificationEmail).toHaveBeenCalledTimes(1);

		// t10: t0's send is still within the 15-min window -> throttled, no email, no stamp.
		// The user-email row is queued anyway (fully deterministic regardless of branch, reset
		// between phases below) -- this is what makes the assertion below actually discriminate: if
		// isThrottled wrongly returns false here (the pre-fix `count > 1` never trips on count=1),
		// the code proceeds to this queued row and calls sendIssueNotificationEmail a second time.
		resultQueue.length = 0;
		resultQueue.push([ISSUE_LINK_ROW], [{ count: 1 }], [{ email: 'watcher@example.com' }]);
		await sendIssueNotificationEmails([{ userId: 'user-1', kind: 'commented' }], {
			issueId: 'issue-1',
			origin: 'https://app.example',
		});
		expect(sendIssueNotificationEmail).toHaveBeenCalledTimes(1); // still just the t0 send

		// t20: t0's send has aged out of the window -> sends again.
		resultQueue.length = 0;
		resultQueue.push([ISSUE_LINK_ROW], [{ count: 0 }], [{ email: 'watcher@example.com' }]);
		await sendIssueNotificationEmails([{ userId: 'user-1', kind: 'commented' }], {
			issueId: 'issue-1',
			origin: 'https://app.example',
		});
		expect(sendIssueNotificationEmail).toHaveBeenCalledTimes(2); // t0 and t20, not t10
	});

	it('Q7: throttles a second emailable notification for the same (user, issue) within 15 minutes', async () => {
		resultQueue.push(
			[ISSUE_LINK_ROW],
			[{ count: 1 }] // isThrottled: an earlier emailed row for this (user, issue) is in the window
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

	// The canonical service-issue URL is /issues/[id] (owner's call, 2026-09-08). This used to
	// assert /[orgSlug]/projects/[projectId]/issues/[issueId]; that path still RESOLVES — it
	// redirects — so links in already-delivered emails keep working, but nothing new is minted
	// against it.
	it('builds a service-issue URL (/issues/[id]) for issue_type=system_error', async () => {
		resultQueue.push(
			[{ ...ISSUE_LINK_ROW, issueType: 'system_error' }],
			[{ count: 0 }],
			[{ email: 'dev@example.com' }]
		);

		await sendIssueNotificationEmails([{ userId: 'user-1', kind: 'resolved' }], {
			issueId: 'issue-2',
			origin: 'https://app.example',
		});

		expect(sendIssueNotificationEmail).toHaveBeenCalledWith(
			'dev@example.com',
			'https://app.example/issues/issue-2',
			'resolved',
			'Login broken'
		);
	});

	it('is a no-op when the notified list is empty', async () => {
		await sendIssueNotificationEmails([], { issueId: 'issue-1', origin: 'https://app.example' });
		expect(sendIssueNotificationEmail).not.toHaveBeenCalled();
	});
});

/**
 * sentinel-project-environments §2.2, decision 25 — **the highest-value new guard in the feature
 * after the migration's**, and the only one in this file about something that CANNOT BE RECALLED.
 *
 * A subscriber outside the issue's environment gets no `notifications` row and no email. There is
 * one filter and it is here, at subscriber resolution, not a second one in the post-commit sender:
 * `sendIssueNotificationEmails` takes the `NotifiedUser[]` this function RETURNS, so a subscriber
 * filtered out here is already absent from the sender's input.
 *
 * `notifyIssueEvent` runs three queries on `tx` in order — R1's org-membership check, then the
 * issue row (project + environment), then ONE batched grant lookup for every candidate subscriber.
 * The queue below supplies them in exactly that order, and a build that resolved per subscriber
 * would exhaust it.
 */
describe('notifyIssueEvent — the §2.2 environment guard (decision 25)', () => {
	function scopedTx(members: string[], issueRow: unknown, grants: unknown[]) {
		const made = makeTx();
		made.txResultQueue.push(
			members.map((userId) => ({ userId })),
			[issueRow],
			grants
		);
		return made;
	}

	it('a subscriber narrowed out of the issue’s environment gets NO row and NO email', async () => {
		listSubscribers.mockResolvedValueOnce([
			{ subscriberType: 'user', subscriberId: 'narrowed-1' },
			{ subscriberType: 'user', subscriberId: 'open-1' },
		]);
		const { tx, insertCalls } = scopedTx(
			['narrowed-1', 'open-1'],
			{ projectId: 'project-1', environment: 'production' },
			// Only the narrowed member has a grant row. Absence means all -- `open-1` is unrestricted.
			[{ userId: 'narrowed-1', environments: ['staging'] }]
		);

		const notified = await notifyIssueEvent(tx, {
			issueId: 'issue-1',
			kind: 'status_changed',
			actorType: 'system',
			actorId: 'system',
		});

		// The RETURN value is what drives the email, so this single assertion covers both halves.
		expect(notified).toEqual([{ userId: 'open-1', kind: 'status_changed' }]);
		expect(insertCalls).toHaveLength(1);
		expect(insertCalls[0]).toEqual([expect.objectContaining({ userId: 'open-1' })]);
	});

	it('the same member IS notified about an issue in an environment they hold', async () => {
		// The other side of the same fixture: the guard is about the issue's environment, not about
		// the member. Without this a filter that dropped every grant-holder would also pass above.
		listSubscribers.mockResolvedValueOnce([{ subscriberType: 'user', subscriberId: 'narrowed-1' }]);
		const { tx, insertCalls } = scopedTx(
			['narrowed-1'],
			{ projectId: 'project-1', environment: 'staging' },
			[{ userId: 'narrowed-1', environments: ['staging'] }]
		);

		const notified = await notifyIssueEvent(tx, {
			issueId: 'issue-1',
			kind: 'status_changed',
			actorType: 'system',
			actorId: 'system',
		});

		expect(notified).toEqual([{ userId: 'narrowed-1', kind: 'status_changed' }]);
		expect(insertCalls).toHaveLength(1);
	});

	it('an UNRESTRICTED subscriber is notified exactly as today — absence means all', async () => {
		// The regression that matters most: this is every subscriber in every organization on the
		// day this ships. A map read as `get(userId) ?? []` would notify nobody, ever.
		listSubscribers.mockResolvedValueOnce([{ subscriberType: 'user', subscriberId: 'open-1' }]);
		const { tx, insertCalls } = scopedTx(
			['open-1'],
			{ projectId: 'project-1', environment: 'production' },
			[]
		);

		const notified = await notifyIssueEvent(tx, {
			issueId: 'issue-1',
			kind: 'commented',
			actorType: 'system',
			actorId: 'system',
		});

		expect(notified).toEqual([{ userId: 'open-1', kind: 'commented' }]);
		expect(insertCalls).toHaveLength(1);
	});

	it('inserts NOTHING at all when every subscriber is narrowed out', async () => {
		// Not "inserts an empty array": the absence a happy-path test cannot see.
		listSubscribers.mockResolvedValueOnce([{ subscriberType: 'user', subscriberId: 'narrowed-1' }]);
		const { tx, insertCalls } = scopedTx(
			['narrowed-1'],
			{ projectId: 'project-1', environment: 'production' },
			[{ userId: 'narrowed-1', environments: ['staging'] }]
		);

		const notified = await notifyIssueEvent(tx, {
			issueId: 'issue-1',
			kind: 'resolved',
			actorType: 'system',
			actorId: 'system',
		});

		expect(notified).toEqual([]);
		expect(insertCalls).toHaveLength(0);
	});
});

/**
 * §2.2's one deliberate boundary crossing (decisions 38/41, *Known limits* 5b): `escalation.ts`'s
 * unscoped rota page rides through this sender, and the marker must reach the subject `email.ts`
 * builds. It travels on the `NotifiedUser` because this sender is the one both paths share.
 */
describe('sendIssueNotificationEmails — the unscoped marker reaches email.ts', () => {
	it('passes { unscoped: true } through for an unscoped escalation, and nothing for an ordinary one', async () => {
		// getIssueLinkInfo, then the throttle count, then the user row -- per notified user.
		resultQueue.push(
			[{ title: 'Checkout 500s', issueType: 'user_report', projectId: 'p1', orgSlug: 'acme' }],
			[{ count: 0 }],
			[{ email: 'paged@example.test' }],
			[{ count: 0 }],
			[{ email: 'assignee@example.test' }]
		);

		await sendIssueNotificationEmails(
			[
				{ userId: 'rota-1', kind: 'escalated', unscoped: true },
				{ userId: 'assignee-1', kind: 'escalated' },
			],
			{ issueId: 'issue-1', origin: 'https://sentinel.test' }
		);

		expect(sendIssueNotificationEmail).toHaveBeenNthCalledWith(
			1,
			'paged@example.test',
			expect.any(String),
			'escalated',
			'Checkout 500s',
			{ unscoped: true }
		);
		expect(sendIssueNotificationEmail).toHaveBeenNthCalledWith(
			2,
			'assignee@example.test',
			expect.any(String),
			'escalated',
			'Checkout 500s'
		);
	});
});
