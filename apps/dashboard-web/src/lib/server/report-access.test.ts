import { describe, it, expect, vi, beforeEach } from 'vitest';

function makeDbMock() {
	const dbMock: any = {
		select: vi.fn(),
		from: vi.fn(),
		innerJoin: vi.fn(),
		where: vi.fn(),
		then: vi.fn(),
	};
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.innerJoin.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.then.mockReset();
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
	return dbMock;
}

const dbMock = makeDbMock();

vi.mock('./db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	issues: { id: 'id', projectId: 'projectId', issueType: 'issueType' },
	projects: { id: 'id', organizationId: 'organizationId' },
	organizationMembers: { organizationId: 'organizationId', userId: 'userId', role: 'role' },
	manualIssueReports: { issueId: 'issueId', reporterId: 'reporterId' },
}));

const {
	requireReportAccess,
	requireReportAccessForIssue,
	canEditReport,
} = await import('./report-access');

function queueResult(rows: any[]) {
	dbMock.then.mockImplementationOnce((resolve: any) => resolve(rows));
}

beforeEach(() => {
	vi.clearAllMocks();
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.innerJoin.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.then.mockReset();
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
});

describe('requireReportAccess', () => {
	it('403s when the caller has no membership row in the organization', async () => {
		queueResult([]);

		await expect(requireReportAccess('user-1', 'org-1', 'read')).rejects.toMatchObject({
			status: 403,
		});
	});

	// §9 Q8: any recognized org member, including viewer, may create/read/comment.
	it('allows a viewer to create a report', async () => {
		queueResult([{ role: 'viewer' }]);

		const result = await requireReportAccess('user-1', 'org-1', 'create');
		expect(result.role).toBe('viewer');
	});

	it('refuses a viewer write access (claim/resolve/link/move)', async () => {
		queueResult([{ role: 'viewer' }]);

		await expect(requireReportAccess('user-1', 'org-1', 'write')).rejects.toMatchObject({
			status: 403,
		});
	});

	it('allows a support role write access', async () => {
		queueResult([{ role: 'support' }]);

		const result = await requireReportAccess('user-1', 'org-1', 'write');
		expect(result.role).toBe('support');
	});

	it('refuses a non-owner/admin write role force-release', async () => {
		queueResult([{ role: 'engineer' }]);

		await expect(requireReportAccess('user-1', 'org-1', 'force-release')).rejects.toMatchObject({
			status: 403,
		});
	});

	it('allows an admin force-release', async () => {
		queueResult([{ role: 'admin' }]);

		const result = await requireReportAccess('user-1', 'org-1', 'force-release');
		expect(result.role).toBe('admin');
	});
});

describe('requireReportAccessForIssue', () => {
	it('404s when the issue id does not name a user_report issue (including a real system_error issue)', async () => {
		// The issue/project join resolves, but issueType is 'system_error' — this must 404, not
		// silently allow report-access checks onto the error-dashboard side (§9).
		queueResult([
			{ issueId: 'issue-1', projectId: 'project-1', organizationId: 'org-1', issueType: 'system_error' },
		]);

		await expect(requireReportAccessForIssue('user-1', 'issue-1', 'read')).rejects.toMatchObject({
			status: 404,
		});
	});

	it('404s when the issue does not exist at all', async () => {
		queueResult([]);

		await expect(requireReportAccessForIssue('user-1', 'missing', 'read')).rejects.toMatchObject({
			status: 404,
		});
	});

	it('resolves org context and checks role for a genuine user_report issue', async () => {
		queueResult([
			{ issueId: 'issue-1', projectId: 'project-1', organizationId: 'org-1', issueType: 'user_report' },
		]);
		queueResult([{ role: 'support' }]);

		const ctx = await requireReportAccessForIssue('user-1', 'issue-1', 'write');
		expect(ctx).toMatchObject({ issueId: 'issue-1', projectId: 'project-1', organizationId: 'org-1', role: 'support' });
	});
});

describe('canEditReport', () => {
	it('lets the author edit their own report regardless of role', () => {
		expect(canEditReport('viewer', 'user-1', 'user-1')).toBe(true);
	});

	it('refuses a non-write-role non-author', () => {
		expect(canEditReport('viewer', 'user-1', 'user-2')).toBe(false);
	});

	it('lets a write-role edit someone else\'s report', () => {
		expect(canEditReport('support', 'user-1', 'user-2')).toBe(true);
	});
});
