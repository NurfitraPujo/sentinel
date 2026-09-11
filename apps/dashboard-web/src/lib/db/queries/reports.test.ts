import { describe, it, expect, vi, beforeEach } from 'vitest';

// Chainable Drizzle-query double, mirroring queries/issues.test.ts's approach: every method
// returns the same object so `.insert(x).values(y).returning()` / `.update(x).set(y).where(z)
// .returning()` / `.select(...).from(...).where(...)` chains all resolve, and `__result` decides
// what awaiting the chain yields.
function makeChainable() {
	const m: any = {};
	const methods = [
		'select',
		'from',
		'innerJoin',
		'leftJoin',
		'where',
		'orderBy',
		'insert',
		'values',
		'returning',
		'update',
		'set',
		'delete',
		'onConflictDoNothing',
	];
	for (const name of methods) {
		m[name] = vi.fn(() => m);
	}
	m.__result = [];
	m.then = vi.fn((resolve: any) => resolve(m.__result));
	return m;
}

const txMock = makeChainable();
const dbMock: any = makeChainable();
dbMock.transaction = vi.fn(async (cb: any) => cb(txMock));

vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	issues: {
		id: 'id',
		projectId: 'projectId',
		assignedTo: 'assignedTo',
		assigneeType: 'assigneeType',
		issueType: 'issueType',
		status: 'status',
		message: 'message',
		// sentinel-project-environments §2.2 (decisions 25 and 36): the column both the notify guard
		// and `listReports`'s predicate read, and the table the scope is resolved from.
		environment: 'environment',
	},
	projectEnvironmentGrants: { userId: 'userId', projectId: 'projectId', environments: 'environments' },
	issueActivity: { id: 'id', issueId: 'issueId' },
	manualIssueReports: { issueId: 'issueId', reporterId: 'reporterId', bodyMd: 'bodyMd', severity: 'severity' },
	attachments: { id: 'id', issueId: 'issueId', commentId: 'commentId', storageKey: 'storageKey' },
	issueComments: { id: 'id', issueId: 'issueId' },
	projects: { id: 'id', organizationId: 'organizationId', isInbox: 'isInbox', name: 'name' },
	users: { id: 'id', name: 'name', email: 'email' },
	organizationMembers: { organizationId: 'organizationId', userId: 'userId' },
	issueSubscriptions: {
		id: 'id',
		issueId: 'issueId',
		subscriberType: 'subscriberType',
		subscriberId: 'subscriberId',
	},
	notifications: {
		id: 'id',
		userId: 'userId',
		issueId: 'issueId',
		kind: 'kind',
		createdAt: 'createdAt',
		emailedAt: 'emailedAt',
	},
}));

vi.mock('./issues', () => ({
	getIssueActivity: vi.fn(),
}));

const {
	createManualIssue,
	claimIssue,
	releaseClaim,
	ClaimConflictError,
	moveIssueToProject,
	updateManualIssueReport,
	deleteManualIssue,
	resolveReportEnvironment,
	ENV_NAME_RE,
	DEFAULT_REPORT_ENVIRONMENT,
} = await import('./reports');

beforeEach(() => {
	vi.clearAllMocks();
	txMock.__result = [];
	dbMock.__result = [];
	// listSubscribers (called from inside notifyIssueEvent) runs a plain select against tx that
	// resolves via txMock.then -- default it to "no subscribers" so claim/release tests don't have
	// to think about fan-out unless they're the ones testing it.
	txMock.then = vi.fn((resolve: any) => resolve([]));
});

/**
 * sentinel-project-environments §4.8a — the ONE helper both `issues` writers in this file share,
 * and the value that also reaches both triage call sites (decisions 50 and 53).
 *
 * Pure: no database, no transaction, no route. Table-driven because the rule is a table.
 */
