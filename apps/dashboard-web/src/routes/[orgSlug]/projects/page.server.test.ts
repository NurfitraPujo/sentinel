import { describe, it, expect, vi, beforeEach } from 'vitest';

const dbMock: any = { select: vi.fn(), from: vi.fn(), where: vi.fn() };
dbMock.select.mockReturnValue(dbMock);
dbMock.from.mockReturnValue(dbMock);
vi.mock('$lib/server/db', () => ({ db: dbMock }));
vi.mock('$lib/db/schema', () => ({
	projects: { id: 'id', name: 'name', createdAt: 'createdAt', organizationId: 'organizationId', isInbox: 'isInbox' },
}));

const { load } = await import('./+page.server');

function event(session: unknown, currentOrg: unknown, orgSlug = 'acme') {
	return {
		params: { orgSlug },
		locals: { auth: vi.fn().mockResolvedValue(session), currentOrg },
	} as any;
}

const ORG = { id: 'org-1', name: 'Acme', slug: 'acme' };
const SESSION = { user: { email: 'engineer@example.com' } };

beforeEach(() => {
	vi.clearAllMocks();
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockResolvedValue([{ id: 'proj-1', name: 'Web app', createdAt: null }]);
});

describe('/[orgSlug]/projects', () => {
	it('returns the org and its projects for a member', async () => {
		expect(await load(event(SESSION, ORG))).toEqual({
			org: { name: 'Acme', slug: 'acme' },
			projects: [{ id: 'proj-1', name: 'Web app', createdAt: null }],
		});
	});

	// D39: authenticate before any lookup that reveals whether an organization exists, so an
	// anonymous caller cannot tell a real slug from a made-up one.
	it('401s an anonymous caller without querying anything', async () => {
		await expect(load(event(null, ORG))).rejects.toMatchObject({ status: 401 });
		expect(dbMock.select).not.toHaveBeenCalled();
	});

	it('403s when the resolved org is not the slug in the URL', async () => {
		await expect(load(event(SESSION, ORG, 'someone-else'))).rejects.toMatchObject({ status: 403 });
		expect(dbMock.select).not.toHaveBeenCalled();
	});

	it('403s when no org resolved at all', async () => {
		await expect(load(event(SESSION, undefined))).rejects.toMatchObject({ status: 403 });
	});
});
