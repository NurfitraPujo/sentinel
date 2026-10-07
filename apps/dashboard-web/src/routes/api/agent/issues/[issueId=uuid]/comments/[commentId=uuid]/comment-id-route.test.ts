import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * A08 (N7e): route-level test for PATCH/DELETE .../comments/[commentId] -- asserts the
 * `commentId` URL param is folded into `params.comment_id` before `runAgentOp` runs, since that
 * is this route's whole reason for NOT using `agentOpRoute` directly (see the route file's own
 * doc comment).
 */

const authenticateAgentRequest = vi.fn();
vi.mock('$lib/server/agent-auth', () => ({ authenticateAgentRequest }));

const runAgentOp = vi.fn();
vi.mock('$lib/server/agent-ops', () => ({ runAgentOp }));

const writeAgentAuditLog = vi.fn();
vi.mock('$lib/server/agent-audit', () => ({ writeAgentAuditLog }));

const { PATCH, DELETE } = await import('./+server');

const CTX = { agentId: 'agent-1', organizationId: 'org-1', agentName: 'bot', keyPrefixForAudit: 'abc' };

function makeEvent(method: string, body?: unknown) {
	return {
		request: new Request('http://localhost/api/agent/issues/issue-1/comments/c1', {
			method,
			...(body !== undefined ? { body: JSON.stringify(body) } : {}),
		}),
		url: new URL('http://localhost/api/agent/issues/issue-1/comments/c1'),
		params: { issueId: 'issue-1', commentId: 'c1' },
	} as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	authenticateAgentRequest.mockResolvedValue(CTX);
});

describe('PATCH .../comments/[commentId]', () => {
	it('calls comments.edit with comment_id folded into params from the URL', async () => {
		runAgentOp.mockResolvedValue({ status: 200, body: { comment: { id: 'c1' } } });

		const res = await PATCH(makeEvent('PATCH', { body_md: 'updated' }));

		expect(res.status).toBe(200);
		expect(runAgentOp).toHaveBeenCalledWith(
			'comments.edit',
			CTX,
			'issue-1',
			{ body_md: 'updated', comment_id: 'c1' },
			'http://localhost'
		);
	});

	it('writes the audit log when the op returns one', async () => {
		runAgentOp.mockResolvedValue({
			status: 200,
			body: { comment: { id: 'c1' } },
			audit: { action: 'agent.issue.comment_edited', resourceType: 'issue', resourceId: 'issue-1' },
		});

		await PATCH(makeEvent('PATCH', { body_md: 'updated' }));

		expect(writeAgentAuditLog).toHaveBeenCalledWith(
			CTX,
			'agent.issue.comment_edited',
			'issue',
			'issue-1',
			undefined
		);
	});
});

describe('DELETE .../comments/[commentId]', () => {
	it('calls comments.delete with comment_id from the URL and no request body needed', async () => {
		runAgentOp.mockResolvedValue({ status: 200, body: { success: true, issueId: 'issue-1' } });

		const res = await DELETE(makeEvent('DELETE'));

		expect(res.status).toBe(200);
		expect(runAgentOp).toHaveBeenCalledWith('comments.delete', CTX, 'issue-1', { comment_id: 'c1' }, 'http://localhost');
	});
});
