import { describe, it, expect, vi, beforeEach } from 'vitest';

// N10 part 1 (docs/plans/AGENT_WORKER_PLAN.md rev 4 SS4.5): route-level tests for the two
// project-scoped agent-settings routes, mirroring agents.test.ts's db-double approach. The db
// mock backs both `requireOrgMembership` (../keys/_shared.ts) and the project-existence lookup
// each route does directly; the actual settings/repo read+write logic is mocked at the
// $lib/db/queries/agent-settings module boundary (already covered by agent-settings.test.ts) so
// these tests only need to prove: permission denied -> 403 + no mutation, validation errors ->
// 400 + no mutation, an audit_logs row is written on every successful mutation, and the happy
// path 200s with the expected body.

function makeDbMock() {
	const dbMock: any = {
		select: vi.fn(),
		from: vi.fn(),
		where: vi.fn(),
		insert: vi.fn(),
		values: vi.fn(),
		then: vi.fn(),
	};
	for (const key of ['select', 'from', 'where', 'insert', 'values']) {
		dbMock[key].mockReturnValue(dbMock);
	}
	dbMock.then.mockReset();
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
	return dbMock;
}

const dbMock = makeDbMock();

vi.mock('$lib/server/db', () => ({ db: dbMock }));
vi.mock('$lib/db/schema', () => ({
	organizationMembers: { organizationId: 'organizationId', userId: 'userId', role: 'role' },
	projects: { id: 'id', organizationId: 'organizationId', name: 'name' },
	auditLogs: { action: 'action', resourceType: 'resourceType', resourceId: 'resourceId', actorId: 'actorId', metadata: 'metadata' },
}));

class FakeValidationError extends Error {}

const agentSettingsQueries = {
	getProjectAgentSettings: vi.fn(),
	upsertProjectAgentSettings: vi.fn(),
	getRepoConnection: vi.fn(),
	upsertRepoConnection: vi.fn(),
	deleteRepoConnection: vi.fn(),
	AgentSettingsValidationError: FakeValidationError,
};
vi.mock('$lib/db/queries/agent-settings', () => agentSettingsQueries);

const { GET, PUT } = await import('./[orgId]/projects/[projectId]/agent-settings/+server');
const {
	GET: repoGET,
	PUT: repoPUT,
	DELETE: repoDELETE,
} = await import('./[orgId]/projects/[projectId]/repo-connection/+server');

function locals(session: { id: string } | null) {
	return { auth: async () => (session ? { user: { id: session.id } } : null) } as any;
}

function membershipRow(role: string) {
	return { role };
}

function projectRow(id = 'proj-1') {
	return { id };
}

const PARAMS = { orgId: 'org-1', projectId: 'proj-1' };

