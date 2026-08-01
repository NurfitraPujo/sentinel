import { describe, it, expect, vi, beforeEach } from 'vitest';

// Chainable Drizzle-query double, following the pattern in
// src/routes/api/organizations/keys.test.ts: each awaited chain resolves via the queued `then`
// implementation below, in the order the route code issues its queries.
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
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
	return dbMock;
}

const dbMock = makeDbMock();

// The route imports '$lib/server/db'; the shared helper (src/lib/server/issue-access.ts) imports
// the same file via a relative './db' specifier. Both resolve to this module.
vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	issues: { id: 'id', projectId: 'projectId' },
	projects: { id: 'id', organizationId: 'organizationId' },
	organizationMembers: { organizationId: 'organizationId', userId: 'userId', role: 'role' },
	projectMembers: { projectId: 'projectId', userId: 'userId', role: 'role' },
}));

const issueQueries = {
	updateIssueStatus: vi.fn(),
};
vi.mock('$lib/db/queries/issues', () => issueQueries);

const { PATCH } = await import('./[issueId]/status/+server');

function locals(session: { id: string } | null) {
	return { auth: async () => (session ? { user: { id: session.id } } : null) } as any;
}

function patchRequest(body: unknown) {
	return new Request('http://x', { method: 'PATCH', body: JSON.stringify(body) });
}

const issueRow = { issueId: 'issue-1', projectId: 'project-1', organizationId: 'org-1' };

describe('PATCH /api/issues/:id/status', () => {
	beforeEach(() => {
		vi.clearAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.innerJoin.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
	});

	it('401s when there is no session', async () => {
		await expect(
			PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'resolved' }),
				locals: locals(null),
			} as any)
		).rejects.toMatchObject({ status: 401 });
	});

	it('400s on an invalid status', async () => {
		await expect(
			PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'bogus' }),
				locals: locals({ id: 'user-1' }),
			} as any)
		).rejects.toMatchObject({ status: 400 });
	});

	// D10: an org `viewer` is not in the bulk endpoint's write allowlist
	// (owner|admin|engineer|support) and must not be able to resolve issues one at a time either.
	it('403s an org viewer attempting to resolve an issue', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([issueRow])) // issue+project lookup
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'viewer' }])); // org role

		await expect(
			PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'resolved' }),
				locals: locals({ id: 'user-1' }),
			} as any)
		).rejects.toMatchObject({ status: 403 });
		expect(issueQueries.updateIssueStatus).not.toHaveBeenCalled();
	});

	it.each(['owner', 'admin', 'engineer', 'support'])(
		'allows an org %s who is also a project member to resolve an issue',
		async (role) => {
			dbMock.then
				.mockImplementationOnce((resolve: any) => resolve([issueRow])) // issue+project lookup
				.mockImplementationOnce((resolve: any) => resolve([{ role }])) // org role
				.mockImplementationOnce((resolve: any) => resolve([{ role: 'developer' }])); // project membership

			const res = await PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'resolved' }),
				locals: locals({ id: 'user-1' }),
			} as any);

			expect(res.status).toBe(200);
			expect(issueQueries.updateIssueStatus).toHaveBeenCalledWith(
				'issue-1',
				'resolved',
				undefined,
				'user',
				'user-1'
			);
		}
	);

	// D10 item 2: org role alone is not enough — the caller must also be on the issue's project.
	it('403s an org admin who is not a member of the issue project', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([issueRow])) // issue+project lookup
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'admin' }])) // org role
			.mockImplementationOnce((resolve: any) => resolve([])); // no project membership row

		await expect(
			PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'resolved' }),
				locals: locals({ id: 'user-1' }),
			} as any)
		).rejects.toMatchObject({ status: 403 });
		expect(issueQueries.updateIssueStatus).not.toHaveBeenCalled();
	});

	// D10 item 3: resolvedInVersion is validated against the varchar(100) column instead of being
	// passed through, so an oversized value 400s rather than 500ing at the DB.
	it('400s an oversized resolvedInVersion instead of letting it reach the DB', async () => {
		await expect(
			PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'resolved', resolvedInVersion: 'v'.repeat(101) }),
				locals: locals({ id: 'user-1' }),
			} as any)
		).rejects.toMatchObject({ status: 400 });
		expect(issueQueries.updateIssueStatus).not.toHaveBeenCalled();
	});

	it('accepts a resolvedInVersion at exactly the 100-char limit', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([issueRow]))
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }]))
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'developer' }]));

		const res = await PATCH({
			params: { issueId: 'issue-1' },
			request: patchRequest({ status: 'resolved', resolvedInVersion: 'v'.repeat(100) }),
			locals: locals({ id: 'user-1' }),
		} as any);

		expect(res.status).toBe(200);
		expect(issueQueries.updateIssueStatus).toHaveBeenCalledWith(
			'issue-1',
			'resolved',
			'v'.repeat(100),
			'user',
			'user-1'
		);
	});
});
