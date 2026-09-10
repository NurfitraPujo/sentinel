import { describe, it, expect, vi, beforeEach } from 'vitest';

const queries = {
	getUserOrganizations: vi.fn(),
	createOrganization: vi.fn(),
	createProject: vi.fn(),
	RESERVED_SLUGS: ['admin', 'settings', 'api', 'auth', 'docs', 'billing', 'support'],
};
vi.mock('$lib/db/queries/organizations', () => queries);

vi.mock('$lib/server/auth', () => ({
	requireAuth: vi.fn(async () => ({ id: 'user-1', email: 'engineer@example.com' })),
}));

const { load, actions } = await import('./+page.server');

function eventWith(fields: Record<string, string>) {
	return {
		request: new Request('https://sentinel.test/onboarding', {
			method: 'POST',
			headers: { 'content-type': 'application/x-www-form-urlencoded' },
			body: new URLSearchParams(fields),
		}),
		locals: { auth: vi.fn() },
	} as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	queries.getUserOrganizations.mockResolvedValue([]);
	queries.createOrganization.mockResolvedValue({ id: 'org-1', slug: 'acme' });
	queries.createProject.mockResolvedValue({ id: 'proj-1', name: 'Web app' });
});

describe('/onboarding', () => {
	it('shows step 1 when the user belongs to no organization', async () => {
		expect(await load({ locals: {} } as any)).toEqual({ organization: null });
	});

	it('shows step 2 once an organization exists', async () => {
		queries.getUserOrganizations.mockResolvedValue([{ id: 'org-1', name: 'Acme', slug: 'acme' }]);

		expect(await load({ locals: {} } as any)).toEqual({
			organization: { id: 'org-1', name: 'Acme', slug: 'acme' },
		});
	});

	it('creates the organization for the SESSION user, not anything in the body', async () => {
		const result: any = await actions.createOrg(eventWith({ name: 'Acme', slug: 'acme' }));

		expect(queries.createOrganization).toHaveBeenCalledWith('user-1', { name: 'Acme', slug: 'acme' });
		expect(result).toEqual({ created: 'organization' });
	});

	it('rejects a reserved slug before touching the database', async () => {
		const result: any = await actions.createOrg(eventWith({ name: 'Admin', slug: 'admin' }));

		expect(result.status).toBe(400);
		expect(queries.createOrganization).not.toHaveBeenCalled();
	});

	it('rejects a slug that is not URL-safe', async () => {
		const result: any = await actions.createOrg(eventWith({ name: 'Acme', slug: 'Acme Corp!' }));

		expect(result.status).toBe(400);
		expect(queries.createOrganization).not.toHaveBeenCalled();
	});

	it('reports a taken slug as a 409 the user can act on, not a 500', async () => {
		queries.createOrganization.mockRejectedValue(
			new Error('duplicate key value violates unique constraint "organizations_slug_unique"')
		);

		const result: any = await actions.createOrg(eventWith({ name: 'Acme', slug: 'acme' }));

		expect(result.status).toBe(409);
		expect(result.data.error).toContain('already taken');
	});

	it('creates the project in the user\'s organization and lands on the dashboard', async () => {
		queries.getUserOrganizations.mockResolvedValue([{ id: 'org-1', name: 'Acme', slug: 'acme' }]);

		await expect(actions.createProject(eventWith({ name: 'Web app' }))).rejects.toMatchObject({
			status: 303,
			location: '/',
		});
		expect(queries.createProject).toHaveBeenCalledWith('user-1', 'org-1', { name: 'Web app' });
	});

	it('refuses to create a project when there is no organization yet', async () => {
		const result: any = await actions.createProject(eventWith({ name: 'Web app' }));

		expect(result.status).toBe(400);
		expect(queries.createProject).not.toHaveBeenCalled();
	});
});
