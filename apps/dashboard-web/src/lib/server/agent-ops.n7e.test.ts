import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * N7e (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md): A08 (comment edit/delete) and A09
 * (severity op) op handlers, exercised via `runAgentOp` directly -- same mocking style as
 * batch/batch.test.ts, since these ops are driven by both the single routes AND the batch API.
 */

const resolveAgentIssueScope = vi.fn();
vi.mock('$lib/server/agent-issue-scope', () => ({ resolveAgentIssueScope }));

const writeAgentAuditLog = vi.fn();
vi.mock('$lib/server/agent-audit', () => ({ writeAgentAuditLog }));

const sendIssueNotificationEmails = vi.fn();
vi.mock('$lib/server/notify', () => ({ sendIssueNotificationEmails }));

const validateResolvedInVersion = vi.fn((v: unknown) => v ?? null);
vi.mock('$lib/server/issue-access', () => ({ validateResolvedInVersion }));

vi.mock('$lib/db/queries/issues', () => ({
	updateIssueStatus: vi.fn(),
	createIssueRelation: vi.fn(),
	deleteIssueRelation: vi.fn(),
	RelationCycleError: class RelationCycleError extends Error {},
}));

class ClaimConflictError extends Error {}
const claimIssue = vi.fn();
const releaseClaim = vi.fn();
const updateManualIssueReport = vi.fn();
vi.mock('$lib/db/queries/reports', () => ({
	claimIssue,
	releaseClaim,
	updateManualIssueReport,
	ClaimConflictError,
}));

class CommentValidationError extends Error {}
class CommentNotFoundError extends Error {}
const createComment = vi.fn();
const editComment = vi.fn();
const deleteComment = vi.fn();
const getCommentById = vi.fn();
vi.mock('$lib/db/queries/comments', () => ({
	createComment,
	editComment,
	deleteComment,
	getCommentById,
	CommentValidationError,
	CommentNotFoundError,
}));

vi.mock('$lib/db/queries/agent-work', () => ({ recordAgentProgress: vi.fn() }));

const { runAgentOp } = await import('./agent-ops');

const CTX = { agentId: 'agent-1', organizationId: 'org-1', agentName: 'bot', keyPrefixForAudit: 'abc' };

function issueScope(overrides: Partial<Record<string, unknown>> = {}) {
	return {
		issueId: 'issue-1',
		projectId: 'proj-1',
		organizationId: 'org-1',
		issueType: 'user_report',
		assignedTo: null,
		assigneeType: null,
		waitingOn: null,
		...overrides,
	};
}

beforeEach(() => {
	vi.clearAllMocks();
	resolveAgentIssueScope.mockResolvedValue(issueScope());
});

describe('A08: comments.edit / comments.delete', () => {
	it('edits the calling agent\'s own comment and writes the comment_edited audit action', async () => {
		getCommentById.mockResolvedValue({ id: 'c1', issueId: 'issue-1', authorType: 'agent', authorId: 'agent-1' });
		editComment.mockResolvedValue({ id: 'c1', bodyMd: 'updated' });

		const result = await runAgentOp(
			'comments.edit',
			CTX as any,
			'issue-1',
			{ comment_id: 'c1', body_md: 'updated' },
			'http://localhost'
		);

		expect(result.status).toBe(200);
		expect(result.body).toEqual({ comment: { id: 'c1', bodyMd: 'updated' } });
		expect(editComment).toHaveBeenCalledWith('c1', 'updated');
		expect(result.audit?.action).toBe('agent.issue.comment_edited');
	});

	it('deletes the calling agent\'s own comment and writes the comment_deleted audit action', async () => {
		getCommentById.mockResolvedValue({ id: 'c1', issueId: 'issue-1', authorType: 'agent', authorId: 'agent-1' });
		deleteComment.mockResolvedValue({ issueId: 'issue-1' });

		const result = await runAgentOp('comments.delete', CTX as any, 'issue-1', { comment_id: 'c1' }, 'http://localhost');

		expect(result.status).toBe(200);
		expect(result.body).toEqual({ success: true, issueId: 'issue-1' });
		expect(deleteComment).toHaveBeenCalledWith('c1');
		expect(result.audit?.action).toBe('agent.issue.comment_deleted');
	});

	it('403s editing another AGENT\'s comment', async () => {
		getCommentById.mockResolvedValue({ id: 'c1', issueId: 'issue-1', authorType: 'agent', authorId: 'other-agent' });

		await expect(
			runAgentOp('comments.edit', CTX as any, 'issue-1', { comment_id: 'c1', body_md: 'x' }, 'http://localhost')
		).rejects.toMatchObject({ status: 403 });
		expect(editComment).not.toHaveBeenCalled();
	});

	it('403s deleting a HUMAN\'s comment', async () => {
		getCommentById.mockResolvedValue({ id: 'c1', issueId: 'issue-1', authorType: 'user', authorId: 'user-1' });

		await expect(
			runAgentOp('comments.delete', CTX as any, 'issue-1', { comment_id: 'c1' }, 'http://localhost')
		).rejects.toMatchObject({ status: 403 });
		expect(deleteComment).not.toHaveBeenCalled();
	});

	it('404s a comment belonging to a different issue (cross-issue)', async () => {
		getCommentById.mockResolvedValue({ id: 'c1', issueId: 'other-issue', authorType: 'agent', authorId: 'agent-1' });

		await expect(
			runAgentOp('comments.edit', CTX as any, 'issue-1', { comment_id: 'c1', body_md: 'x' }, 'http://localhost')
		).rejects.toMatchObject({ status: 404 });
	});

	it('404s a comment that no longer exists (e.g. already deleted)', async () => {
		getCommentById.mockResolvedValue(null);

		await expect(
			runAgentOp('comments.delete', CTX as any, 'issue-1', { comment_id: 'gone' }, 'http://localhost')
		).rejects.toMatchObject({ status: 404 });
	});

	it('400s when comment_id is missing', async () => {
		await expect(
			runAgentOp('comments.edit', CTX as any, 'issue-1', { body_md: 'x' }, 'http://localhost')
		).rejects.toMatchObject({ status: 400 });
	});
});