describe('resolveReportEnvironment (§4.8a)', () => {
	it.each<[unknown, string, string]>([
		// [asserted, resolved, why]
		[undefined, 'production', 'an omitted key is not an assertion'],
		[null, 'production', 'an explicit null is not an assertion'],
		['', 'production', 'an explicit empty string is not an assertion'],
		['production', 'production', 'the default is a real, grantable name and is accepted as one'],
		['staging', 'staging', 'a matching value is stored verbatim'],
		['eu-west-1', 'eu-west-1', "decision 10's widened set: hyphens and digits"],
		['staging-2', 'staging-2', 'the widened set again'],
		['prod_eu', 'prod_eu', 'underscores are allowed AFTER the first character'],
		['pre-prod', 'pre-prod', 'the operator-named case decision 10 exists for'],
		['a.b', 'a.b', 'dots are in the set'],
		['Production', 'production', 'the set is lower-case only, so this falls back'],
		['eu west', 'production', 'a space is outside the set'],
		['Prod!', 'production', 'punctuation outside [._-] is outside the set'],
		['-prod', 'production', 'a leading hyphen fails ^[a-z0-9]'],
		[
			'_unspecified',
			'production',
			"decision 51: the migration's transient DEFAULT can never be asserted as an override",
		],
		['a'.repeat(50), 'a'.repeat(50), 'exactly 50 characters is inside the bound'],
		['a'.repeat(51), 'production', '51 is outside it — the size bound is folded into the pattern'],
		[42, 'production', 'a non-string is not an assertion'],
		[{ environment: 'staging' }, 'production', 'an object is not an assertion'],
	])('resolves %j to %j (%s)', (asserted, expected) => {
		expect(resolveReportEnvironment(asserted)).toBe(expected);
	});

	it('exposes the default as the literal `production` (decision 24)', () => {
		expect(DEFAULT_REPORT_ENVIRONMENT).toBe('production');
	});

	// A `/g` RegExp carries `lastIndex` between `.test()` calls and would alternate true/false for
	// the same input — which would make one in every two valid overrides silently fall back to
	// `production`, with nothing anywhere to say so.
	it('uses a non-global RegExp, so repeated tests of the same value agree', () => {
		expect(ENV_NAME_RE.global).toBe(false);
		expect(ENV_NAME_RE.test('staging')).toBe(true);
		expect(ENV_NAME_RE.test('staging')).toBe(true);
		expect(resolveReportEnvironment('staging')).toBe('staging');
		expect(resolveReportEnvironment('staging')).toBe('staging');
	});
});

