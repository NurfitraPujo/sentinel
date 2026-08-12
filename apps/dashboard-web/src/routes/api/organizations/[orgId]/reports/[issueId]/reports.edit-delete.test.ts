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

vi.mock('$lib/db/queries/reports', () => ({
	getReportDetail,
	updateManualIssueReport,
	deleteManualIssue,
}));
vi.mock('$lib/server/report-access', () => ({ getReporterId, getOrgRole }));
vi.mock('$lib/server/retention', () => ({ bestEffortDeleteObjects }));

const { PATCH, DELETE } = await import('./+server');

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
		getReporterId.mockResolvedValue('author-1');
		updateManualIssueReport.mockResolvedValue({ issue: {}, report: {} });

		const res = await PATCH(makeEvent('author-1', { bodyMd: 'new body' }));

		expect(res.status).toBe(200);
		expect(updateManualIssueReport).toHaveBeenCalledWith(
			expect.objectContaining({ issueId: 'issue-1', actorId: 'author-1', bodyMd: 'new body' })
		);
	});

	it('a non-author cannot edit the report, even a write-role member', async () => {
		getReportDetail.mockResolvedValue(reportDetail('unresolved'));
		getReporterId.mockResolvedValue('author-1');

		await expect(PATCH(makeEvent('someone-else', { bodyMd: 'x' }))).rejects.toMatchObject({ status: 403 });
		expect(updateManualIssueReport).not.toHaveBeenCalled();
	});

	it('the author cannot edit once the report is resolved', async () => {
		getReportDetail.mockResolvedValue(reportDetail('resolved'));
		getReporterId.mockResolvedValue('author-1');

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
		getReporterId.mockResolvedValue('author-1');
		getOrgRole.mockResolvedValue('viewer');
		deleteManualIssue.mockResolvedValue({ storageKeys: ['k1'] });

		const res = await DELETE(makeEvent('author-1'));

		expect(res.status).toBe(200);
		expect(deleteManualIssue).toHaveBeenCalledWith('issue-1');
		expect(bestEffortDeleteObjects).toHaveBeenCalledWith(['k1'], 'manual_issue_delete');
	});

	it('the author CANNOT delete their own report once resolved', async () => {
		getReportDetail.mockResolvedValue(reportDetail('resolved'));
		getReporterId.mockResolvedValue('author-1');
		getOrgRole.mockResolvedValue('viewer');

		await expect(DELETE(makeEvent('author-1'))).rejects.toMatchObject({ status: 403 });
		expect(deleteManualIssue).not.toHaveBeenCalled();
	});

	it('an org owner can delete ANY report, even resolved and not their own', async () => {
		getReportDetail.mockResolvedValue(reportDetail('resolved'));
		getReporterId.mockResolvedValue('author-1');
		getOrgRole.mockResolvedValue('owner');
		deleteManualIssue.mockResolvedValue({ storageKeys: [] });

		const res = await DELETE(makeEvent('owner-1'));

		expect(res.status).toBe(200);
		expect(deleteManualIssue).toHaveBeenCalledWith('issue-1');
	});

	it('a non-author, non-owner/admin member cannot delete the report', async () => {
		getReportDetail.mockResolvedValue(reportDetail('unresolved'));
		getReporterId.mockResolvedValue('author-1');
		getOrgRole.mockResolvedValue('engineer');

		await expect(DELETE(makeEvent('engineer-1'))).rejects.toMatchObject({ status: 403 });
		expect(deleteManualIssue).not.toHaveBeenCalled();
	});
});
