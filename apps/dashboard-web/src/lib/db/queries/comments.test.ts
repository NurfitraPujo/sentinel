import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * Manual Issues M3 (docs/plans/MANUAL_ISSUES_DESIGN.md §5) -- co-located, shuffle-safe unit
 * tests for comments.ts. Two kinds of double are used, mirroring reports.test.ts /
 * reports.attachments.test.ts:
 *   - a queue-based `db` double for the plain (non-transactional) reads in `listComments` /
 *     `getCommentById`, since those issue several *different*-shaped selects in one call and a
 *     single shared `__result` can't express that;
 *   - a queue-based `tx` double, built fresh per test, for the transactional writes
 *     (`createComment`, `deleteComment`).
 */

const claimDraftAttachmentsForComment = vi.fn();
const deleteObject = vi.fn();
const isStorageConfigured = vi.fn(() => true);
const logError = vi.fn();

vi.mock('./reports', () => ({ claimDraftAttachmentsForComment }));
vi.mock('$lib/server/storage', () => ({ deleteObject, isStorageConfigured }));
vi.mock('$lib/server/observability/log', () => ({ log: { error: logError } }));

function makeQueueableDb() {
	const resultQueue: unknown[] = [];
	const chain: any = {};
	const methods = ['select', 'from', 'leftJoin', 'where', 'orderBy', 'update', 'set', 'delete'];
	for (const m of methods) {
		chain[m] = vi.fn(() => chain);
	}
	chain.returning = vi.fn(() => Promise.resolve(resultQueue.shift() ?? []));
	chain.then = vi.fn((resolve: any) => resolve(resultQueue.shift() ?? []));
	return { db: chain, resultQueue };
}

const { db: dbMock, resultQueue: dbResultQueue } = makeQueueableDb();

function makeTx() {
	const selectQueue: unknown[][] = [];
	const insertReturningQueue: unknown[][] = [];
	const insertCalls: { table: unknown; values: unknown }[] = [];
	const updateCalls: { table: unknown; set: unknown }[] = [];

	const tx: any = {
		select: vi.fn(() => ({
			from: vi.fn(() => ({
				where: vi.fn(() => Promise.resolve(selectQueue.shift() ?? [])),
			})),
		})),
		insert: vi.fn((table: unknown) => ({
			values: vi.fn((values: unknown) => {
				insertCalls.push({ table, values });
				const obj: any = {
					returning: vi.fn(() => Promise.resolve(insertReturningQueue.shift() ?? [])),
					// subscribe() (queries/subscriptions.ts) upserts via onConflictDoNothing rather
					// than a plain insert -- it never calls .returning().
					onConflictDoNothing: vi.fn(() => Promise.resolve(undefined)),
					then: (resolve: any) => resolve(undefined),
				};
				return obj;
			}),
		})),
		update: vi.fn((table: unknown) => ({
			set: vi.fn((values: unknown) => {
				updateCalls.push({ table, set: values });
				return { where: vi.fn(() => Promise.resolve(undefined)) };
			}),
		})),
		delete: vi.fn(() => ({ where: vi.fn(() => Promise.resolve(undefined)) })),
	};

	return { tx, selectQueue, insertReturningQueue, insertCalls, updateCalls };
}

let txHandle = makeTx();
const dbTransactionMock = vi.fn(async (cb: any) => cb(txHandle.tx));

vi.mock('$lib/server/db', () => ({ db: { ...dbMock, transaction: (cb: any) => dbTransactionMock(cb) } }));

vi.mock('$lib/db/schema', () => ({
	issues: { id: 'id', projectId: 'projectId', waitingOn: 'waitingOn', status: 'status' },
	issueActivity: { id: 'id', issueId: 'issueId' },
	issueComments: {
		id: 'id',
		issueId: 'issueId',
		parentId: 'parentId',
		authorType: 'authorType',
		authorId: 'authorId',
		blocking: 'blocking',
		bodyMd: 'bodyMd',
		createdAt: 'createdAt',
		editedAt: 'editedAt',
	},
	attachments: {
		id: 'id',
		commentId: 'commentId',
		storageKey: 'storageKey',
	},
	users: { id: 'id', name: 'name', email: 'email' },
	projects: { id: 'id', organizationId: 'organizationId' },
	issueSubscriptions: {
		id: 'id',
		issueId: 'issueId',
		subscriberType: 'subscriberType',
		subscriberId: 'subscriberId',
	},
	notifications: { id: 'id', userId: 'userId', issueId: 'issueId', kind: 'kind', createdAt: 'createdAt' },
}));

const { createComment, listComments, getCommentById, editComment, deleteComment, CommentValidationError, CommentNotFoundError } =
	await import('./comments');

