import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * A11 (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7f): claim/release 409s are enriched with
 * `claimedBy`/`claimedAt`, and a non-claimant mutating a claimed issue fires a structured warning
 * log without changing the response. Same mocking style as agent-ops.n7e.test.ts.
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
vi.mock('$lib/db/queries/comments', () => ({
	createComment: vi.fn(),
	editComment: vi.fn(),
	deleteComment: vi.fn(),
	getCommentById: vi.fn(),
	CommentValidationError,
	CommentNotFoundError,
}));

vi.mock('$lib/db/queries/agent-work', () => ({ recordAgentProgress: vi.fn() }));

const logWarn = vi.fn();
vi.mock('$lib/server/observability/log', () => ({ log: { warn: logWarn, info: vi.fn(), error: vi.fn() } }));

const { runAgentOp } = await import('./agent-ops');
const { updateIssueStatus } = await import('$lib/db/queries/issues');

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
		claimedAt: null,
		...overrides,
	};
}

beforeEach(() => {
	vi.clearAllMocks();
});

describe('A11: claim conflict enrichment', () => {
	it('claim 409 includes claimedBy/claimedAt from a fresh re-read', async () => {
		const claimedAt = new Date('2026-08-14T10:00:00.000Z');
		// First resolveAgentIssueScope call: initial scope resolution (unclaimed as far as we know).
		resolveAgentIssueScope.mockResolvedValueOnce(issueScope());
		claimIssue.mockRejectedValue(new ClaimConflictError('Issue is already claimed'));
		// Second call inside throwClaimConflict: fresh state showing who won the race.
		resolveAgentIssueScope.mockResolvedValueOnce(
			issueScope({ assignedTo: 'agent-2', assigneeType: 'agent', claimedAt })
		);

		let caught: any;
		try {
			await runAgentOp('issues.claim', CTX as any, 'issue-1', {}, 'http://localhost');
		} catch (err) {
			caught = err;
		}

		expect(caught).toMatchObject({ status: 409 });
		expect(caught.body).toEqual({
			message: 'Issue is already claimed',
			claimedBy: 'agent-2',
			claimedAt: claimedAt.toISOString(),
		});
	});

	it('release 409 includes claimedBy/claimedAt too', async () => {
		resolveAgentIssueScope.mockResolvedValueOnce(issueScope({ assignedTo: 'agent-1' }));
		releaseClaim.mockRejectedValue(new ClaimConflictError('Issue is not claimed by this actor'));
		resolveAgentIssueScope.mockResolvedValueOnce(
			issueScope({ assignedTo: 'agent-3', assigneeType: 'agent', claimedAt: null })
		);

		let caught: any;
		try {
			await runAgentOp('issues.claim.release', CTX as any, 'issue-1', {}, 'http://localhost');
		} catch (err) {
			caught = err;
		}

		expect(caught).toMatchObject({ status: 409 });
		expect(caught.body).toMatchObject({ claimedBy: 'agent-3', claimedAt: null });
	});

	it('falls back to null context if the re-read itself fails (never turns into a 500)', async () => {
		resolveAgentIssueScope.mockResolvedValueOnce(issueScope());
		claimIssue.mockRejectedValue(new ClaimConflictError('Issue is already claimed'));
		resolveAgentIssueScope.mockRejectedValueOnce(new Error('issue vanished'));

		let caught: any;
		try {
			await runAgentOp('issues.claim', CTX as any, 'issue-1', {}, 'http://localhost');
		} catch (err) {
			caught = err;
		}

		expect(caught).toMatchObject({ status: 409 });
		expect(caught.body).toEqual({
			message: 'Issue is already claimed',
			claimedBy: null,
			claimedAt: null,
		});
	});
});

describe('A11: non-claimant mutation warning (observability only, no behavior change)', () => {
	it('logs agent.mutated_claimed_issue when a non-claimant changes status on a claimed issue', async () => {
		resolveAgentIssueScope.mockResolvedValue(
			issueScope({ assignedTo: 'agent-2', assigneeType: 'agent', claimedAt: new Date('2026-08-14T09:00:00.000Z') })
		);
		(updateIssueStatus as any).mockResolvedValue({ changed: true, notified: [] });

		const result = await runAgentOp(
			'issues.status',
			CTX as any,
			'issue-1',
			{ status: 'resolved' },
			'http://localhost'
		);

		expect(result.status).toBe(200);
		expect(logWarn).toHaveBeenCalledWith(
			'agent.mutated_claimed_issue',
			expect.objectContaining({ action: 'issues.status', agentId: 'agent-1', claimedBy: 'agent-2' })
		);
	});

	it('does not log when the calling agent IS the claimant', async () => {
		resolveAgentIssueScope.mockResolvedValue(issueScope({ assignedTo: 'agent-1', assigneeType: 'agent' }));
		(updateIssueStatus as any).mockResolvedValue({ changed: true, notified: [] });

		await runAgentOp('issues.status', CTX as any, 'issue-1', { status: 'resolved' }, 'http://localhost');

		expect(logWarn).not.toHaveBeenCalled();
	});

	it('does not log when the issue is unclaimed', async () => {
		resolveAgentIssueScope.mockResolvedValue(issueScope({ assignedTo: null }));
		(updateIssueStatus as any).mockResolvedValue({ changed: true, notified: [] });

		await runAgentOp('issues.status', CTX as any, 'issue-1', { status: 'resolved' }, 'http://localhost');

		expect(logWarn).not.toHaveBeenCalled();
	});
});