describe('createManualIssue', () => {
	it('creates the issues row, the manual_issue_reports companion, one activity row, and auto-subscribes the reporter, inside a single transaction', async () => {
		// The inserts below run in sequence against tx; queue each RETURNING result in order.
		let call = 0;
		txMock.returning = vi.fn(() => {
			call += 1;
			if (call === 1) return Promise.resolve([{ id: 'issue-1', projectId: 'project-1' }]);
			if (call === 2) return Promise.resolve([{ issueId: 'issue-1', reporterId: 'user-1' }]);
			return Promise.resolve([]);
		});

		const result = await createManualIssue({
			organizationId: 'org-1',
			projectId: 'project-1',
			reporterId: 'user-1',
			title: 'Cannot log in',
			bodyMd: 'Steps to reproduce...',
			severity: 'high',
		});

		expect(dbMock.transaction).toHaveBeenCalledTimes(1);
		// §4.9 makes issues.environment NOT NULL with no DEFAULT, so this writer must supply it.
		// This report asserted nothing, so §4.8a's default is what lands.
		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({ projectId: 'project-1', environment: 'production' })
		);
		expect(result.issue).toEqual({ id: 'issue-1', projectId: 'project-1' });
		expect(result.report).toEqual({ issueId: 'issue-1', reporterId: 'user-1' });
		// insert called 4 times: issues, manual_issue_reports, issue_activity, issue_subscriptions
		// (the reporter auto-subscribe).
		expect(txMock.insert).toHaveBeenCalledTimes(4);
		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({
				issueId: 'issue-1',
				subscriberType: 'user',
				subscriberId: 'user-1',
				reason: 'reporter',
			})
		);
		// Idempotent upsert: subscribe() always goes through onConflictDoNothing, never a plain
		// insert that would 23505 on a re-subscribe.
		expect(txMock.onConflictDoNothing).toHaveBeenCalled();

		// R12 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): creation must write 'report_created',
		// not 'report_edited' -- the latter is reserved for R11 body/title/severity edits and
		// mislabeled creation as an edit before this fix.
		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({
				issueId: 'issue-1',
				eventType: 'report_created',
				actorType: 'user',
				actorId: 'user-1',
			})
		);
	});

	// R2 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): findOrCreateTriageProject's SELECT-then-
	// INSERT is not atomic on its own -- two concurrent callers can both miss the initial SELECT
	// and both attempt the INSERT. `idx_projects_org_inbox_unique` (partial unique index,
	// 1723000000_pr13_remediation.sql) makes the LOSER's insert a no-op via `onConflictDoNothing`
	// instead of a duplicate-row error; this proves the loser then re-selects and returns the
	// WINNER's project id rather than crashing or returning its own (never-created) row.
	it('R2: when the Triage insert loses the uniqueness race (onConflictDoNothing no-op), re-selects and uses the winner\'s project id', async () => {
		let selectCall = 0;
		txMock.where = vi.fn(() => {
			selectCall += 1;
			// Only findOrCreateTriageProject's two selects (existing check, then the post-conflict
			// re-select) run through a bare `.where(...)` await in this flow -- both resolve via
			// txMock.then below, keyed off selectCall.
			return txMock;
		});

		let thenCall = 0;
		txMock.then = vi.fn((resolve: any) => {
			thenCall += 1;
			if (thenCall === 1) return resolve([]); // no existing inbox project yet
			if (thenCall === 2) return resolve([{ id: 'triage-winner' }]); // re-select finds the winner
			return resolve(undefined); // issue_activity insert / subscribe upsert (bare awaits)
		});

		let returningCall = 0;
		txMock.returning = vi.fn(() => {
			returningCall += 1;
			if (returningCall === 1) return Promise.resolve([]); // Triage project insert: LOST the race
			if (returningCall === 2) return Promise.resolve([{ id: 'issue-1', projectId: 'triage-winner' }]);
			if (returningCall === 3) return Promise.resolve([{ issueId: 'issue-1', reporterId: 'user-1' }]);
			return Promise.resolve([]);
		});

		const result = await createManualIssue({
			organizationId: 'org-1',
			projectId: null,
			reporterId: 'user-1',
			title: 'Cannot log in',
			bodyMd: 'Steps to reproduce...',
			severity: 'high',
		});

		expect(result.issue).toEqual({ id: 'issue-1', projectId: 'triage-winner' });
		expect(txMock.onConflictDoNothing).toHaveBeenCalled();
	});

	it('rejects an empty title before opening a transaction', async () => {
		await expect(
			createManualIssue({
				organizationId: 'org-1',
				projectId: 'project-1',
				reporterId: 'user-1',
				title: '   ',
				bodyMd: 'body',
				severity: 'low',
			})
		).rejects.toThrow('title must not be empty');

		expect(dbMock.transaction).not.toHaveBeenCalled();
	});
});