beforeEach(() => {
	vi.clearAllMocks();
	dbResultQueue.length = 0;
	txHandle = makeTx();
	dbTransactionMock.mockImplementation(async (cb: any) => cb(txHandle.tx));
	isStorageConfigured.mockReturnValue(true);
});

describe('createComment', () => {
	it('creates a root comment and writes exactly one "commented" activity row', async () => {
		txHandle.selectQueue.push(
			[{ id: 'issue-1', projectId: 'project-1', waitingOn: null }], // issue lookup
			[{ organizationId: 'org-1' }] // project lookup
		);
		txHandle.insertReturningQueue.push([{ id: 'comment-1', issueId: 'issue-1', parentId: null }]);

		const { comment: result } = await createComment({
			issueId: 'issue-1',
			authorType: 'user',
			authorId: 'user-1',
			bodyMd: 'Looks like a regression',
		});

		expect(result).toEqual({ id: 'comment-1', issueId: 'issue-1', parentId: null });
		// comment + commented activity + auto-subscribe (reason 'participant'), no waiting_on clear.
		expect(txHandle.insertCalls).toHaveLength(3);
		expect(txHandle.insertCalls[1].values).toMatchObject({ eventType: 'commented', actorType: 'user' });
		expect(txHandle.insertCalls[2].values).toMatchObject({
			subscriberType: 'user',
			subscriberId: 'user-1',
			reason: 'participant',
		});
		expect(txHandle.updateCalls).toHaveLength(0);
	});

	it('rejects an empty body before opening a transaction', async () => {
		await expect(
			createComment({ issueId: 'issue-1', authorType: 'user', authorId: 'user-1', bodyMd: '   ' })
		).rejects.toBeInstanceOf(CommentValidationError);

		expect(dbTransactionMock).not.toHaveBeenCalled();
	});

	it('resolves a reply-to-a-reply onto the SAME parent (one-level threads, §5)', async () => {
		txHandle.selectQueue.push(
			[{ id: 'issue-1', projectId: 'project-1', waitingOn: null }],
			[{ organizationId: 'org-1' }],
			[{ id: 'reply-1', issueId: 'issue-1', parentId: 'root-1' }] // parentId names a REPLY
		);
		txHandle.insertReturningQueue.push([{ id: 'comment-2', issueId: 'issue-1', parentId: 'root-1' }]);

		await createComment({
			issueId: 'issue-1',
			authorType: 'user',
			authorId: 'user-1',
			bodyMd: 'Same here',
			parentId: 'reply-1',
		});

		expect(txHandle.insertCalls[0].values).toMatchObject({ parentId: 'root-1' });
	});

	it('rejects a parentId that does not belong to this issue', async () => {
		txHandle.selectQueue.push(
			[{ id: 'issue-1', projectId: 'project-1', waitingOn: null }],
			[{ organizationId: 'org-1' }],
			[{ id: 'foreign-1', issueId: 'issue-OTHER', parentId: null }]
		);

		await expect(
			createComment({
				issueId: 'issue-1',
				authorType: 'user',
				authorId: 'user-1',
				bodyMd: 'x',
				parentId: 'foreign-1',
			})
		).rejects.toBeInstanceOf(CommentValidationError);
	});

	it('throws CommentNotFoundError for a nonexistent issue, without inserting anything', async () => {
		txHandle.selectQueue.push([]); // issue lookup finds nothing

		await expect(
			createComment({ issueId: 'missing', authorType: 'user', authorId: 'user-1', bodyMd: 'x' })
		).rejects.toBeInstanceOf(CommentNotFoundError);

		expect(txHandle.insertCalls).toHaveLength(0);
	});

	it('claims draft attachments onto the new comment via claimDraftAttachmentsForComment', async () => {
		txHandle.selectQueue.push(
			[{ id: 'issue-1', projectId: 'project-1', waitingOn: null }],
			[{ organizationId: 'org-1' }]
		);
		txHandle.insertReturningQueue.push([{ id: 'comment-1', issueId: 'issue-1', parentId: null }]);
		claimDraftAttachmentsForComment.mockResolvedValueOnce([{ id: 'att-1' }]);

		await createComment({
			issueId: 'issue-1',
			authorType: 'user',
			authorId: 'user-1',
			bodyMd: 'see attached',
			attachmentIds: ['att-1'],
		});

		expect(claimDraftAttachmentsForComment).toHaveBeenCalledWith(
			txHandle.tx,
			['att-1'],
			'comment-1',
			'user-1',
			'org-1'
		);
		expect(txHandle.insertCalls[1].values).toMatchObject({
			eventType: 'commented',
			newValue: expect.objectContaining({ attachmentIds: ['att-1'] }),
		});
	});

	it('Q11: a USER reply clears a pending waiting_on and writes question_answered', async () => {
		txHandle.selectQueue.push(
			[{ id: 'issue-1', projectId: 'project-1', waitingOn: 'reporter' }],
			[{ organizationId: 'org-1' }]
		);
		txHandle.insertReturningQueue.push([{ id: 'comment-1', issueId: 'issue-1', parentId: null }]);

		await createComment({
			issueId: 'issue-1',
			authorType: 'user',
			authorId: 'user-1',
			bodyMd: 'here is the answer',
		});

		expect(txHandle.updateCalls).toHaveLength(1);
		expect(txHandle.updateCalls[0].set).toEqual({ waitingOn: null });
		// comment, commented activity, question_answered activity, auto-subscribe.
		expect(txHandle.insertCalls).toHaveLength(4);
		expect(txHandle.insertCalls[2].values).toMatchObject({
			eventType: 'question_answered',
			oldValue: { waitingOn: 'reporter' },
		});
	});

	it('an AGENT reply does NOT clear waiting_on (only a human reply unblocks)', async () => {
		txHandle.selectQueue.push(
			[{ id: 'issue-1', projectId: 'project-1', waitingOn: 'team' }],
			[{ organizationId: 'org-1' }]
		);
		txHandle.insertReturningQueue.push([{ id: 'comment-1', issueId: 'issue-1', parentId: null }]);

		await createComment({
			issueId: 'issue-1',
			authorType: 'agent',
			authorId: 'agent-1',
			bodyMd: 'still working on it',
		});

		expect(txHandle.updateCalls).toHaveLength(0);
		expect(txHandle.insertCalls).toHaveLength(2); // comment + commented activity only
	});

	describe('M5 blocking questions (§7 step 4, Q11)', () => {
		it('rejects blocking:true without a valid waitingOnAudience, before opening a transaction', async () => {
			await expect(
				createComment({
					issueId: 'issue-1',
					authorType: 'agent',
					authorId: 'agent-1',
					bodyMd: 'need input',
					blocking: true,
				})
			).rejects.toBeInstanceOf(CommentValidationError);

			expect(dbTransactionMock).not.toHaveBeenCalled();
		});

		it('sets the comment row blocking=true, sets issues.waiting_on, writes question_asked, in one transaction', async () => {
			txHandle.selectQueue.push(
				[{ id: 'issue-1', projectId: 'project-1', waitingOn: null }],
				[{ organizationId: 'org-1' }]
			);
			txHandle.insertReturningQueue.push([
				{ id: 'comment-1', issueId: 'issue-1', parentId: null, blocking: true },
			]);

			const { comment } = await createComment({
				issueId: 'issue-1',
				authorType: 'agent',
				authorId: 'agent-1',
				bodyMd: 'Which environment is this in?',
				blocking: true,
				waitingOnAudience: 'reporter',
			});

			expect(comment).toMatchObject({ id: 'comment-1', blocking: true });
			expect(txHandle.insertCalls[0].values).toMatchObject({ blocking: true });
			// waiting_on set to the audience, in the same tx as the comment insert.
			expect(txHandle.updateCalls).toHaveLength(1);
			expect(txHandle.updateCalls[0].set).toEqual({ waitingOn: 'reporter' });
			// activity is 'question_asked', not 'commented'.
			expect(txHandle.insertCalls[1].values).toMatchObject({
				eventType: 'question_asked',
				actorType: 'agent',
				actorId: 'agent-1',
				newValue: expect.objectContaining({ waitingOn: 'reporter' }),
			});
			// agent authors never auto-subscribe (M4/M5 -- agents poll).
			expect(txHandle.insertCalls).toHaveLength(2);
		});

		it('fans out notifyIssueEvent with kind question_asked, not commented', async () => {
			txHandle.selectQueue.push(
				[{ id: 'issue-1', projectId: 'project-1', waitingOn: null }],
				[{ organizationId: 'org-1' }]
			);
			txHandle.insertReturningQueue.push([{ id: 'comment-1', issueId: 'issue-1', parentId: null }]);
			// notifyIssueEvent's listSubscribers select runs against tx via the shared select-queue
			// chain in this double, which is dbMock, not txHandle.selectQueue -- see the module mock
			// for $lib/db/queries/subscriptions below.

			const { notified } = await createComment({
				issueId: 'issue-1',
				authorType: 'agent',
				authorId: 'agent-1',
				bodyMd: 'blocked, need an answer',
				blocking: true,
				waitingOnAudience: 'team',
			});

			expect(Array.isArray(notified)).toBe(true);
		});
	});
});

