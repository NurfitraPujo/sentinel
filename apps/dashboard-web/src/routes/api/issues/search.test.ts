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
	// `clearAllMocks` clears call records but NOT queued `mockImplementationOnce` entries, so an
	// un-consumed queued resolution leaks into the NEXT test and answers the wrong query, making
	// results order-dependent. `mockReset` on the queue-bearing mock drops the queue; the base
	// implementation is re-established on the next line.
	dbMock.then.mockReset();
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
	return dbMock;
}

const dbMock = makeDbMock();

// The route imports '$lib/server/db'; the shared helper (src/lib/server/issue-access.ts) imports
// the same file via a relative './db' specifier. Both resolve to this module.
vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	issues: { id: 'id', projectId: 'projectId', environment: 'environment' },
	projects: { id: 'id', name: 'name', organizationId: 'organizationId' },
	organizationMembers: { organizationId: 'organizationId', userId: 'userId', role: 'role' },
	projectMembers: { projectId: 'projectId', userId: 'userId', role: 'role' },
	projectEnvironmentGrants: {
		userId: 'userId',
		projectId: 'projectId',
		environments: 'environments',
	},
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
		dbMock.then.mockReset();
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
	//
	// sentinel-project-environments decision 45: the derivation behind that filter WIDENED from
	// `getAccessibleProjectIds` (`project_members` only) to `getAccessibleProjects`
	// (`project_members` ∪ `organization_members` in `ISSUE_WRITE_ROLES`). The queued resolutions
	// below follow the route's real query order: org lookup, org role, the grant read
	// (`resolveEnvironmentScopes`), then `getAccessibleProjects`'s two halves.
	it('excludes issues from a project the caller cannot reach', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([{ orgId: 'org-1' }])) // currentIssueId -> org lookup
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'viewer' }])) // org role (read-capable)
			.mockImplementationOnce((resolve: any) => resolve([])) // resolveEnvironmentScopes: no grant rows
			.mockImplementationOnce((resolve: any) => resolve([])) // getAccessibleProjects: by org role
			.mockImplementationOnce((resolve: any) =>
				resolve([{ id: 'project-accessible', name: 'A', organizationId: 'org-1' }])
			); // getAccessibleProjects: by project role

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

	// Decision 45's reason for widening, asserted rather than described: an org-role staff member
	// with NO `project_members` row reached NOTHING here before — `getAccessibleProjectIds` read a
	// table nothing writes, so the filter returned `[]` for every user and any guard added to
	// `searchIssuesInOrg` was unobservable. This is the widening; it is user-visible.
	it('returns results to an org-role member who has no project_members row at all', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([{ orgId: 'org-1' }]))
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'engineer' }]))
			.mockImplementationOnce((resolve: any) => resolve([])) // no grant rows: unrestricted
			.mockImplementationOnce((resolve: any) =>
				resolve([{ id: 'project-org-wide', name: 'B', organizationId: 'org-1' }])
			) // by org role — the half that used not to exist
			.mockImplementationOnce((resolve: any) => resolve([])); // by project role: none

		issueQueries.searchIssuesInOrg.mockResolvedValueOnce([
			{ id: 'issue-a', projectId: 'project-org-wide', message: 'boom a' },
		]);

		const { url, locals: l } = req({ q: 'boom', issueId: 'issue-1' });
		const res = await GET({ url, locals: l } as any);
		const body = await res.json();

		expect(body.issues).toEqual([
			{ id: 'issue-a', projectId: 'project-org-wide', message: 'boom a' },
		]);
	});

	// sentinel-project-environments §2.2, decision 25: the route hands `searchIssuesInOrg` the
	// caller's per-project scopes. The predicate itself is a query-shape claim asserted at
	// `environmentScopeCondition`'s own boundary; what this route owns is passing the scopes at all,
	// and a route that silently dropped them would leave the guard unreachable.
	it('passes the resolved per-project environment scopes to searchIssuesInOrg', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([{ orgId: 'org-1' }]))
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'engineer' }]))
			.mockImplementationOnce((resolve: any) =>
				resolve([{ projectId: 'project-scoped', environments: ['staging'] }])
			)
			.mockImplementationOnce((resolve: any) => resolve([]))
			.mockImplementationOnce((resolve: any) => resolve([]));

		issueQueries.searchIssuesInOrg.mockResolvedValueOnce([]);

		const { url, locals: l } = req({ q: 'boom', issueId: 'issue-1' });
		await GET({ url, locals: l } as any);

		expect(issueQueries.searchIssuesInOrg).toHaveBeenCalledWith(
			'org-1',
			'boom',
			'issue-1',
			'system_error',
			new Map([['project-scoped', ['staging']]])
		);
	});

	it('returns an empty list when the caller can reach no project at all', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([{ orgId: 'org-1' }]))
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'admin' }]))
			.mockImplementationOnce((resolve: any) => resolve([]))
			.mockImplementationOnce((resolve: any) => resolve([]))
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
