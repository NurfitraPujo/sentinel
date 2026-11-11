// @vitest-environment node
import { describe, it, expect, vi, beforeEach } from 'vitest';

const dbMock: any = { select: vi.fn(), from: vi.fn(), where: vi.fn() };
dbMock.select.mockReturnValue(dbMock);
dbMock.from.mockReturnValue(dbMock);
vi.mock('$lib/server/db', () => ({ db: dbMock }));
vi.mock('$lib/db/schema', () => ({
	projects: { id: 'id', name: 'name', createdAt: 'createdAt', organizationId: 'organizationId', isInbox: 'isInbox' },
}));

const shared = { requireOrgMembership: vi.fn() };
vi.mock('../../api/organizations/[orgId=uuid]/keys/_shared', () => shared);

const queries = { createProject: vi.fn() };
vi.mock('$lib/db/queries/organizations', () => queries);

const { load, actions } = await import('./+page.server');

function event(session: unknown, currentOrg: unknown, orgSlug = 'acme') {
	return {
		params: { orgSlug },
		locals: { auth: vi.fn().mockResolvedValue(session), currentOrg },
	} as any;
}

function postEvent(fields: Record<string, string>, session: unknown = SESSION, currentOrg: unknown = ORG, orgSlug = 'acme') {
	return {
		...event(session, currentOrg, orgSlug),
		request: new Request('https://sentinel.test/acme/projects?/create', {
			method: 'POST',
			headers: { 'content-type': 'application/x-www-form-urlencoded' },
			body: new URLSearchParams(fields),
		}),
	};
}

const ORG = { id: 'org-1', name: 'Acme', slug: 'acme' };
const SESSION = { user: { id: 'user-1', email: 'engineer@example.com' } };

beforeEach(() => {
	vi.clearAllMocks();
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockResolvedValue([{ id: 'proj-1', name: 'Web app', createdAt: null }]);
	shared.requireOrgMembership.mockResolvedValue({ role: 'engineer' });
	queries.createProject.mockResolvedValue({ id: 'proj-2', name: 'Mobile app' });
});

describe('/[orgSlug]/projects', () => {
	it('returns the org and its projects for a member', async () => {
		expect(await load(event(SESSION, ORG))).toEqual({
			org: { name: 'Acme', slug: 'acme' },
			projects: [{ id: 'proj-1', name: 'Web app', createdAt: null }],
			canCreate: true,
		});
		expect(shared.requireOrgMembership).toHaveBeenCalledWith('user-1', 'org-1');
	});

	it.each(['owner', 'admin', 'engineer'])('canCreate is true for an org %s', async (role) => {
		shared.requireOrgMembership.mockResolvedValue({ role });
		expect(((await load(event(SESSION, ORG))) as any).canCreate).toBe(true);
	});

	it.each(['viewer', 'support'])('canCreate is false for an org %s', async (role) => {
		shared.requireOrgMembership.mockResolvedValue({ role });
		expect(((await load(event(SESSION, ORG))) as any).canCreate).toBe(false);
	});

	it('canCreate is false when the caller has no membership row', async () => {
		shared.requireOrgMembership.mockResolvedValue(undefined);
		expect(((await load(event(SESSION, ORG))) as any).canCreate).toBe(false);
	});

	// D39: authenticate before any lookup that reveals whether an organization exists, so an
	// anonymous caller cannot tell a real slug from a made-up one.
	it('401s an anonymous caller without querying anything', async () => {
		await expect(load(event(null, ORG))).rejects.toMatchObject({ status: 401 });
		expect(dbMock.select).not.toHaveBeenCalled();
		expect(shared.requireOrgMembership).not.toHaveBeenCalled();
	});

	it('403s when the resolved org is not the slug in the URL', async () => {
		await expect(load(event(SESSION, ORG, 'someone-else'))).rejects.toMatchObject({ status: 403 });
		expect(dbMock.select).not.toHaveBeenCalled();
	});

	it('403s when no org resolved at all', async () => {
		await expect(load(event(SESSION, undefined))).rejects.toMatchObject({ status: 403 });
	});
});

