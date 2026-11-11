import { describe, it, expect, vi, beforeEach } from 'vitest';

const requireOrgMembership = vi.fn();
vi.mock('../keys/_shared', () => ({ requireOrgMembership: (...a: any[]) => requireOrgMembership(...a) }));

const createProject = vi.fn();
vi.mock('$lib/db/queries/organizations', () => ({ createProject: (...a: any[]) => createProject(...a) }));

// sentinel-project-environments §4.10, decision 31: GET's population is the accessible union, not
// the organization's whole project table.
const getAccessibleProjects = vi.fn();
vi.mock('$lib/server/issue-access', () => ({
	getAccessibleProjects: (...a: any[]) => getAccessibleProjects(...a),
}));

const dbMock: any = { select: vi.fn(), from: vi.fn(), where: vi.fn() };
dbMock.select.mockReturnValue(dbMock);
dbMock.from.mockReturnValue(dbMock);
dbMock.where.mockResolvedValue([{ id: 'proj-1' }]);
vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	projects: { id: 'id', name: 'name', createdAt: 'createdAt', organizationId: 'organizationId', isInbox: 'isInbox' },
}));

const { GET, POST } = await import('./+server');

function event(userId: string | null, body?: unknown) {
	return {
		params: { orgId: 'org-1' },
		locals: { auth: vi.fn().mockResolvedValue(userId ? { user: { id: userId } } : null) },
		request: new Request('https://sentinel.test/api/organizations/org-1/projects', {
			method: 'POST',
			body: JSON.stringify(body ?? {}),
		}),
	} as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	requireOrgMembership.mockResolvedValue({ role: 'owner' });
	createProject.mockResolvedValue({ id: 'proj-1', name: 'Web app' });
	getAccessibleProjects.mockResolvedValue([
		{ id: 'proj-1', name: 'Web app', organizationId: 'org-1' },
	]);
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockResolvedValue([{ id: 'proj-1' }]);
});

describe('POST /api/organizations/[orgId]/projects', () => {
	it('creates the project as the SESSION user', async () => {
		const res = await POST(event('user-1', { name: 'Web app' }));

		expect(res.status).toBe(201);
		expect(createProject).toHaveBeenCalledWith('user-1', 'org-1', { name: 'Web app' });
	});

	it('401s an anonymous caller', async () => {
		await expect(POST(event(null, { name: 'Web app' }))).rejects.toMatchObject({ status: 401 });
		expect(createProject).not.toHaveBeenCalled();
	});

	it('403s a caller who is not a member of the organization', async () => {
		requireOrgMembership.mockResolvedValue(undefined);

		await expect(POST(event('user-1', { name: 'Web app' }))).rejects.toMatchObject({ status: 403 });
		expect(createProject).not.toHaveBeenCalled();
	});

	it('403s a viewer, who has no write permission', async () => {
		requireOrgMembership.mockResolvedValue({ role: 'viewer' });

		await expect(POST(event('user-1', { name: 'Web app' }))).rejects.toMatchObject({ status: 403 });
		expect(createProject).not.toHaveBeenCalled();
	});

	it('400s a missing or blank name', async () => {
		await expect(POST(event('user-1', {}))).rejects.toMatchObject({ status: 400 });
		await expect(POST(event('user-1', { name: '   ' }))).rejects.toMatchObject({ status: 400 });
		expect(createProject).not.toHaveBeenCalled();
	});
});

describe('GET /api/organizations/[orgId]/projects', () => {
	it('lists the organization\'s projects for a member', async () => {
		const res = await GET(event('user-1'));

		expect(res.status).toBe(200);
		expect(await res.json()).toEqual({ projects: [{ id: 'proj-1', name: 'Web app' }] });
	});

	it('401s an anonymous caller', async () => {
		await expect(GET(event(null))).rejects.toMatchObject({ status: 401 });
		expect(getAccessibleProjects).not.toHaveBeenCalled();
	});

	it('403s a caller who is not a member of the organization', async () => {
		requireOrgMembership.mockResolvedValue(undefined);

		await expect(GET(event('user-1'))).rejects.toMatchObject({ status: 403 });
		expect(getAccessibleProjects).not.toHaveBeenCalled();
	});

	// Decision 31: the population is the accessible union scoped to THIS organization, resolved from
	// the session user — not `select ... where organization_id = :orgId`. Delete the
	// `getAccessibleProjects` call and this fails: nothing else derives the id set the query filters
	// on.
	it('derives the list from the caller\'s accessible union, scoped to the organization', async () => {
		await GET(event('user-1'));

		expect(getAccessibleProjects).toHaveBeenCalledWith('user-1', 'org-1');
	});

	// A caller with no accessible project in this organization gets an empty list without a second
	// query — `inArray(id, [])` is not a filter this endpoint should ever emit.
	it('returns an empty list, and queries nothing, when the union is empty', async () => {
		getAccessibleProjects.mockResolvedValue([]);

		const res = await GET(event('user-1'));

		expect(await res.json()).toEqual({ projects: [] });
		expect(dbMock.select).not.toHaveBeenCalled();
	});

	// Inbox projects stay excluded, as they were when this listed the table directly: they are the
	// landing place for unassigned reports, not something anyone chose to create.
	it('excludes inbox projects from the union it returns', async () => {
		getAccessibleProjects.mockResolvedValue([
			{ id: 'proj-1', name: 'Web app', organizationId: 'org-1' },
			{ id: 'inbox-1', name: 'Triage', organizationId: 'org-1' },
		]);
		dbMock.where.mockResolvedValue([{ id: 'proj-1' }]);

		const res = await GET(event('user-1'));

		expect(await res.json()).toEqual({
			projects: [{ id: 'proj-1', name: 'Web app' }],
		});
		expect(dbMock.where).toHaveBeenCalledTimes(1);
	});

	// Decision 31: `{ id, name }` and nothing else, in the resolver's name order -- the picker renders
	// it as-is. The inbox query has no ORDER BY and its rows are not the response: the 2026-09-10 merge
	// returned them directly, which put `createdAt` back and the order at the planner's mercy (red on
	// every push since in environments.integration.test.ts).
	it('returns { id, name } only, in getAccessibleProjects\' name order, whatever order the inbox filter reads', async () => {
		getAccessibleProjects.mockResolvedValue([
			{ id: 'proj-a', name: 'Alpha', organizationId: 'org-1' },
			{ id: 'proj-b', name: 'Beta', organizationId: 'org-1' },
			{ id: 'proj-c', name: 'Gamma', organizationId: 'org-1' },
		]);
		dbMock.where.mockResolvedValue([{ id: 'proj-c' }, { id: 'proj-a' }, { id: 'proj-b' }]);

		const res = await GET(event('user-1'));

		expect(await res.json()).toEqual({
			projects: [
				{ id: 'proj-a', name: 'Alpha' },
				{ id: 'proj-b', name: 'Beta' },
				{ id: 'proj-c', name: 'Gamma' },
			],
		});
	});
});
