import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * D23. The bulk update endpoint had no test file at all, which is how it ended up as the MORE
 * permissive of the two issue-write paths: it checked `organization_members` plus a locally
 * declared role allowlist, but never project membership, while the single-issue path
 * (`api/issues/[issueId]/status`) required both after D10. A user in the org but not on the
 * project could bulk-resolve that project's issues while being refused a single one.
 *
 * Both paths now go through `requireProjectAccess`. These tests fence the delegation; the helper's
 * own behaviour (org role table, project membership, 403 vs 404) is covered in
 * src/lib/server/issue-access.test.ts.
 */

const dbMock: any = { select: vi.fn(), from: vi.fn(), where: vi.fn(), innerJoin: vi.fn(), then: vi.fn() };
dbMock.select.mockReturnValue(dbMock);
dbMock.from.mockReturnValue(dbMock);
dbMock.where.mockReturnValue(dbMock);
dbMock.innerJoin.mockReturnValue(dbMock);
dbMock.then.mockImplementation((resolve: any) => resolve([]));
vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	issues: { id: 'id', projectId: 'projectId' },
	projects: { id: 'id', organizationId: 'organizationId' },
	organizationMembers: { organizationId: 'organizationId', userId: 'userId', role: 'role' },
	projectMembers: { projectId: 'projectId', userId: 'userId', role: 'role' },
}));

const issueQueries = {
	batchUpdateIssues: vi.fn(),
	MAX_BATCH_ISSUE_IDS: 100,
};
vi.mock('$lib/db/queries/issues', () => issueQueries);

const access = {
	requireProjectAccess: vi.fn(),
	validateResolvedInVersion: vi.fn(),
};
vi.mock('$lib/server/issue-access', () => access);

const { POST } = await import('./[projectId]/issues/batch/+server');

function locals(session: { id: string } | null) {
	return { auth: async () => (session ? { user: { id: session.id } } : null) } as any;
}

function body(payload: unknown) {
	return new Request('http://x', { method: 'POST', body: JSON.stringify(payload) });
}

describe('POST /api/projects/:projectId/issues/batch (D23)', () => {
	beforeEach(() => {
		vi.resetAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.innerJoin.mockReturnValue(dbMock);
		dbMock.then.mockReset();
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
		access.requireProjectAccess.mockResolvedValue({ projectId: 'proj-1', organizationId: 'org-1' });
		access.validateResolvedInVersion.mockReturnValue(null);
		issueQueries.batchUpdateIssues.mockResolvedValue(2);
	});

	it('401s an unauthenticated request without touching authorization or the DB', async () => {
		await expect(
			POST({
				params: { projectId: 'proj-1' },
				request: body({ action: 'resolve', issueIds: ['i1'] }),
				locals: locals(null),
			} as any)
		).rejects.toMatchObject({ status: 401 });
		expect(access.requireProjectAccess).not.toHaveBeenCalled();
	});

	// The core D23 fence: this fails if the endpoint reverts to its own org-only membership check.
	it('demands write access on the project before updating anything', async () => {
		await POST({
			params: { projectId: 'proj-1' },
			request: body({ action: 'resolve', issueIds: ['i1', 'i2'] }),
			locals: locals({ id: 'user-1' }),
		} as any);

		expect(access.requireProjectAccess).toHaveBeenCalledWith('user-1', 'proj-1', 'write');
	});

	it('propagates a 403 for an org member who is not on the project, and updates nothing', async () => {
		access.requireProjectAccess.mockRejectedValueOnce(
			Object.assign(new Error('Forbidden'), { status: 403 })
		);

		await expect(
			POST({
				params: { projectId: 'proj-1' },
				request: body({ action: 'resolve', issueIds: ['i1'] }),
				locals: locals({ id: 'org-member-not-on-project' }),
			} as any)
		).rejects.toMatchObject({ status: 403 });
		expect(issueQueries.batchUpdateIssues).not.toHaveBeenCalled();
	});

	it('validates resolvedInVersion instead of passing it through to the varchar(100) column', async () => {
		access.validateResolvedInVersion.mockImplementationOnce(() => {
			throw Object.assign(new Error('too long'), { status: 400 });
		});

		await expect(
			POST({
				params: { projectId: 'proj-1' },
				request: body({ action: 'resolve', issueIds: ['i1'], resolvedInVersion: 'x'.repeat(200) }),
				locals: locals({ id: 'user-1' }),
			} as any)
		).rejects.toMatchObject({ status: 400 });
		expect(issueQueries.batchUpdateIssues).not.toHaveBeenCalled();
	});

	it('400s a malformed JSON body rather than surfacing a 500', async () => {
		await expect(
			POST({
				params: { projectId: 'proj-1' },
				request: new Request('http://x', { method: 'POST', body: '{not json' }),
				locals: locals({ id: 'user-1' }),
			} as any)
		).rejects.toMatchObject({ status: 400 });
		expect(issueQueries.batchUpdateIssues).not.toHaveBeenCalled();
	});

	it('updates and reports the count on the happy path', async () => {
		const res = await POST({
			params: { projectId: 'proj-1' },
			request: body({ action: 'resolve', issueIds: ['i1', 'i2'] }),
			locals: locals({ id: 'user-1' }),
		} as any);

		expect(await res.json()).toEqual({ success: true, updated: 2 });
	});
});
