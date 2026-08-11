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
		dbMock.then.mockReset();
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
	});

	it('401s when there is no session', async () => {
		await expect(
			PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'resolved' }),
				locals: locals(null),
				url: new URL('http://x'),
			} as any)
		).rejects.toMatchObject({ status: 401 });
	});

	it('400s on an invalid status', async () => {
		await expect(
			PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'bogus' }),
				locals: locals({ id: 'user-1' }),
				url: new URL('http://x'),
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
				url: new URL('http://x'),
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
				url: new URL('http://x'),
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
	// This previously asserted that an org ADMIN with no `project_members` row was refused. That
	// model was wrong and tests/e2e U13 proved it: an org admin could not link two issues in their
	// own organization, because `project_members` is populated only for per-project grants, not for
	// org-level staff. An org role on the write allowlist is itself an org-wide grant.
	it('allows an org admin with no project_members row — an org role is an org-wide grant', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([issueRow])) // issue+project lookup
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'admin' }])); // org role, no project query

		issueQueries.updateIssueStatus.mockResolvedValueOnce([]);

		await PATCH({
			params: { issueId: 'issue-1' },
			request: patchRequest({ status: 'resolved' }),
			locals: locals({ id: 'user-1' }),
			url: new URL('http://x'),
		} as any);

		expect(issueQueries.updateIssueStatus).toHaveBeenCalled();
	});

	// D10's actual hole, still closed: an org `viewer` gets no write access from the org role, and
	// with no project membership either, is refused outright.
	it('403s an org viewer with no project membership', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([issueRow])) // issue+project lookup
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'viewer' }])) // org role
			.mockImplementationOnce((resolve: any) => resolve([])); // no project membership row

		await expect(
			PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'resolved' }),
				locals: locals({ id: 'user-1' }),
				url: new URL('http://x'),
			} as any)
		).rejects.toMatchObject({ status: 403 });
		expect(issueQueries.updateIssueStatus).not.toHaveBeenCalled();
	});

	// A project grant conveys READ only — it must never become a backdoor to writing, or the
	// single-issue path would again be more permissive than the bulk one (D23).
	it('403s an org viewer who IS a project member — project grants do not convey write', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([issueRow])) // issue+project lookup
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'viewer' }])) // org role
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'developer' }])); // project member

		await expect(
			PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'resolved' }),
				locals: locals({ id: 'user-1' }),
				url: new URL('http://x'),
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
				url: new URL('http://x'),
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
			url: new URL('http://x'),
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
