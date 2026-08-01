import { describe, it, expect, vi, beforeEach } from 'vitest';

// Chainable Drizzle-query double, following the pattern in status.test.ts / keys.test.ts: each
// awaited chain resolves via the queued `then` implementation below, in the order the route code
// issues its queries.
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

vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	issues: { id: 'id', projectId: 'projectId' },
	projects: { id: 'id', organizationId: 'organizationId' },
	organizationMembers: { organizationId: 'organizationId', userId: 'userId', role: 'role' },
	issueRelations: {
		id: 'id',
		sourceIssueId: 'sourceIssueId',
		targetIssueId: 'targetIssueId',
		relationType: 'relationType',
	},
}));

const issueQueries = {
	createIssueRelation: vi.fn(),
	deleteIssueRelation: vi.fn(),
};
vi.mock('$lib/db/queries/issues', () => issueQueries);

// D10: this endpoint used to gate on a bare `organization_members` existence check — no role, no
// project membership — so an org `viewer` who could not resolve issues via the batch endpoint
// could still create and delete relations here. It now delegates to `requireIssueAccess`, whose
// own behaviour (role table, project membership, 404 vs 403) is tested in
// src/lib/server/issue-access.test.ts. Mocked here so these tests stay focused on whether this
// route ASKS for the right permission on the right issue.
const issueAccess = { requireIssueAccess: vi.fn() };
vi.mock('$lib/server/issue-access', () => issueAccess);

const { POST, DELETE } = await import('./[issueId]/relations/+server');

function locals(session: { id: string } | null) {
	return { auth: async () => (session ? { user: { id: session.id } } : null) } as any;
}

function postRequest(body: unknown) {
	return new Request('http://x', { method: 'POST', body: JSON.stringify(body) });
}

function malformedRequest() {
	// A body that fails request.json() parsing.
	return new Request('http://x', { method: 'POST', body: '{not valid json' });
}

const sourceRow = { issueId: 'issue-1', orgId: 'org-1' };
const targetRow = { issueId: 'issue-2', orgId: 'org-1' };

