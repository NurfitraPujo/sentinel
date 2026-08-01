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
		limit: vi.fn(),
		then: vi.fn(),
	};
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.innerJoin.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.limit.mockReturnValue(dbMock);
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
	searchIssuesInOrg: vi.fn(),
};
vi.mock('$lib/db/queries/issues', () => issueQueries);

const { GET } = await import('./search/+server');

function locals(session: { id: string } | null) {
	return { auth: async () => (session ? { user: { id: session.id } } : null) } as any;
}

function req(params: Record<string, string>) {
	const url = new URL('http://x/api/issues/search');
	for (const [k, v] of Object.entries(params)) url.searchParams.set(k, v);
	return { url, locals: locals({ id: 'user-1' }) };
}

describe('GET /api/issues/search', () => {
	beforeEach(() => {
		vi.clearAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.innerJoin.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.limit.mockReturnValue(dbMock);
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
	});

	it('401s when there is no session', async () => {
		const url = new URL('http://x/api/issues/search?q=abc');
		await expect(GET({ url, locals: locals(null) } as any)).rejects.toMatchObject({ status: 401 });
	});

	it('403s an org viewer with malformed/unrecognized role', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) =>
				resolve([{ orgId: 'org-1' }])
			) // currentIssueId -> org lookup
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'not-a-real-role' }])); // org role

		const { url, locals: l } = req({ q: 'boom', issueId: 'issue-1' });
		await expect(GET({ url, locals: l } as any)).rejects.toMatchObject({ status: 403 });
		expect(issueQueries.searchIssuesInOrg).not.toHaveBeenCalled();
	});

	// D10: an org member who is not on the project of the returned issues must not see them, even
	// though searchIssuesInOrg itself is org-wide with no project filter.
	it('excludes issues from a project the caller is not a member of', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([{ orgId: 'org-1' }])) // currentIssueId -> org lookup
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'viewer' }])) // org role (read-capable)
			.mockImplementationOnce((resolve: any) =>
				resolve([{ projectId: 'project-accessible' }])
			); // getAccessibleProjectIds

		issueQueries.searchIssuesInOrg.mockResolvedValueOnce([
			{ id: 'issue-a', projectId: 'project-accessible', message: 'boom a' },
			{ id: 'issue-b', projectId: 'project-forbidden', message: 'boom b' },
		]);

		const { url, locals: l } = req({ q: 'boom', issueId: 'issue-1' });
		const res = await GET({ url, locals: l } as any);
		const body = await res.json();

		expect(body.issues).toEqual([
			{ id: 'issue-a', projectId: 'project-accessible', message: 'boom a' },
		]);
	});

	it('returns an empty list when the caller has no project membership at all', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([{ orgId: 'org-1' }]))
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'admin' }]))
			.mockImplementationOnce((resolve: any) => resolve([])); // no accessible projects

		issueQueries.searchIssuesInOrg.mockResolvedValueOnce([
			{ id: 'issue-a', projectId: 'project-forbidden', message: 'boom a' },
		]);

		const { url, locals: l } = req({ q: 'boom', issueId: 'issue-1' });
		const res = await GET({ url, locals: l } as any);
		const body = await res.json();

		expect(body.issues).toEqual([]);
	});
});