describe('/[orgSlug]/projects ?/create', () => {
	it('creates the project for the SESSION user in the RESOLVED org, not anything in the body', async () => {
		const result: any = await actions.create(postEvent({ name: '  Mobile app  ', organizationId: 'org-evil' }));

		expect(queries.createProject).toHaveBeenCalledWith('user-1', 'org-1', { name: 'Mobile app' });
		expect(result).toEqual({ created: { id: 'proj-2', name: 'Mobile app' } });
	});

	it.each(['owner', 'admin', 'engineer'])('lets an org %s create', async (role) => {
		shared.requireOrgMembership.mockResolvedValue({ role });
		const result: any = await actions.create(postEvent({ name: 'Mobile app' }));
		expect(result.created).toBeDefined();
		expect(queries.createProject).toHaveBeenCalledTimes(1);
	});

	it.each(['viewer', 'support'])('403s an org %s without touching the database', async (role) => {
		shared.requireOrgMembership.mockResolvedValue({ role });
		const result: any = await actions.create(postEvent({ name: 'Mobile app' }));

		expect(result.status).toBe(403);
		expect(result.data.error).toMatch(/permission/);
		expect(queries.createProject).not.toHaveBeenCalled();
	});

	it('403s a caller with no membership row', async () => {
		shared.requireOrgMembership.mockResolvedValue(undefined);
		const result: any = await actions.create(postEvent({ name: 'Mobile app' }));

		expect(result.status).toBe(403);
		expect(queries.createProject).not.toHaveBeenCalled();
	});

	it('400s an empty name', async () => {
		const result: any = await actions.create(postEvent({ name: '   ' }));

		expect(result.status).toBe(400);
		expect(result.data.error).toBe('Project name is required');
		expect(queries.createProject).not.toHaveBeenCalled();
	});

	it('400s a missing name field', async () => {
		const result: any = await actions.create(postEvent({}));

		expect(result.status).toBe(400);
		expect(queries.createProject).not.toHaveBeenCalled();
	});

	it('400s a name longer than 255 characters and echoes it back', async () => {
		const name = 'x'.repeat(256);
		const result: any = await actions.create(postEvent({ name }));

		expect(result.status).toBe(400);
		expect(result.data.error).toBe('Project name must be 255 characters or fewer');
		expect(result.data.values.name).toBe(name);
		expect(queries.createProject).not.toHaveBeenCalled();
	});

	it('accepts a name of exactly 255 characters', async () => {
		const name = 'x'.repeat(255);
		const result: any = await actions.create(postEvent({ name }));

		expect(result.status).toBeUndefined();
		expect(queries.createProject).toHaveBeenCalledWith('user-1', 'org-1', { name });
	});

	// BUG-01: `projects` is UNIQUE (organization_id, name) and every org with a report inbox
	// already owns a hidden 'Triage' project, so a collision is an ordinary user input error.
	it('409s a duplicate name and echoes it back instead of throwing', async () => {
		queries.createProject.mockRejectedValue(
			new Error('duplicate key value violates unique constraint "projects_organization_id_name_unique"'),
		);
		const result: any = await actions.create(postEvent({ name: 'Triage' }));

		expect(result.status).toBe(409);
		expect(result.data.error).toBe("A project named 'Triage' already exists in Acme");
		expect(result.data.values.name).toBe('Triage');
	});

	it('rethrows a non-unique createProject failure', async () => {
		queries.createProject.mockRejectedValue(new Error('connection refused'));
		await expect(actions.create(postEvent({ name: 'Mobile app' }))).rejects.toThrow('connection refused');
	});

	it('401s an anonymous caller before reading the body or the membership', async () => {
		await expect(actions.create(postEvent({ name: 'Mobile app' }, null))).rejects.toMatchObject({ status: 401 });
		expect(shared.requireOrgMembership).not.toHaveBeenCalled();
		expect(queries.createProject).not.toHaveBeenCalled();
	});

	it('403s when the resolved org is not the slug in the URL', async () => {
		await expect(actions.create(postEvent({ name: 'Mobile app' }, SESSION, ORG, 'someone-else'))).rejects.toMatchObject({
			status: 403,
		});
		expect(queries.createProject).not.toHaveBeenCalled();
	});
});
