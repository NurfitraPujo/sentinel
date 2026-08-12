import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * R4/R17 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): the session claim DELETE route previously
 * always went through `requireReportAccessForIssue`, which 404s on any `system_error` issue id
 * (report-access.ts's §9 strict separation) -- an agent's claim on a system_error issue could
 * never be force-released by a human. R4 fixed this with a per-issue-type dispatch; R17 moved
 * that dispatch into the shared `requireIssueAccessAnyType` (issue-access-dispatch.ts), reused by
 * the attachments route and comment-access.ts. This proves the route now defers entirely to that
 * shared helper: an owner/admin can force-release a system_error issue's claim, and a
 * non-owner/admin cannot.
 */

const requireIssueAccessAnyType = vi.fn();
const requireReportAccessForIssue = vi.fn();
const releaseClaim = vi.fn();
const claimIssue = vi.fn();
const sendIssueNotificationEmails = vi.fn();

vi.mock('$lib/server/issue-access-dispatch', () => ({ requireIssueAccessAnyType }));
vi.mock('$lib/server/report-access', () => ({ requireReportAccessForIssue }));
vi.mock('$lib/db/queries/reports', () => ({
	claimIssue,
	releaseClaim,
	ClaimConflictError: class ClaimConflictError extends Error {},
}));
vi.mock('$lib/server/notify', () => ({ sendIssueNotificationEmails }));

const { DELETE } = await import('./+server');

function makeEvent(force: boolean, userId = 'owner-1') {
	return {
		params: { issueId: 'issue-1' },
		url: new URL(`http://localhost/api/issues/issue-1/claim${force ? '?force=true' : ''}`),
		locals: { auth: async () => ({ user: { id: userId } }) },
	} as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	releaseClaim.mockResolvedValue({ issue: { id: 'issue-1' }, notified: [] });
});

describe('DELETE /api/issues/[issueId]/claim (R4/R17)', () => {
	it('owner can force-release a claim on a system_error issue', async () => {
		requireIssueAccessAnyType.mockResolvedValue({
			issueId: 'issue-1',
			issueType: 'system_error',
			organizationId: 'org-1',
			role: 'owner',
		});

		const res = await DELETE(makeEvent(true));

		expect(res.status).toBe(200);
		expect(requireIssueAccessAnyType).toHaveBeenCalledWith('owner-1', 'issue-1', 'force-release');
		expect(releaseClaim).toHaveBeenCalledWith('issue-1', 'owner-1', { force: true });
	});

	it('a non-owner/admin cannot force-release a system_error issue claim (the shared dispatch throws)', async () => {
		requireIssueAccessAnyType.mockRejectedValue(
			Object.assign(new Error('Forbidden'), { status: 403 })
		);

		await expect(DELETE(makeEvent(true, 'engineer-1'))).rejects.toMatchObject({ status: 403 });
		expect(releaseClaim).not.toHaveBeenCalled();
	});

	it('still dispatches user_report issues through the shared helper', async () => {
		requireIssueAccessAnyType.mockResolvedValue({
			issueId: 'issue-1',
			issueType: 'user_report',
			organizationId: 'org-1',
			role: 'owner',
		});

		const res = await DELETE(makeEvent(true, 'owner-1'));

		expect(res.status).toBe(200);
		expect(requireIssueAccessAnyType).toHaveBeenCalledWith('owner-1', 'issue-1', 'force-release');
	});

	it('a plain (non-force) release requires only "write"', async () => {
		requireIssueAccessAnyType.mockResolvedValue({
			issueId: 'issue-1',
			issueType: 'user_report',
			organizationId: 'org-1',
			role: 'engineer',
		});

		const res = await DELETE(makeEvent(false, 'engineer-1'));

		expect(res.status).toBe(200);
		expect(requireIssueAccessAnyType).toHaveBeenCalledWith('engineer-1', 'issue-1', 'write');
	});
});
