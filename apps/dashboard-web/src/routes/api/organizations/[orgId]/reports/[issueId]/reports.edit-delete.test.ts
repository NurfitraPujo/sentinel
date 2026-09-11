import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * R11 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md, §9): PATCH/DELETE
 * /api/organizations/[orgId]/reports/[issueId] -- the design's promised author edit/delete of
 * their own report body, until resolved, plus an owner/admin delete-anytime carve-out. No such
 * route existed before this fix (404 for every caller, author or not).
 */

const getReportDetail = vi.fn();
const updateManualIssueReport = vi.fn();
const deleteManualIssue = vi.fn();
const getReporterId = vi.fn();
const getOrgRole = vi.fn();
const bestEffortDeleteObjects = vi.fn();

const listIssueAttachments = vi.fn(async () => []);
const requireReportAccess = vi.fn(async () => ({ organizationId: 'org-1', role: 'engineer' }));
const readTriage = vi.fn();

/**
 * sentinel-project-environments §2.2 — this route now runs the environment guard through
 * `issue-access-dispatch.ts: requireIssueAccessAnyType`, the guard point §2.2's table names for
 * reports (explicitly NOT `report-access.ts: requireReportAccessForIssue`). Mocked here so these
 * handler tests stay database-free; that it is actually CALLED on each of the three handlers is
 * asserted below, because a mock that is never reached cannot tell "guarded" from "not guarded".
 */
const requireIssueAccessAnyType = vi.fn(async () => ({
	issueId: 'issue-1',
	issueType: 'user_report',
	organizationId: 'org-1',
	role: 'engineer',
}));

vi.mock('$lib/db/queries/reports', () => ({
	getReportDetail,
	updateManualIssueReport,
	deleteManualIssue,
	listIssueAttachments,
}));
vi.mock('$lib/server/report-access', () => ({ getReporterId, getOrgRole, requireReportAccess }));
vi.mock('$lib/server/triage', () => ({ readTriage }));
vi.mock('$lib/server/retention', () => ({ bestEffortDeleteObjects }));
vi.mock('$lib/server/issue-access-dispatch', () => ({ requireIssueAccessAnyType }));

const { GET, PATCH, DELETE } = await import('./+server');

function makeEvent(userId: string, body?: unknown) {
	return {
		params: { orgId: 'org-1', issueId: 'issue-1' },
		locals: { auth: async () => ({ user: { id: userId } }) },
		request: { json: async () => body ?? {} },
	} as any;
}

function reportDetail(status: string) {
	return {
		issue: { id: 'issue-1', status, message: 'old title' },
		report: { issueId: 'issue-1', bodyMd: 'old body', severity: 'low' },
		organizationId: 'org-1',
	};
}

beforeEach(() => {
	vi.clearAllMocks();
});

describe('PATCH /api/organizations/[orgId]/reports/[issueId] (R11)', () => {
	it('the author can edit their unresolved report', async () => {
		getReportDetail.mockResolvedValue(reportDetail('unresolved'));
		getReporterId.mockResolvedValue({ kind: 'org_member', userId: 'author-1' });
		updateManualIssueReport.mockResolvedValue({ issue: {}, report: {} });

		const res = await PATCH(makeEvent('author-1', { bodyMd: 'new body' }));

		expect(res.status).toBe(200);
		expect(updateManualIssueReport).toHaveBeenCalledWith(
			expect.objectContaining({ issueId: 'issue-1', actorId: 'author-1', bodyMd: 'new body' })
		);
	});

	it('a non-author cannot edit the report, even a write-role member', async () => {
		getReportDetail.mockResolvedValue(reportDetail('unresolved'));
		getReporterId.mockResolvedValue({ kind: 'org_member', userId: 'author-1' });

		await expect(PATCH(makeEvent('someone-else', { bodyMd: 'x' }))).rejects.toMatchObject({ status: 403 });
		expect(updateManualIssueReport).not.toHaveBeenCalled();
	});

	it('the author cannot edit once the report is resolved', async () => {
		getReportDetail.mockResolvedValue(reportDetail('resolved'));
		getReporterId.mockResolvedValue({ kind: 'org_member', userId: 'author-1' });

		await expect(PATCH(makeEvent('author-1', { bodyMd: 'x' }))).rejects.toMatchObject({ status: 403 });
		expect(updateManualIssueReport).not.toHaveBeenCalled();
	});

	it('404s for a report in a different organization', async () => {
		getReportDetail.mockResolvedValue({ ...reportDetail('unresolved'), organizationId: 'org-2' });

		await expect(PATCH(makeEvent('author-1', { bodyMd: 'x' }))).rejects.toMatchObject({ status: 404 });
	});
});

