import { describe, it, expect, vi, beforeEach } from 'vitest';

const mockDb = {
	select: vi.fn(),
};

vi.mock('$lib/server/db', () => ({
	db: mockDb,
}));

const mockOrgQueries = {
	getUserOrganizations: vi.fn(),
};

vi.mock('$lib/db/queries/organizations', () => mockOrgQueries);

const { load } = await import('./+layout.server');

const ORG_A = {
	id: 'org-1',
	name: 'Acme Corp',
	slug: 'acme',
	avatarUrl: 'https://example.com/acme.png',
	role: 'owner',
	createdAt: new Date(),
	projectCount: 2,
};

beforeEach(() => {
	vi.clearAllMocks();
});

describe('+layout.server', () => {
	it('returns nulls and empty organizations when session is missing', async () => {
		const locals = {
			auth: vi.fn(async () => null),
			currentOrg: null,
		};

		const result = await load({ locals } as any);
		expect(result).toEqual({
			session: null,
			orgSlug: null,
			activeOrg: null,
			organizations: [],
		});
	});

	it('returns activeOrg, orgSlug, and user organizations when authenticated', async () => {
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

		mockOrgQueries.getUserOrganizations.mockResolvedValue([ORG_A]);

		const result = await load({ locals } as any);
		expect(result).toEqual({
			session,
			orgSlug: 'acme',
			activeOrg: currentOrg,
			organizations: [ORG_A],
		});

		expect(mockOrgQueries.getUserOrganizations).toHaveBeenCalledWith('user-1');
	});
});