describe('A09: issues.report.severity', () => {
	it('updates severity via updateManualIssueReport with actorType agent, and writes the audit action', async () => {
		updateManualIssueReport.mockResolvedValue({ report: { severity: 'high' } });

		const result = await runAgentOp(
			'issues.report.severity',
			CTX as any,
			'issue-1',
			{ severity: 'high' },
			'http://localhost'
		);

		expect(result.status).toBe(200);
		expect(result.body).toEqual({ success: true, severity: 'high' });
		expect(updateManualIssueReport).toHaveBeenCalledWith({
			issueId: 'issue-1',
			actorId: 'agent-1',
			actorType: 'agent',
			severity: 'high',
		});
		expect(result.audit?.action).toBe('agent.issue.report_severity_changed');
	});

	it('400s on a system_error issue (guards the issueType !== user_report check)', async () => {
		resolveAgentIssueScope.mockResolvedValue(issueScope({ issueType: 'system_error' }));

		await expect(
			runAgentOp('issues.report.severity', CTX as any, 'issue-1', { severity: 'high' }, 'http://localhost')
		).rejects.toMatchObject({ status: 400 });
		expect(updateManualIssueReport).not.toHaveBeenCalled();
	});

	it('400s on an invalid severity value', async () => {
		await expect(
			runAgentOp('issues.report.severity', CTX as any, 'issue-1', { severity: 'urgent' }, 'http://localhost')
		).rejects.toMatchObject({ status: 400 });
		expect(updateManualIssueReport).not.toHaveBeenCalled();
	});
});

// Sanity-check for the batch registry contract: both A08 ops and the severity op must be
// reachable through the same dispatch table batch.ts uses. If either falls out of `agentOps`
// (agent-ops.ts), this fails instead of the route silently 400ing "Unknown op".
describe('batch inclusion', () => {
	it('routes comments.edit, comments.delete, and issues.report.severity through runAgentOp without UnknownAgentOpError', async () => {
		getCommentById.mockResolvedValue({ id: 'c1', issueId: 'issue-1', authorType: 'agent', authorId: 'agent-1' });
		editComment.mockResolvedValue({ id: 'c1' });
		deleteComment.mockResolvedValue({ issueId: 'issue-1' });
		updateManualIssueReport.mockResolvedValue({ report: { severity: 'low' } });

		await expect(
			runAgentOp('comments.edit', CTX as any, 'issue-1', { comment_id: 'c1', body_md: 'x' }, 'http://localhost')
		).resolves.toBeTruthy();
		await expect(
			runAgentOp('comments.delete', CTX as any, 'issue-1', { comment_id: 'c1' }, 'http://localhost')
		).resolves.toBeTruthy();
		await expect(
			runAgentOp('issues.report.severity', CTX as any, 'issue-1', { severity: 'low' }, 'http://localhost')
		).resolves.toBeTruthy();
	});
});
