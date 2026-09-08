import { describe, it, expect, vi, beforeEach } from 'vitest';

const requireOrgMembership = vi.fn();
vi.mock('../keys/_shared', () => ({ requireOrgMembership: (...a: any[]) => requireOrgMembership(...a) }));

const createProject = vi.fn();
vi.mock('$lib/db/queries/organizations', () => ({ createProject: (...a: any[]) => createProject(...a) }));

const dbMock: any = { select: vi.fn(), from: vi.fn(), where: vi.fn() };
dbMock.select.mockReturnValue(dbMock);
dbMock.from.mockReturnValue(dbMock);
dbMock.where.mockResolvedValue([{ id: 'proj-1', name: 'Web app', createdAt: null }]);
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
		expect(await res.json()).toEqual({ projects: [{ id: 'proj-1', name: 'Web app', createdAt: null }] });
	});

	it('401s an anonymous caller', async () => {
		await expect(GET(event(null))).rejects.toMatchObject({ status: 401 });
	});
});