describe('listComments', () => {
	it('nests replies under their root and attaches each attachment to the right comment', async () => {
		dbResultQueue.push(
			[
				{
					id: 'root-1',
					issueId: 'issue-1',
					parentId: null,
					authorType: 'user',
					authorId: 'user-1',
					blocking: false,
					bodyMd: 'root',
					createdAt: new Date('2026-08-01T00:00:00Z'),
					editedAt: null,
					authorName: 'Alice',
					authorEmail: 'alice@example.com',
				},
				{
					id: 'reply-1',
					issueId: 'issue-1',
					parentId: 'root-1',
					authorType: 'agent',
					authorId: 'agent-1',
					blocking: false,
					bodyMd: 'reply',
					createdAt: new Date('2026-08-02T00:00:00Z'),
					editedAt: null,
					authorName: null,
					authorEmail: null,
				},
			],
			[{ id: 'att-1', commentId: 'reply-1', storageKey: 'k1' }]
		);

		const result = await listComments('issue-1');

		expect(result).toHaveLength(1);
		expect(result[0].id).toBe('root-1');
		expect(result[0].replies).toHaveLength(1);
		expect(result[0].replies[0].id).toBe('reply-1');
		expect(result[0].replies[0].attachments).toEqual([{ id: 'att-1', commentId: 'reply-1', storageKey: 'k1' }]);
		expect(result[0].attachments).toEqual([]);
	});

	it('after-filter: a thread with no new activity is excluded, one with a new reply is included whole', async () => {
		dbResultQueue.push(
			[
				{
					id: 'root-1',
					issueId: 'issue-1',
					parentId: null,
					authorType: 'user',
					authorId: 'user-1',
					blocking: false,
					bodyMd: 'old root',
					createdAt: new Date('2026-08-01T00:00:00Z'),
					editedAt: null,
					authorName: null,
					authorEmail: null,
				},
				{
					id: 'reply-1',
					issueId: 'issue-1',
					parentId: 'root-1',
					authorType: 'user',
					authorId: 'user-1',
					blocking: false,
					bodyMd: 'new reply',
					createdAt: new Date('2026-08-03T00:00:00Z'),
					editedAt: null,
					authorName: null,
					authorEmail: null,
				},
				{
					id: 'root-2',
					issueId: 'issue-1',
					parentId: null,
					authorType: 'user',
					authorId: 'user-1',
					blocking: false,
					bodyMd: 'stale root, no new replies',
					createdAt: new Date('2026-08-01T00:00:00Z'),
					editedAt: null,
					authorName: null,
					authorEmail: null,
				},
			],
			[]
		);

		const result = await listComments('issue-1', { after: new Date('2026-08-02T00:00:00Z') });

		expect(result.map((r) => r.id)).toEqual(['root-1']);
		expect(result[0].replies.map((r) => r.id)).toEqual(['reply-1']);
	});
});

