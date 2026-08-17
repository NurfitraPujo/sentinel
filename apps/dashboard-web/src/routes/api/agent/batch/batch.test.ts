import { describe, it, expect, vi, beforeEach } from 'vitest';
import { error } from '@sveltejs/kit';

/**
 * N2 (AI-agent-native plan): POST /api/agent/batch. Mocks the same dependency surface the
 * agent-ops.ts handlers use (auth, scope resolution, audit, and each underlying db query/notify
 * function) so this exercises the REAL op dispatch/error-mapping/stopOnError logic, not a stub.
 */

const authenticateAgentRequest = vi.fn();
vi.mock('$lib/server/agent-auth', () => ({ authenticateAgentRequest }));

const resolveAgentIssueScope = vi.fn();
vi.mock('$lib/server/agent-issue-scope', () => ({ resolveAgentIssueScope }));

const writeAgentAuditLog = vi.fn();
vi.mock('$lib/server/agent-audit', () => ({ writeAgentAuditLog }));

const sendIssueNotificationEmails = vi.fn();
vi.mock('$lib/server/notify', () => ({ sendIssueNotificationEmails }));

const validateResolvedInVersion = vi.fn((v: unknown) => v ?? null);
vi.mock('$lib/server/issue-access', () => ({ validateResolvedInVersion }));

