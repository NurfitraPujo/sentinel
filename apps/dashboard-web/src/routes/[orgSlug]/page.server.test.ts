import { describe, it, expect, vi, beforeEach } from 'vitest';

const mockDb = {
	select: vi.fn(),
};

vi.mock('$lib/server/db', () => ({
	db: mockDb,
}));

const mockOrgQueries = {
	updateUserLastActiveOrg: vi.fn(),
};

vi.mock('$lib/db/queries/organizations', () => mockOrgQueries);

const { load } = await import('./+page.server');

beforeEach(() => {
	vi.clearAllMocks();
});

describe('/[orgSlug]', () => {
	it('redirects to /[orgSlug]/projects and updates last active org if authenticated', async () => {
		const session = { user: { email: 'alice@example.com' } };
		const currentOrg = { id: 'org-1', name: 'Acme Corp', slug: 'acme' };

		const locals = {
			auth: vi.fn(async () => session),
			currentOrg,
		};

		mockDb.select.mockReturnValue({
			from: vi.fn().mockReturnValue({
				where: vi.fn().mockReturnValue({
					limit: vi.fn().mockResolvedValue([{ id: 'user-1' }]),
				}),
			}),
		});

		try {
			await load({ params: { orgSlug: 'acme' }, locals } as any);
			expect.fail('Expected redirect');
		} catch (e: any) {
			expect(e.status).toBe(303);
			expect(e.location).toBe('/acme/projects');
		}

		expect(mockOrgQueries.updateUserLastActiveOrg).toHaveBeenCalledWith('user-1', 'org-1');
	});

	it('redirects unauthenticated visitor to /[orgSlug]/projects without updating preference', async () => {
		const locals = {
			auth: vi.fn(async () => null),
			currentOrg: null,
		};

		try {
			await load({ params: { orgSlug: 'acme' }, locals } as any);
			expect.fail('Expected redirect');
		} catch (e: any) {
			expect(e.status).toBe(303);
			expect(e.location).toBe('/acme/projects');
		}

		expect(mockOrgQueries.updateUserLastActiveOrg).not.toHaveBeenCalled();
	});
});
