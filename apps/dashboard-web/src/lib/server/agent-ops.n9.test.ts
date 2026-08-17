import { describe, it, expect, vi, beforeEach } from 'vitest';
import { IdempotencyKeyOpMismatchError } from '$lib/server/agent-idempotency';

/**
 * N9 (docs/plans/AGENT_WORKER_PLAN.md C4/C5, D21): idempotency-key threading through the
 * `issues.comment` and `issues.progress` ops (which back BOTH the single routes AND POST
 * /api/agent/batch). The query-layer dedupe logic itself is proven in
 * queries/comments.idempotency.test.ts; here we assert the OP layer: it forwards the key, surfaces
 * `deduplicated` on the body, never sends a second email on a replay (notified is []), rejects a
 * cross-op key reuse as 409, and 400s a malformed key.
 */

const resolveAgentIssueScope = vi.fn();
vi.mock('$lib/server/agent-issue-scope', () => ({ resolveAgentIssueScope }));

const writeAgentAuditLog = vi.fn();
vi.mock('$lib/server/agent-audit', () => ({ writeAgentAuditLog }));

const sendIssueNotificationEmails = vi.fn();
vi.mock('$lib/server/notify', () => ({ sendIssueNotificationEmails }));

vi.mock('$lib/server/issue-access', () => ({ validateResolvedInVersion: vi.fn((v: unknown) => v ?? null) }));

vi.mock('$lib/db/queries/issues', () => ({
	updateIssueStatus: vi.fn(),
	createIssueRelation: vi.fn(),
	deleteIssueRelation: vi.fn(),
	RelationCycleError: class RelationCycleError extends Error {},
}));

vi.mock('$lib/db/queries/reports', () => ({
	claimIssue: vi.fn(),
	releaseClaim: vi.fn(),
	updateManualIssueReport: vi.fn(),
	ClaimConflictError: class ClaimConflictError extends Error {},
}));

class CommentValidationError extends Error {}
class CommentNotFoundError extends Error {}
const createComment = vi.fn();
vi.mock('$lib/db/queries/comments', () => ({
	createComment,
	editComment: vi.fn(),
	deleteComment: vi.fn(),
	getCommentById: vi.fn(),
	CommentValidationError,
	CommentNotFoundError,
}));

const recordAgentProgress = vi.fn();
vi.mock('$lib/db/queries/agent-work', () => ({ recordAgentProgress }));

const { runAgentOp } = await import('./agent-ops');

const CTX = { agentId: 'agent-1', organizationId: 'org-1', agentName: 'bot', keyPrefixForAudit: 'abc' };
const SCOPE = {
	issueId: 'issue-1',
	projectId: 'proj-1',
	organizationId: 'org-1',
	issueType: 'user_report',
	assignedTo: null,
	assigneeType: null,
	waitingOn: null,
};

beforeEach(() => {
	vi.clearAllMocks();
	resolveAgentIssueScope.mockResolvedValue(SCOPE);
});

describe('N9 (D21): issues.comment idempotency', () => {
	it('forwards the key and, on a fresh write, returns no deduplicated flag and emails once', async () => {
		createComment.mockResolvedValue({ comment: { id: 'c1' }, notified: [{ userId: 'u1' }], deduplicated: false });

		const res = await runAgentOp(
			'issues.comment',
			CTX as any,
			'issue-1',
			{ body_md: 'hello', idempotency_key: 'job-1' },
			'http://localhost'
		);

		expect(createComment).toHaveBeenCalledWith(expect.objectContaining({ idempotencyKey: 'job-1' }));
		expect(res.status).toBe(201);
		expect(res.body).toEqual({ comment: { id: 'c1' } });
		expect(sendIssueNotificationEmails).toHaveBeenCalledWith([{ userId: 'u1' }], expect.anything());
	});

	it('on a replay surfaces deduplicated:true and sends NO second email (notified empty)', async () => {
		createComment.mockResolvedValue({ comment: { id: 'c1' }, notified: [], deduplicated: true });

		const res = await runAgentOp(
			'issues.comment',
			CTX as any,
			'issue-1',
			{ body_md: 'hello', idempotency_key: 'job-1' },
			'http://localhost'
		);

		expect(res.body).toEqual({ comment: { id: 'c1' }, deduplicated: true });
		expect(sendIssueNotificationEmails).toHaveBeenCalledWith([], expect.anything());
	});

	it('409s when the query layer reports a cross-op key reuse', async () => {
		createComment.mockRejectedValue(new IdempotencyKeyOpMismatchError('issues.question', 'issues.comment'));

		await expect(
			runAgentOp('issues.comment', CTX as any, 'issue-1', { body_md: 'x', idempotency_key: 'shared' }, 'http://localhost')
		).rejects.toMatchObject({ status: 409 });
	});

	it('400s a blank idempotency_key without touching the query layer', async () => {
		await expect(
			runAgentOp('issues.comment', CTX as any, 'issue-1', { body_md: 'x', idempotency_key: '   ' }, 'http://localhost')
		).rejects.toMatchObject({ status: 400 });
		expect(createComment).not.toHaveBeenCalled();
	});
});

describe('N9 (D21): issues.progress idempotency', () => {
	it('forwards the key and surfaces deduplicated:true on a replay', async () => {
		recordAgentProgress.mockResolvedValue({ notified: [], deduplicated: true });

		const res = await runAgentOp(
			'issues.progress',
			CTX as any,
			'issue-1',
			{ message_md: 'still working', idempotency_key: 'job-2' },
			'http://localhost'
		);

		expect(recordAgentProgress).toHaveBeenCalledWith('issue-1', 'agent-1', 'still working', 'job-2');
		expect(res.status).toBe(201);
		expect(res.body).toEqual({ success: true, deduplicated: true });
	});

	it('omits the flag on a fresh write', async () => {
		recordAgentProgress.mockResolvedValue({ notified: [], deduplicated: false });

		const res = await runAgentOp(
			'issues.progress',
			CTX as any,
			'issue-1',
			{ message_md: 'working', idempotency_key: 'job-3' },
			'http://localhost'
		);

		expect(res.body).toEqual({ success: true });
	});
});