describe('claimIssue', () => {
	// This is the test that proves the atomic-claim conflict logic: the UPDATE is scoped
	// WHERE assigned_to IS NULL, so a second claimant racing the first gets 0 rows back, and the
	// function must surface that as a distinguishable failure (ClaimConflictError) rather than
	// silently succeeding or returning undefined (D18: the caller must be able to tell "I claimed
	// it" from "I did nothing").
	it('throws ClaimConflictError when the conditional UPDATE matches zero rows', async () => {
		txMock.returning = vi.fn(() => Promise.resolve([]));

		await expect(claimIssue('issue-1', 'user', 'user-2')).rejects.toBeInstanceOf(ClaimConflictError);

		// No activity should be written for a claim that did not happen (the UPDATE is not an
		// `insert` call at all).
		expect(txMock.insert).not.toHaveBeenCalled();
	});

	// N9 (sentinel-worker plan, contract correction C1): a retry of an already-successful claim by
	// the SAME actor must be idempotent success, not a 409 -- mirrors releaseClaim's A13 guard.
	// Red-proof: delete the re-read branch (revert to the unconditional throw on 0 rows) and this
	// test fails.
	it('self-reclaim (already claimed by the same actor) is idempotent success: no activity, no notify', async () => {
		txMock.returning = vi.fn(() => Promise.resolve([])); // conditional UPDATE matched 0 rows
		// Re-read (plain select awaited via `.then`) shows this actor already holds the claim.
		txMock.then = vi.fn((resolve: any) =>
			resolve([{ id: 'issue-1', assignedTo: 'agent-1', assigneeType: 'agent' }])
		);

		const { issue, notified, alreadyClaimed } = await claimIssue('issue-1', 'agent', 'agent-1');

		expect(alreadyClaimed).toBe(true);
		expect(issue).toEqual({ id: 'issue-1', assignedTo: 'agent-1', assigneeType: 'agent' });
		expect(notified).toEqual([]);
		expect(txMock.insert).not.toHaveBeenCalled();
	});

	it('claim when the issue is held by a DIFFERENT actor still throws ClaimConflictError', async () => {
		txMock.returning = vi.fn(() => Promise.resolve([]));
		txMock.then = vi.fn((resolve: any) =>
			resolve([{ id: 'issue-1', assignedTo: 'agent-2', assigneeType: 'agent' }])
		);

		await expect(claimIssue('issue-1', 'agent', 'agent-1')).rejects.toBeInstanceOf(ClaimConflictError);
		expect(txMock.insert).not.toHaveBeenCalled();
	});

	// Same id-space caveat as R10 on releaseClaim: assigned_to is a bare varchar shared across
	// 'user'/'agent' ids, so an id collision across actor types must NOT read as self-reclaim.
	it('an id collision across actor types is a conflict, not a self-reclaim', async () => {
		txMock.returning = vi.fn(() => Promise.resolve([]));
		txMock.then = vi.fn((resolve: any) =>
			resolve([{ id: 'issue-1', assignedTo: 'agent-1', assigneeType: 'user' }])
		);

		await expect(claimIssue('issue-1', 'agent', 'agent-1')).rejects.toBeInstanceOf(ClaimConflictError);
	});

	it('succeeds, writes one "claimed" activity row, auto-subscribes the claimant, and fans out to other subscribers, excluding the actor', async () => {
		txMock.returning = vi.fn(() =>
			Promise.resolve([{ id: 'issue-1', assigneeType: 'user', assignedTo: 'user-1' }])
		);
		// listSubscribers returns two USER subscribers: the reporter ('user-2', notified) and the
		// claimant themselves ('user-1', excluded as the actor). The next call is R1's
		// current-org-membership check (notify.ts): both are still members. The two
		// mockImplementationOnce calls before that are dummies for claimIssue's own
		// issue_activity insert and subscribe()'s upsert -- both bare-awaited through the same
		// chainable `tx.then`, ahead of notifyIssueEvent's two calls.
		txMock.then = vi
			.fn()
			.mockImplementationOnce((resolve: any) => resolve(undefined)) // issue_activity insert
			.mockImplementationOnce((resolve: any) => resolve(undefined)) // subscribe() upsert
			.mockImplementationOnce((resolve: any) =>
				resolve([
					{ subscriberType: 'user', subscriberId: 'user-2' },
					{ subscriberType: 'user', subscriberId: 'user-1' },
				])
			)
			.mockImplementationOnce((resolve: any) => resolve([{ userId: 'user-2' }, { userId: 'user-1' }]))
			// sentinel-project-environments §2.2, decision 25: two more reads in the same branch --
			// the issue row notifyIssueEvent takes `projectId` + `environment` off, then ONE batched
			// grant lookup for every candidate subscriber. No grant rows, so absence means all and the
			// fan-out asserted below is exactly the one this test always asserted.
			.mockImplementationOnce((resolve: any) =>
				resolve([{ projectId: 'proj-1', environment: 'production' }])
			)
			.mockImplementationOnce((resolve: any) => resolve([]))
			.mockImplementationOnce((resolve: any) => resolve(undefined)); // notifyIssueEvent's own notifications insert

		const { issue, notified } = await claimIssue('issue-1', 'user', 'user-1');

		expect(issue).toEqual({ id: 'issue-1', assigneeType: 'user', assignedTo: 'user-1' });
		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({ eventType: 'claimed', actorType: 'user', actorId: 'user-1' })
		);
		// Auto-subscribe (reason 'claimant').
		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({ subscriberId: 'user-1', reason: 'claimant' })
		);
		// notifyIssueEvent excludes the actor: only user-2 is notified, never user-1.
		expect(notified).toEqual([{ userId: 'user-2', kind: 'claimed' }]);
		expect(txMock.values).toHaveBeenCalledWith(
			expect.arrayContaining([expect.objectContaining({ userId: 'user-2', kind: 'claimed' })])
		);
	});

	it('auto-subscribes an agent claimant as of M5 (row exists; notifyIssueEvent still skips agent subscribers for notification rows)', async () => {
		txMock.returning = vi.fn(() =>
			Promise.resolve([{ id: 'issue-1', assigneeType: 'agent', assignedTo: 'agent-1' }])
		);
		txMock.then = vi.fn((resolve: any) => resolve([]));

		await claimIssue('issue-1', 'agent', 'agent-1');

		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({ subscriberType: 'agent', subscriberId: 'agent-1', reason: 'claimant' })
		);
		expect(txMock.onConflictDoNothing).toHaveBeenCalled();
	});
});