const updateIssueStatus = vi.fn();
const createIssueRelation = vi.fn();
const deleteIssueRelation = vi.fn();
class RelationCycleError extends Error {}
vi.mock('$lib/db/queries/issues', () => ({
	updateIssueStatus,
	createIssueRelation,
	deleteIssueRelation,
	RelationCycleError,
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

const recordAgentProgress = vi.fn();
vi.mock('$lib/db/queries/agent-work', () => ({ recordAgentProgress }));

const { POST } = await import('./+server');

function makeEvent(payload: unknown) {
	return {
		request: new Request('http://localhost/api/agent/batch', {
			method: 'POST',
			body: JSON.stringify(payload),
		}),
		url: new URL('http://localhost/api/agent/batch'),
		params: {},
	} as any;
}

const CTX = { agentId: 'agent-1', organizationId: 'org-1', agentName: 'bot', keyPrefixForAudit: 'abc' };

beforeEach(() => {
	vi.clearAllMocks();
	authenticateAgentRequest.mockResolvedValue(CTX);
	resolveAgentIssueScope.mockImplementation(async (issueId: string) => {
		if (issueId === 'other-org-issue') error(404, 'Issue not found');
		return {
			issueId,
			projectId: 'proj-1',
			organizationId: 'org-1',
			issueType: 'system_error',
			assignedTo: null,
			assigneeType: null,
			waitingOn: null,
		};
	});
	sendIssueNotificationEmails.mockResolvedValue(undefined);
	updateIssueStatus.mockResolvedValue({ changed: true, notified: [] });
	recordAgentProgress.mockResolvedValue(undefined);
	createComment.mockResolvedValue({ comment: { id: 'c1' }, notified: [] });
	claimIssue.mockResolvedValue({ issue: { id: 'issue-1', assignedTo: 'agent-1' }, notified: [] });
	releaseClaim.mockResolvedValue({ issue: { id: 'issue-1', assignedTo: null }, notified: [] });
	createIssueRelation.mockResolvedValue({ relation: { id: 'rel-1' }, notified: [] });
	deleteIssueRelation.mockResolvedValue(true);
});

describe('POST /api/agent/batch', () => {
	it('runs a multi-op happy path sequentially and reports completed count', async () => {
		const res = await POST(
			makeEvent({
				operations: [
					{ op: 'issues.status', issueId: 'issue-1', params: { status: 'resolved' } },
					{ op: 'issues.progress', issueId: 'issue-1', params: { message_md: 'working on it' } },
					{ op: 'issues.comment', issueId: 'issue-1', params: { body_md: 'done' } },
				],
			})
		);

		expect(res.status).toBe(200);
		const data = await res.json();
		expect(data.completed).toBe(3);
		expect(data.results).toHaveLength(3);
		expect(data.results[0]).toMatchObject({ ok: true, status: 200 });
		expect(data.results[1]).toMatchObject({ ok: true, status: 201 });
		expect(data.results[2]).toMatchObject({ ok: true, status: 201 });
		expect(updateIssueStatus).toHaveBeenCalledWith('issue-1', 'resolved', undefined, 'agent', 'agent-1');
		expect(recordAgentProgress).toHaveBeenCalledWith('issue-1', 'agent-1', 'working on it');
		expect(createComment).toHaveBeenCalledTimes(1);
		expect(writeAgentAuditLog).toHaveBeenCalledTimes(3);
	});

	it('stopOnError true (default) stops after the first failure and marks the rest skipped', async () => {
		claimIssue.mockRejectedValue(new ClaimConflictError('Issue is already claimed'));

		const res = await POST(
			makeEvent({
				operations: [
					{ op: 'issues.claim', issueId: 'issue-1' },
					{ op: 'issues.progress', issueId: 'issue-1', params: { message_md: 'never runs' } },
				],
			})
		);

		expect(res.status).toBe(200);
		const data = await res.json();
		expect(data.completed).toBe(0);
		expect(data.results[0]).toMatchObject({ ok: false, status: 409 });
		expect(data.results[1]).toMatchObject({ ok: false, skipped: true });
		expect(recordAgentProgress).not.toHaveBeenCalled();
	});

	it('stopOnError false runs every op even after a failure', async () => {
		claimIssue.mockRejectedValue(new ClaimConflictError('Issue is already claimed'));

		const res = await POST(
			makeEvent({
				stopOnError: false,
				operations: [
					{ op: 'issues.claim', issueId: 'issue-1' },
					{ op: 'issues.progress', issueId: 'issue-1', params: { message_md: 'runs anyway' } },
				],
			})
		);

		expect(res.status).toBe(200);
		const data = await res.json();
		expect(data.completed).toBe(1);
		expect(data.results[0]).toMatchObject({ ok: false, status: 409 });
		expect(data.results[1]).toMatchObject({ ok: true, status: 201 });
		expect(recordAgentProgress).toHaveBeenCalledTimes(1);
	});

	it('per-op 409 on claim conflict does not fail the whole request', async () => {
		claimIssue.mockRejectedValue(new ClaimConflictError('Issue is already claimed'));

		const res = await POST(makeEvent({ operations: [{ op: 'issues.claim', issueId: 'issue-1' }] }));

		expect(res.status).toBe(200);
		const data = await res.json();
		expect(data.results[0]).toMatchObject({ ok: false, status: 409, error: 'Issue is already claimed' });
	});

	it('404s per-op for a cross-org issueId without failing the request (B7)', async () => {
		const res = await POST(
			makeEvent({
				operations: [{ op: 'issues.progress', issueId: 'other-org-issue', params: { message_md: 'x' } }],
			})
		);

		expect(res.status).toBe(200);
		const data = await res.json();
		expect(data.completed).toBe(0);
		expect(data.results[0]).toMatchObject({ ok: false, status: 404 });
		expect(resolveAgentIssueScope).toHaveBeenCalledWith('other-org-issue', 'org-1');
	});

	it('reports an unknown op as a per-op 400, not a request-level failure', async () => {
		const res = await POST(
			makeEvent({
				operations: [{ op: 'issues.nonexistent', issueId: 'issue-1' }],
			})
		);

		expect(res.status).toBe(200);
		const data = await res.json();
		expect(data.results[0]).toMatchObject({ ok: false, status: 400 });
		expect(data.results[0].error).toMatch(/Unknown op/);
	});

	it('rejects more than 20 operations with a request-level 400', async () => {
		const operations = Array.from({ length: 21 }, () => ({ op: 'issues.progress', issueId: 'issue-1', params: {} }));
		const res = await POST(makeEvent({ operations }));

		expect(res.status).toBe(400);
	});

	it('rejects an empty operations array with a request-level 400', async () => {
		const res = await POST(makeEvent({ operations: [] }));
		expect(res.status).toBe(400);
	});

	it('rejects a malformed envelope (missing operations) with a request-level 400', async () => {
		const res = await POST(makeEvent({ notOperations: [] }));
		expect(res.status).toBe(400);
	});

	it('rejects a malformed operation entry (missing op/issueId) with a request-level 400', async () => {
		const res = await POST(makeEvent({ operations: [{ issueId: 'issue-1' }] }));
		expect(res.status).toBe(400);
	});

	it('propagates auth failure as a real 401, not a per-op result', async () => {
		authenticateAgentRequest.mockRejectedValue(Object.assign(new Error('Unauthorized'), { status: 401 }));

		await expect(POST(makeEvent({ operations: [{ op: 'issues.progress', issueId: 'issue-1' }] }))).rejects.toMatchObject(
			{ status: 401 }
		);
	});
});