describe('project agent-settings routes', () => {
	beforeEach(() => {
		vi.clearAllMocks();
		for (const key of ['select', 'from', 'where', 'insert', 'values']) {
			dbMock[key].mockReturnValue(dbMock);
		}
		dbMock.then.mockReset();
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
	});

	describe('GET /agent-settings', () => {
		it('401s when there is no session', async () => {
			await expect(GET({ params: PARAMS, locals: locals(null) } as any)).rejects.toMatchObject({ status: 401 });
		});

		it('403s when the caller is not permitted (engineer)', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('engineer')]));
			await expect(GET({ params: PARAMS, locals: locals({ id: 'user-1' }) } as any)).rejects.toMatchObject({
				status: 403,
			});
			expect(agentSettingsQueries.getProjectAgentSettings).not.toHaveBeenCalled();
		});

		it('404s when the project is not in this organization', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([]));
			await expect(GET({ params: PARAMS, locals: locals({ id: 'user-1' }) } as any)).rejects.toMatchObject({
				status: 404,
			});
			expect(agentSettingsQueries.getProjectAgentSettings).not.toHaveBeenCalled();
		});

		it('200s with defaults when no settings row exists yet', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('admin')]));
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([projectRow()]));
			agentSettingsQueries.getProjectAgentSettings.mockResolvedValueOnce(null);

			const res = await GET({ params: PARAMS, locals: locals({ id: 'user-1' }) } as any);
			const body = await res.json();
			expect(body).toEqual({ settings: { projectId: 'proj-1', fixEnabled: false, maxPrsPerDay: null } });
		});
	});

	describe('PUT /agent-settings', () => {
		it('403s for a viewer and does not write settings or an audit row', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('viewer')]));
			const request = new Request('http://x', { method: 'PUT', body: JSON.stringify({ fixEnabled: true }) });
			await expect(PUT({ params: PARAMS, request, locals: locals({ id: 'user-1' }) } as any)).rejects.toMatchObject({
				status: 403,
			});
			expect(agentSettingsQueries.upsertProjectAgentSettings).not.toHaveBeenCalled();
			expect(dbMock.insert).not.toHaveBeenCalled();
		});

		it('400s when the query layer rejects the input, and writes no audit row', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([projectRow()]));
			agentSettingsQueries.getProjectAgentSettings.mockResolvedValueOnce(null);
			agentSettingsQueries.upsertProjectAgentSettings.mockRejectedValueOnce(new FakeValidationError('bad input'));

			const request = new Request('http://x', {
				method: 'PUT',
				body: JSON.stringify({ fixEnabled: true, maxPrsPerDay: -1 }),
			});
			await expect(PUT({ params: PARAMS, request, locals: locals({ id: 'user-1' }) } as any)).rejects.toMatchObject({
				status: 400,
			});
			expect(dbMock.insert).not.toHaveBeenCalled();
		});

		it('200s for an owner, upserts, and writes an audit_logs row with before/after', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([projectRow()]));
			agentSettingsQueries.getProjectAgentSettings.mockResolvedValueOnce({ fixEnabled: false, maxPrsPerDay: null });
			agentSettingsQueries.upsertProjectAgentSettings.mockResolvedValueOnce({
				projectId: 'proj-1',
				fixEnabled: true,
				maxPrsPerDay: 5,
			});

			const request = new Request('http://x', {
				method: 'PUT',
				body: JSON.stringify({ fixEnabled: true, maxPrsPerDay: 5 }),
			});
			const res = await PUT({ params: PARAMS, request, locals: locals({ id: 'user-1' }) } as any);
			const body = await res.json();
			expect(body).toEqual({ settings: { projectId: 'proj-1', fixEnabled: true, maxPrsPerDay: 5 } });

			expect(dbMock.insert).toHaveBeenCalledWith({
				action: 'action',
				resourceType: 'resourceType',
				resourceId: 'resourceId',
				actorId: 'actorId',
				metadata: 'metadata',
			});
			const inserted = dbMock.values.mock.calls[0][0];
			expect(inserted.action).toBe('agent_settings.updated');
			expect(inserted.actorId).toBe('user-1');
			expect(inserted.resourceId).toBe('proj-1');
			expect(inserted.metadata.before).toEqual({ fixEnabled: false, maxPrsPerDay: null });
			expect(inserted.metadata.after).toEqual({ fixEnabled: true, maxPrsPerDay: 5 });
		});
	});

	describe('GET/PUT/DELETE /repo-connection', () => {
		it('403s for a support member on PUT and writes nothing', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('support')]));
			const request = new Request('http://x', {
				method: 'PUT',
				body: JSON.stringify({ provider: 'github', owner: 'a', repo: 'b', defaultBranch: 'main', testCmd: 'pnpm test' }),
			});
			await expect(repoPUT({ params: PARAMS, request, locals: locals({ id: 'user-1' }) } as any)).rejects.toMatchObject({
				status: 403,
			});
			expect(agentSettingsQueries.upsertRepoConnection).not.toHaveBeenCalled();
			expect(dbMock.insert).not.toHaveBeenCalled();
		});

		it('400s when validation fails on PUT', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([projectRow()]));
			agentSettingsQueries.getRepoConnection.mockResolvedValueOnce(null);
			agentSettingsQueries.upsertRepoConnection.mockRejectedValueOnce(new FakeValidationError('bad'));

			const request = new Request('http://x', { method: 'PUT', body: JSON.stringify({ provider: 'gitlab' }) });
			await expect(repoPUT({ params: PARAMS, request, locals: locals({ id: 'user-1' }) } as any)).rejects.toMatchObject({
				status: 400,
			});
			expect(dbMock.insert).not.toHaveBeenCalled();
		});

		it('200s on PUT for an admin, writes an audit row distinguishing created vs updated', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('admin')]));
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([projectRow()]));
			agentSettingsQueries.getRepoConnection.mockResolvedValueOnce(null);
			agentSettingsQueries.upsertRepoConnection.mockResolvedValueOnce({
				projectId: 'proj-1',
				provider: 'github',
				owner: 'acme',
				repo: 'widgets',
				defaultBranch: 'main',
				testCmd: 'pnpm test',
				agentCmd: null,
				cloneDepth: null,
			});

			const request = new Request('http://x', {
				method: 'PUT',
				body: JSON.stringify({
					provider: 'github',
					owner: 'acme',
					repo: 'widgets',
					defaultBranch: 'main',
					testCmd: 'pnpm test',
				}),
			});
			const res = await repoPUT({ params: PARAMS, request, locals: locals({ id: 'user-1' }) } as any);
			expect(res.status).toBe(200);
			const inserted = dbMock.values.mock.calls[0][0];
			expect(inserted.action).toBe('agent_repo_connection.created');
			expect(inserted.metadata.before).toBeNull();
			expect(inserted.metadata.after).toEqual({
				provider: 'github',
				owner: 'acme',
				repo: 'widgets',
				defaultBranch: 'main',
			});
		});

		it('404s on DELETE when there is nothing to disconnect', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([projectRow()]));
			agentSettingsQueries.getRepoConnection.mockResolvedValueOnce(null);

			await expect(repoDELETE({ params: PARAMS, locals: locals({ id: 'user-1' }) } as any)).rejects.toMatchObject({
				status: 404,
			});
			expect(agentSettingsQueries.deleteRepoConnection).not.toHaveBeenCalled();
		});

		it('200s on DELETE and writes an audit row with the deleted connection', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([membershipRow('owner')]));
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([projectRow()]));
			agentSettingsQueries.getRepoConnection.mockResolvedValueOnce({
				provider: 'github',
				owner: 'acme',
				repo: 'widgets',
				defaultBranch: 'main',
			});

			const res = await repoDELETE({ params: PARAMS, locals: locals({ id: 'user-1' }) } as any);
			expect(res.status).toBe(200);
			expect(agentSettingsQueries.deleteRepoConnection).toHaveBeenCalledWith('proj-1');
			const inserted = dbMock.values.mock.calls[0][0];
			expect(inserted.action).toBe('agent_repo_connection.deleted');
			expect(inserted.metadata.before).toEqual({
				provider: 'github',
				owner: 'acme',
				repo: 'widgets',
				defaultBranch: 'main',
			});
		});

		it('403s on GET for a non-member', async () => {
			dbMock.then.mockImplementationOnce((resolve: any) => resolve([]));
			await expect(repoGET({ params: PARAMS, locals: locals({ id: 'user-1' }) } as any)).rejects.toMatchObject({
				status: 403,
			});
		});
	});
});