describe('releaseClaim', () => {
	it('throws ClaimConflictError when a non-force release matches zero rows (not the current claimant)', async () => {
		txMock.returning = vi.fn(() => Promise.resolve([]));

		await expect(releaseClaim('issue-1', 'user-2')).rejects.toBeInstanceOf(ClaimConflictError);
	});

	// R10 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): a non-force release's conditional UPDATE
	// matched on `assignedTo` alone -- `assigned_to` is a bare varchar shared across the
	// 'user'/'agent' id spaces, so an id collision across the two could let one actor type release
	// a claim actually held by the other. This proves `assigneeType` is now part of the WHERE.
	it('the non-force conditional UPDATE matches on assigneeType as well as assignedTo', async () => {
		txMock.returning = vi.fn(() => Promise.resolve([{ id: 'issue-1', assignedTo: null }]));
		txMock.then = vi
			.fn()
			.mockImplementationOnce((resolve: any) => resolve(undefined))
			.mockImplementationOnce((resolve: any) => resolve([]))
			.mockImplementationOnce((resolve: any) => resolve(undefined));

		await releaseClaim('issue-1', 'agent-1', { actorType: 'agent' });

		const whereArg = txMock.where.mock.calls[0][0];
		const serialized = JSON.stringify(whereArg);
		expect(serialized).toContain('assignedTo');
		expect(serialized).toContain('assigneeType');
	});

	// A13 (N7d): a retry of an already-successful release must not surface as a conflict. This is
	// the red-proof for the guard -- delete the re-read branch in releaseClaim (reverting to the
	// unconditional throw on 0 rows) and this test fails, because the old code cannot tell
	// "already released" from "never held".
	it('release-after-release (issue is now simply unclaimed) is idempotent success: 200, no activity, no notify', async () => {
		txMock.returning = vi.fn(() => Promise.resolve([])); // conditional UPDATE matched 0 rows
		// The re-read (a plain select, awaited via `.then`) finds the issue unclaimed by anyone.
		txMock.then = vi.fn((resolve: any) => resolve([{ id: 'issue-1', assignedTo: null, assigneeType: null }]));

		const { issue, notified } = await releaseClaim('issue-1', 'agent-1', { actorType: 'agent' });

		expect(issue).toEqual({ id: 'issue-1', assignedTo: null, assigneeType: null });
		expect(notified).toEqual([]);
		expect(txMock.insert).not.toHaveBeenCalled();
	});

	// A13: the flip side -- the issue is claimed by SOMEONE ELSE now, which is a real conflict, not
	// a self-retry. Must still 409.
	it('release when the issue is now claimed by a different actor still throws ClaimConflictError', async () => {
		txMock.returning = vi.fn(() => Promise.resolve([]));
		txMock.then = vi.fn((resolve: any) => resolve([{ id: 'issue-1', assignedTo: 'agent-2', assigneeType: 'agent' }]));

		await expect(releaseClaim('issue-1', 'agent-1', { actorType: 'agent' })).rejects.toBeInstanceOf(
			ClaimConflictError
		);
		expect(txMock.insert).not.toHaveBeenCalled();
	});

	it('force release succeeds, writes a claim_released activity row, and fans out a "claimed" notification with released:true', async () => {
		txMock.returning = vi.fn(() => Promise.resolve([{ id: 'issue-1', assignedTo: null }]));
		// One dummy for releaseClaim's own issue_activity insert, then listSubscribers, then R1's
		// membership check (see the comment in the claimIssue test above for why the dummy exists).
		txMock.then = vi
			.fn()
			.mockImplementationOnce((resolve: any) => resolve(undefined)) // issue_activity insert
			.mockImplementationOnce((resolve: any) => resolve([{ subscriberType: 'user', subscriberId: 'user-2' }]))
			.mockImplementationOnce((resolve: any) => resolve([{ userId: 'user-2' }]))
			// §2.2, decision 25's two reads: the issue row, then one batched grant lookup. No grants.
			.mockImplementationOnce((resolve: any) =>
				resolve([{ projectId: 'proj-1', environment: 'production' }])
			)
			.mockImplementationOnce((resolve: any) => resolve([]))
			.mockImplementationOnce((resolve: any) => resolve(undefined)); // notifyIssueEvent's own notifications insert

		const { issue, notified } = await releaseClaim('issue-1', 'admin-1', { force: true });

		expect(issue).toEqual({ id: 'issue-1', assignedTo: null });
		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({ eventType: 'claim_released', newValue: { force: true } })
		);
		expect(notified).toEqual([{ userId: 'user-2', kind: 'claimed' }]);
		expect(txMock.values).toHaveBeenCalledWith(
			expect.arrayContaining([
				expect.objectContaining({ userId: 'user-2', kind: 'claimed', payload: { released: true, force: true } }),
			])
		);
	});
});

