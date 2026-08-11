import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * Manual Issues M2 (docs/plans/MANUAL_ISSUES_DESIGN.md §4) -- co-located, shuffle-safe unit
 * tests for `claimDraftAttachments` and `getAttachmentById`. A dedicated, purpose-built mock
 * (rather than reuse of reports.test.ts's shared chainable double) because this function issues
 * a DIFFERENT select().where() per candidate id followed by a conditional update().returning(),
 * and the results need to vary per attachment id within a single call -- a single shared
 * `__result` slot can't express that.
 */

interface SelectQueueEntry {
	id: string;
	orgId: string;
	uploaderId: string;
	issueId: string | null;
	commentId: string | null;
}

vi.mock('$lib/server/db', () => ({ db: { select: vi.fn(), transaction: vi.fn() } }));

vi.mock('$lib/db/schema', () => ({
	issues: { id: 'id', projectId: 'projectId', assignedTo: 'assignedTo', issueType: 'issueType' },
	issueActivity: { id: 'id', issueId: 'issueId' },
	manualIssueReports: { issueId: 'issueId', reporterId: 'reporterId' },
	projects: { id: 'id', organizationId: 'organizationId', isInbox: 'isInbox', name: 'name' },
	users: { id: 'id', name: 'name', email: 'email' },
	attachments: {
		id: 'id',
		orgId: 'orgId',
		issueId: 'issueId',
		commentId: 'commentId',
		uploaderId: 'uploaderId',
		uploaderType: 'uploaderType',
		storageKey: 'storageKey',
		filename: 'filename',
		contentType: 'contentType',
		sizeBytes: 'sizeBytes',
	},
}));

vi.mock('./issues', () => ({ getIssueActivity: vi.fn() }));

const { claimDraftAttachments } = await import('./reports');

beforeEach(() => {
	vi.clearAllMocks();
});

function makeCorrectTx() {
	const selectQueue: (SelectQueueEntry | undefined)[] = [];
	const updateQueue: { id: string }[][] = [];
	return {
		selectQueue,
		updateQueue,
		tx: {
			select: vi.fn(() => ({
				from: vi.fn(() => ({
					where: vi.fn(() => {
						const row = selectQueue.shift();
						return Promise.resolve(row ? [row] : []);
					}),
				})),
			})),
			update: vi.fn(() => ({
				set: vi.fn(() => ({
					where: vi.fn(() => ({
						returning: vi.fn(() => Promise.resolve(updateQueue.shift() ?? [])),
					})),
				})),
			})),
		},
	};
}

describe('claimDraftAttachments', () => {
	it('claims a draft attachment that matches org, uploader, and is still unlinked', async () => {
		const { tx, selectQueue, updateQueue } = makeCorrectTx();
		selectQueue.push({
			id: 'att-1',
			orgId: 'org-1',
			uploaderId: 'user-1',
			issueId: null,
			commentId: null,
		});
		updateQueue.push([{ id: 'att-1' }]);

		const result = await claimDraftAttachments(tx, ['att-1'], 'issue-1', 'user-1', 'org-1');

		expect(result).toEqual([{ id: 'att-1' }]);
		expect(tx.update).toHaveBeenCalledTimes(1);
	});

	it('skips an attachment belonging to a different org (B7) without claiming it', async () => {
		const { tx, selectQueue } = makeCorrectTx();
		selectQueue.push({
			id: 'att-1',
			orgId: 'org-OTHER',
			uploaderId: 'user-1',
			issueId: null,
			commentId: null,
		});

		const result = await claimDraftAttachments(tx, ['att-1'], 'issue-1', 'user-1', 'org-1');

		expect(result).toEqual([]);
		expect(tx.update).not.toHaveBeenCalled();
	});

	it('skips an attachment uploaded by a different user', async () => {
		const { tx, selectQueue } = makeCorrectTx();
		selectQueue.push({
			id: 'att-1',
			orgId: 'org-1',
			uploaderId: 'user-OTHER',
			issueId: null,
			commentId: null,
		});

		const result = await claimDraftAttachments(tx, ['att-1'], 'issue-1', 'user-1', 'org-1');

		expect(result).toEqual([]);
		expect(tx.update).not.toHaveBeenCalled();
	});

	it('skips an attachment that is already linked to another issue', async () => {
		const { tx, selectQueue } = makeCorrectTx();
		selectQueue.push({
			id: 'att-1',
			orgId: 'org-1',
			uploaderId: 'user-1',
			issueId: 'issue-ALREADY',
			commentId: null,
		});

		const result = await claimDraftAttachments(tx, ['att-1'], 'issue-1', 'user-1', 'org-1');

		expect(result).toEqual([]);
		expect(tx.update).not.toHaveBeenCalled();
	});

	it('skips a nonexistent attachment id silently', async () => {
		const { tx, selectQueue } = makeCorrectTx();
		selectQueue.push(undefined);

		const result = await claimDraftAttachments(tx, ['att-missing'], 'issue-1', 'user-1', 'org-1');

		expect(result).toEqual([]);
		expect(tx.update).not.toHaveBeenCalled();
	});

	it('returns [] immediately for an empty id list without touching tx', async () => {
		const { tx } = makeCorrectTx();

		const result = await claimDraftAttachments(tx, [], 'issue-1', 'user-1', 'org-1');

		expect(result).toEqual([]);
		expect(tx.select).not.toHaveBeenCalled();
	});

	it('processes multiple ids independently, claiming only the valid ones', async () => {
		const { tx, selectQueue, updateQueue } = makeCorrectTx();
		selectQueue.push(
			{ id: 'att-1', orgId: 'org-1', uploaderId: 'user-1', issueId: null, commentId: null },
			{ id: 'att-2', orgId: 'org-OTHER', uploaderId: 'user-1', issueId: null, commentId: null }
		);
		updateQueue.push([{ id: 'att-1' }]);

		const result = await claimDraftAttachments(
			tx,
			['att-1', 'att-2'],
			'issue-1',
			'user-1',
			'org-1'
		);

		expect(result).toEqual([{ id: 'att-1' }]);
		expect(tx.update).toHaveBeenCalledTimes(1);
	});
});