describe('DELETE /api/organizations/[orgId]/reports/[issueId] (R11)', () => {
	it('the author can delete their own unresolved report', async () => {
		getReportDetail.mockResolvedValue(reportDetail('unresolved'));
		getReporterId.mockResolvedValue({ kind: 'org_member', userId: 'author-1' });
		getOrgRole.mockResolvedValue('viewer');
		deleteManualIssue.mockResolvedValue({ storageKeys: ['k1'] });

		const res = await DELETE(makeEvent('author-1'));

		expect(res.status).toBe(200);
		expect(deleteManualIssue).toHaveBeenCalledWith('issue-1');
		expect(bestEffortDeleteObjects).toHaveBeenCalledWith(['k1'], 'manual_issue_delete');
	});

	it('the author CANNOT delete their own report once resolved', async () => {
		getReportDetail.mockResolvedValue(reportDetail('resolved'));
		getReporterId.mockResolvedValue({ kind: 'org_member', userId: 'author-1' });
		getOrgRole.mockResolvedValue('viewer');

		await expect(DELETE(makeEvent('author-1'))).rejects.toMatchObject({ status: 403 });
		expect(deleteManualIssue).not.toHaveBeenCalled();
	});

	it('an org owner can delete ANY report, even resolved and not their own', async () => {
		getReportDetail.mockResolvedValue(reportDetail('resolved'));
		getReporterId.mockResolvedValue({ kind: 'org_member', userId: 'author-1' });
		getOrgRole.mockResolvedValue('owner');
		deleteManualIssue.mockResolvedValue({ storageKeys: [] });

		const res = await DELETE(makeEvent('owner-1'));

		expect(res.status).toBe(200);
		expect(deleteManualIssue).toHaveBeenCalledWith('issue-1');
	});

	it('a non-author, non-owner/admin member cannot delete the report', async () => {
		getReportDetail.mockResolvedValue(reportDetail('unresolved'));
		getReporterId.mockResolvedValue({ kind: 'org_member', userId: 'author-1' });
		getOrgRole.mockResolvedValue('engineer');

		await expect(DELETE(makeEvent('engineer-1'))).rejects.toMatchObject({ status: 403 });
		expect(deleteManualIssue).not.toHaveBeenCalled();
	});
});

/**
 * sentinel-project-environments §2.2, decision 25 — that this route actually GOES THROUGH the guard
 * point §2.2 names.
 *
 * The predicate itself is asserted against a real database at
 * `src/lib/server/read-guards.integration.test.ts`; what a mock can prove and that file cannot is
 * that each handler here calls the dispatch at all. It could not before: every handler authorised
 * at the ORGANIZATION level only, so §2.2's claim that guarding `requireIssueAccessAnyType` covers
 * `api/organizations/[orgId]/reports/[issueId]` "by construction" was false of this file. A mock
 * that is never reached cannot tell "guarded" from "not guarded" (B10 addendum), so the call is the
 * assertion.
 */
describe('§2.2 — the environment guard point, on all three handlers', () => {
	it('GET runs the dispatch after the org-level checks', async () => {
		getReportDetail.mockResolvedValue(reportDetail('unresolved'));
		readTriage.mockResolvedValue(null);

		await GET(makeEvent('reader-1'));

		expect(requireIssueAccessAnyType).toHaveBeenCalledWith('reader-1', 'issue-1', 'read');
	});

	it('PATCH runs it too — an out-of-scope report is not editable either', async () => {
		getReportDetail.mockResolvedValue(reportDetail('unresolved'));
		getReporterId.mockResolvedValue({ kind: 'org_member', userId: 'author-1' });
		updateManualIssueReport.mockResolvedValue({ issue: {}, report: {} });

		await PATCH(makeEvent('author-1', { bodyMd: 'new body' }));

		expect(requireIssueAccessAnyType).toHaveBeenCalledWith('author-1', 'issue-1', 'read');
	});

	it('DELETE runs it too', async () => {
		getReportDetail.mockResolvedValue(reportDetail('unresolved'));
		getReporterId.mockResolvedValue({ kind: 'org_member', userId: 'author-1' });
		getOrgRole.mockResolvedValue('engineer');
		deleteManualIssue.mockResolvedValue({ storageKeys: [] });

		await DELETE(makeEvent('author-1'));

		expect(requireIssueAccessAnyType).toHaveBeenCalledWith('author-1', 'issue-1', 'read');
	});

	it('a 404 from the dispatch propagates rather than being swallowed', async () => {
		getReportDetail.mockResolvedValue(reportDetail('unresolved'));
		readTriage.mockResolvedValue(null);
		requireIssueAccessAnyType.mockRejectedValueOnce(
			Object.assign(new Error('Issue not found'), { status: 404 })
		);

		// Decision 20: the environment dimension denies with a 404, never a 403 — a 403 on a report
		// id confirms that a production report with that id exists.
		await expect(GET(makeEvent('reader-1'))).rejects.toMatchObject({ status: 404 });
	});
});
