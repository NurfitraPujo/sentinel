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
	issues: { id: 'id', projectId: 'projectId', environment: 'environment' },
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

const environmentAccess = {
	resolveEnvironmentScope: vi.fn(),
};
vi.mock('$lib/server/environment-access', () => environmentAccess);

const { POST } = await import('./[projectId=uuid]/issues/batch/+server');

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
		environmentAccess.resolveEnvironmentScope.mockResolvedValue(null);
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

/**
 * sentinel-project-environments §2.2 — the environment dimension on the bulk path.
 *
 * This file previously had no environment case at all, and that absence IS the finding: the route
 * resolved only the project dimension via `requireProjectAccess`, which never reads
 * `project_environment_grants`, while the single-issue path (`requireIssueAccess`) resolved the
 * scope and 404'd. §2.2's guard table names this file and says the bulk path "must not now become
 * *more* permissive" than the single one — which is exactly what it had become.
 *
 * The guard is asserted as DELEGATION, the way the D23 tests above assert `requireProjectAccess`:
 * the resolver is called with the caller and the project, and the scope it returns reaches
 * `batchUpdateIssues`, whose own predicate composition is fenced at its own boundary in
 * `src/lib/db/queries/issues.test.ts`.
 */
describe('POST /api/projects/:projectId/issues/batch — the environment guard (§2.2)', () => {
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
		environmentAccess.resolveEnvironmentScope.mockResolvedValue(null);
		issueQueries.batchUpdateIssues.mockResolvedValue(0);
	});

	// The core fence. Guard-deletion red-proof: remove the `resolveEnvironmentScope` call (or the
	// `environmentScope` key) from the route and this fails on both assertions.
	it("passes the narrowed caller's scope into the update instead of updating unscoped", async () => {
		environmentAccess.resolveEnvironmentScope.mockResolvedValue(['staging']);

		await POST({
			params: { projectId: 'proj-1' },
			request: body({ action: 'resolve', issueIds: ['staging-issue', 'production-issue'] }),
			locals: locals({ id: 'narrowed-user' }),
		} as any);

		expect(environmentAccess.resolveEnvironmentScope).toHaveBeenCalledWith(
			'narrowed-user',
			'proj-1'
		);
		expect(issueQueries.batchUpdateIssues).toHaveBeenCalledWith(
			'proj-1',
			'resolve',
			['staging-issue', 'production-issue'],
			expect.objectContaining({ environmentScope: ['staging'] })
		);
	});

	// Decision 20 on a bulk path: FEWER ROWS, not a 403. A 403 would confirm to a contractor that a
	// production issue with the id they named exists — the fact the boundary was drawn to withhold.
	// The count is the rows actually matched, so an out-of-scope id is indistinguishable from one
	// that does not exist.
	it('reports the rows actually updated rather than refusing the batch outright', async () => {
		environmentAccess.resolveEnvironmentScope.mockResolvedValue(['staging']);
		// Two ids sent; only the in-scope one matched.
		issueQueries.batchUpdateIssues.mockResolvedValue(1);

		const res = await POST({
			params: { projectId: 'proj-1' },
			request: body({ action: 'resolve', issueIds: ['staging-issue', 'production-issue'] }),
			locals: locals({ id: 'narrowed-user' }),
		} as any);

		expect(res.status).toBe(200);
		expect(await res.json()).toEqual({ success: true, updated: 1 });
	});

	// Absence means all: an unrestricted caller has no grant row, and `null` must reach the query
	// as `null` — not as `[]`, which is the natural-but-inverted reading that would silently make
	// every bulk action a no-op for everybody.
	it('passes null through unchanged for an unrestricted caller', async () => {
		environmentAccess.resolveEnvironmentScope.mockResolvedValue(null);
		issueQueries.batchUpdateIssues.mockResolvedValue(2);

		await POST({
			params: { projectId: 'proj-1' },
			request: body({ action: 'resolve', issueIds: ['i1', 'i2'] }),
			locals: locals({ id: 'user-1' }),
		} as any);

		expect(issueQueries.batchUpdateIssues).toHaveBeenCalledWith(
			'proj-1',
			'resolve',
			['i1', 'i2'],
			expect.objectContaining({ environmentScope: null })
		);
	});

	// The project dimension still comes first: a caller refused there never reaches the resolver,
	// so the environment guard cannot be read as having replaced it.
	it('resolves the scope only after project access has been granted', async () => {
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
		expect(environmentAccess.resolveEnvironmentScope).not.toHaveBeenCalled();
	});
});