describe('moveIssueToProject', () => {
	it('throws when the issue does not exist, without writing anything', async () => {
		txMock.where = vi.fn(() => txMock);
		txMock.__result = [];

		await expect(moveIssueToProject('missing-issue', 'project-2', 'user', 'user-1')).rejects.toThrow(
			'not found'
		);
	});
});

// R11 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md, §9): author edit/delete of their own report,
// until resolved.
describe('updateManualIssueReport', () => {
	it('writes a report_edited activity row with old/new values and updates both tables', async () => {
		let thenCall = 0;
		txMock.then = vi.fn((resolve: any) => {
			thenCall += 1;
			if (thenCall === 1) return resolve([{ id: 'issue-1', message: 'old title' }]); // existing issue
			if (thenCall === 2) return resolve([{ bodyMd: 'old body', severity: 'low' }]); // existing report
			if (thenCall === 3) return resolve(undefined); // update issues (bare await)
			if (thenCall === 4) return resolve(undefined); // update manual_issue_reports (bare await)
			if (thenCall === 5) return resolve(undefined); // insert issue_activity (bare await)
			if (thenCall === 6) return resolve([{ id: 'issue-1', message: 'new title' }]); // updated issue
			return resolve([{ issueId: 'issue-1', bodyMd: 'new body', severity: 'high' }]); // updated report
		});

		const result = await updateManualIssueReport({
			issueId: 'issue-1',
			actorId: 'author-1',
			title: 'new title',
			bodyMd: 'new body',
			severity: 'high',
		});

		expect(result.issue).toEqual({ id: 'issue-1', message: 'new title' });
		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({
				issueId: 'issue-1',
				eventType: 'report_edited',
				actorType: 'user',
				actorId: 'author-1',
				oldValue: { title: 'old title', bodyMd: 'old body', severity: 'low' },
				newValue: { title: 'new title', bodyMd: 'new body', severity: 'high' },
			})
		);
	});

	it('throws when the issue does not exist', async () => {
		txMock.then = vi.fn((resolve: any) => resolve([]));

		await expect(
			updateManualIssueReport({ issueId: 'missing', actorId: 'author-1', bodyMd: 'x' })
		).rejects.toThrow('not found');
	});

	// N7e (A09): the agent severity op passes actorType: 'agent' so the report_edited activity row
	// attributes the change correctly instead of the hardcoded 'user' this function used to write
	// unconditionally.
	it('attributes the report_edited row to actorType "agent" when the caller passes one', async () => {
		let thenCall = 0;
		txMock.then = vi.fn((resolve: any) => {
			thenCall += 1;
			if (thenCall === 1) return resolve([{ id: 'issue-1', message: 'title' }]);
			if (thenCall === 2) return resolve([{ bodyMd: 'body', severity: 'low' }]);
			if (thenCall === 3) return resolve(undefined); // update manual_issue_reports
			if (thenCall === 4) return resolve(undefined); // insert issue_activity
			if (thenCall === 5) return resolve([{ id: 'issue-1', message: 'title' }]);
			return resolve([{ issueId: 'issue-1', bodyMd: 'body', severity: 'high' }]);
		});

		await updateManualIssueReport({
			issueId: 'issue-1',
			actorId: 'agent-1',
			actorType: 'agent',
			severity: 'high',
		});

		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({ eventType: 'report_edited', actorType: 'agent', actorId: 'agent-1' })
		);
	});
});