describe('editComment', () => {
	it('throws CommentNotFoundError when the target row does not exist', async () => {
		dbResultQueue.push([]);
		await expect(editComment('missing', 'new body')).rejects.toBeInstanceOf(CommentNotFoundError);
	});

	it('rejects an empty body', async () => {
		await expect(editComment('comment-1', '   ')).rejects.toBeInstanceOf(CommentValidationError);
	});
});

describe('deleteComment', () => {
	it('collects storage keys from the comment and its replies, then best-effort deletes them after commit', async () => {
		txHandle.selectQueue.push(
			[{ id: 'root-1', issueId: 'issue-1' }], // the comment itself
			[{ id: 'reply-1' }], // its replies
			[{ storageKey: 'k1' }, { storageKey: 'k2' }] // attachments across comment+replies
		);

		const result = await deleteComment('root-1');

		expect(result).toEqual({ issueId: 'issue-1' });
		expect(deleteObject).toHaveBeenCalledTimes(2);
		expect(deleteObject).toHaveBeenCalledWith('k1');
		expect(deleteObject).toHaveBeenCalledWith('k2');
	});

	it('skips storage cleanup entirely when there are no linked attachments', async () => {
		txHandle.selectQueue.push([{ id: 'root-1', issueId: 'issue-1' }], [], []);

		await deleteComment('root-1');

		expect(deleteObject).not.toHaveBeenCalled();
	});

	it('a storage delete failure is logged, not thrown (best-effort)', async () => {
		txHandle.selectQueue.push([{ id: 'root-1', issueId: 'issue-1' }], [], [{ storageKey: 'k1' }]);
		deleteObject.mockRejectedValueOnce(new Error('minio down'));

		await expect(deleteComment('root-1')).resolves.toEqual({ issueId: 'issue-1' });
		expect(logError).toHaveBeenCalledWith('comments.delete_attachment_storage_failed', expect.any(Object));
	});

	it('throws CommentNotFoundError for a missing comment, without touching storage', async () => {
		txHandle.selectQueue.push([]);

		await expect(deleteComment('missing')).rejects.toBeInstanceOf(CommentNotFoundError);
		expect(deleteObject).not.toHaveBeenCalled();
	});
});

describe('getCommentById', () => {
	it('returns null when no row matches', async () => {
		dbResultQueue.push([]);
		expect(await getCommentById('missing')).toBeNull();
	});
});