describe('POST /api/issues/:id/relations', () => {
	beforeEach(() => {
		// resetAllMocks, not clearAllMocks: `clear` wipes call records but NOT queued
		// `mockImplementationOnce`/`mockRejectedValueOnce` entries, so an un-consumed queued
		// resolution leaks into the next test and resolves the wrong query.
		vi.resetAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.innerJoin.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.then.mockReset();
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
		// Default: caller is authorized. Individual tests override to assert denial propagates.
		issueAccess.requireIssueAccess.mockResolvedValue({
			issueId: 'issue-1',
			projectId: 'proj-1',
			organizationId: 'org-1',
		});
	});

	// D40: malformed JSON body must 400, not 500 (request.json() previously had no .catch()).
	it('400s on a malformed JSON body instead of throwing an unhandled error', async () => {
		await expect(
			POST({
				params: { issueId: 'issue-1' },
				request: malformedRequest(),
				locals: locals({ id: 'user-1' }),
			} as any)
		).rejects.toMatchObject({ status: 400 });
		expect(issueQueries.createIssueRelation).not.toHaveBeenCalled();
	});

	// D22: a direct POST relating an issue to itself must 400, not silently insert.
	it('400s a self-relation (sourceIssueId === targetIssueId)', async () => {
		await expect(
			POST({
				params: { issueId: 'issue-1' },
				request: postRequest({ targetIssueId: 'issue-1', relationType: 'duplicate_of' }),
				locals: locals({ id: 'user-1' }),
			} as any)
		).rejects.toMatchObject({ status: 400 });
		expect(issueQueries.createIssueRelation).not.toHaveBeenCalled();
	});

	// D22: re-linking an already-existing relation surfaces the DB's 23505 unique_violation as a
	// 409, not an unhandled 500.
	it('409s a re-link that violates issue_relations_unique', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([sourceRow])) // source issue+project lookup
			.mockImplementationOnce((resolve: any) => resolve([targetRow])) // target issue+project lookup
			.mockImplementationOnce((resolve: any) => resolve([])); // duplicate_of inverse-cycle check: none

		const dbError = Object.assign(new Error('duplicate key value violates unique constraint'), {
			code: '23505',
		});
		issueQueries.createIssueRelation.mockRejectedValueOnce(dbError);

		await expect(
			POST({
				params: { issueId: 'issue-1' },
				request: postRequest({ targetIssueId: 'issue-2', relationType: 'duplicate_of' }),
				locals: locals({ id: 'user-1' }),
			} as any)
		).rejects.toMatchObject({ status: 409 });
	});

	// D22: A duplicate_of B already exists; POSTing B duplicate_of A must 400 rather than create a
	// 2-cycle that the UI would read as mutually-duplicate issues.
	it('400s a duplicate_of 2-cycle', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([sourceRow])) // source issue+project lookup (issue-2)
			.mockImplementationOnce((resolve: any) => resolve([targetRow])) // target issue+project lookup (issue-1)
			.mockImplementationOnce((resolve: any) =>
				resolve([{ id: 'rel-1', sourceIssueId: 'issue-1', targetIssueId: 'issue-2', relationType: 'duplicate_of' }])
			); // inverse relation exists: issue-1 duplicate_of issue-2

		await expect(
			POST({
				params: { issueId: 'issue-2' },
				request: postRequest({ targetIssueId: 'issue-1', relationType: 'duplicate_of' }),
				locals: locals({ id: 'user-1' }),
			} as any)
		).rejects.toMatchObject({ status: 400 });
		expect(issueQueries.createIssueRelation).not.toHaveBeenCalled();
	});

	// D10 regression fence. These fail if the `requireIssueAccess` calls are ever removed and the
	// endpoint reverts to a bare org-membership existence check — which is exactly what shipped.
	it('demands write on the source issue and read on the target before creating a relation', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([sourceRow]))
			.mockImplementationOnce((resolve: any) => resolve([targetRow]));
		issueQueries.createIssueRelation.mockResolvedValueOnce({ id: 'rel-1' });

		await POST({
			params: { issueId: 'issue-1' },
			request: postRequest({ targetIssueId: 'issue-2', relationType: 'linked_to' }),
			locals: locals({ id: 'user-1' }),
		} as any);

		expect(issueAccess.requireIssueAccess).toHaveBeenCalledWith('user-1', 'issue-1', 'write');
		expect(issueAccess.requireIssueAccess).toHaveBeenCalledWith('user-1', 'issue-2', 'read');
	});

	it('propagates a 403 from requireIssueAccess and does not create the relation', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([sourceRow]))
			.mockImplementationOnce((resolve: any) => resolve([targetRow]));
		// What an org `viewer` now gets: requireIssueAccess rejects on the write permission.
		issueAccess.requireIssueAccess.mockRejectedValueOnce(
			Object.assign(new Error('Forbidden'), { status: 403 })
		);

		await expect(
			POST({
				params: { issueId: 'issue-1' },
				request: postRequest({ targetIssueId: 'issue-2', relationType: 'duplicate_of' }),
				locals: locals({ id: 'viewer-1' }),
			} as any)
		).rejects.toMatchObject({ status: 403 });
		expect(issueQueries.createIssueRelation).not.toHaveBeenCalled();
	});
});

describe('DELETE /api/issues/:id/relations (D10)', () => {
	beforeEach(() => {
		// resetAllMocks, not clearAllMocks: `clear` wipes call records but NOT queued
		// `mockImplementationOnce`/`mockRejectedValueOnce` entries, so an un-consumed queued
		// resolution leaks into the next test and resolves the wrong query.
		vi.resetAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.innerJoin.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.then.mockReset();
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
		issueAccess.requireIssueAccess.mockResolvedValue({
			issueId: 'issue-1',
			projectId: 'proj-1',
			organizationId: 'org-1',
		});
	});

	it('demands write on the source issue before unlinking', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([sourceRow]))
			.mockImplementationOnce((resolve: any) => resolve([targetRow]));
		issueQueries.deleteIssueRelation.mockResolvedValueOnce(true);

		await DELETE({
			params: { issueId: 'issue-1' },
			request: postRequest({ targetIssueId: 'issue-2', relationType: 'linked_to' }),
			locals: locals({ id: 'user-1' }),
		} as any);

		expect(issueAccess.requireIssueAccess).toHaveBeenCalledWith('user-1', 'issue-1', 'write');
	});

	it('propagates a 403 and does not delete the relation', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([sourceRow]))
			.mockImplementationOnce((resolve: any) => resolve([targetRow]));
		issueAccess.requireIssueAccess.mockRejectedValueOnce(
			Object.assign(new Error('Forbidden'), { status: 403 })
		);

		await expect(
			DELETE({
				params: { issueId: 'issue-1' },
				request: postRequest({ targetIssueId: 'issue-2', relationType: 'linked_to' }),
				locals: locals({ id: 'viewer-1' }),
			} as any)
		).rejects.toMatchObject({ status: 403 });
		expect(issueQueries.deleteIssueRelation).not.toHaveBeenCalled();
	});
});