describe('deleteManualIssue', () => {
	it('collects attachment storage keys (direct and via comments) before deleting the issue', async () => {
		let thenCall = 0;
		txMock.then = vi.fn((resolve: any) => {
			thenCall += 1;
			if (thenCall === 1) return resolve([{ id: 'issue-1' }]); // existing issue
			if (thenCall === 2) return resolve([{ id: 'comment-1' }]); // comment ids
			if (thenCall === 3) return resolve([{ storageKey: 'direct-key' }]); // direct attachments
			if (thenCall === 4) return resolve([{ storageKey: 'comment-key' }]); // comment attachments
			return resolve([]); // the delete itself
		});

		const result = await deleteManualIssue('issue-1');

		expect(result.storageKeys).toEqual(['direct-key', 'comment-key']);
		expect(txMock.delete).toHaveBeenCalled();
	});

	it('throws when the issue does not exist, without deleting anything', async () => {
		txMock.then = vi.fn((resolve: any) => resolve([]));

		await expect(deleteManualIssue('missing')).rejects.toThrow('not found');
	});
});

describe('createManualIssue — the environment it writes (§4.8a)', () => {
	function queueInserts() {
		let call = 0;
		txMock.returning = vi.fn(() => {
			call += 1;
			if (call === 1) return Promise.resolve([{ id: 'issue-1', projectId: 'project-1' }]);
			if (call === 2) return Promise.resolve([{ issueId: 'issue-1', reporterId: 'user-1' }]);
			return Promise.resolve([]);
		});
	}

	const input = {
		organizationId: 'org-1',
		projectId: 'project-1',
		reporterId: 'user-1',
		title: 'Cannot log in',
		bodyMd: 'Steps to reproduce...',
		severity: 'high' as const,
	};

	it.each<[string | null | undefined, string]>([
		[undefined, 'production'],
		[null, 'production'],
		['', 'production'],
		['eu-west-1', 'eu-west-1'],
		// The route rejects this one with a 400 before it can get here; the writer still resolves it
		// rather than passing a value the NOT NULL column would take and no reader could ever filter.
		['Prod!', 'production'],
	])('writes %j as %j on the issues row', async (asserted, expected) => {
		queueInserts();

		await createManualIssue({ ...input, environment: asserted });

		expect(txMock.values).toHaveBeenCalledWith(
			expect.objectContaining({ projectId: 'project-1', environment: expected })
		);
	});
});
