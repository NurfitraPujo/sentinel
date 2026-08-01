import { describe, it, expect, vi, beforeEach } from 'vitest';

// A chainable Drizzle-query double (same pattern used across this repo's other loader tests,
// e.g. settings/alerts/page.server.test.ts and organizations/keys.test.ts).
function makeDbMock() {
	const dbMock: any = {
		select: vi.fn(),
		from: vi.fn(),
		where: vi.fn(),
		then: vi.fn(),
	};
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
	return dbMock;
}

const dbMock = makeDbMock();

vi.mock('$lib/server/db', () => ({ db: dbMock }));
vi.mock('$lib/db/schema', () => ({
	organizations: { id: 'id', name: 'name', slug: 'slug' },
	projects: { id: 'id', name: 'name', organizationId: 'organizationId' },
}));

const { load } = await import('./+page.server');

function resetDbMock() {
	vi.clearAllMocks();
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
}

function locals(session: { email: string } | null, currentOrg?: { id: string; name: string; slug: string }) {
	return {
		auth: async () => (session ? { user: { email: session.email } } : null),
		currentOrg,
	} as any;
}

const ORG = { id: 'org-1', name: 'Acme', slug: 'acme' };

describe('[orgSlug]/settings/keys +page.server load', () => {
	beforeEach(() => {
		resetDbMock();
	});

	// D39: this loader used to resolve `organizations` by slug BEFORE checking auth, making the
	// route an anonymous org-existence oracle (404 for a slug that doesn't exist, further work for
	// one that does). Auth must be checked first, and no DB/fetch work should have happened yet.
	it('401s an anonymous request before doing any lookup', async () => {
		const fetchMock = vi.fn();
		await expect(
			load({ params: { orgSlug: 'acme' }, fetch: fetchMock, locals: locals(null) } as any)
		).rejects.toMatchObject({ status: 401 });
		expect(fetchMock).not.toHaveBeenCalled();
		expect(dbMock.select).not.toHaveBeenCalled();
	});

	it('403s when the session has no membership in the requested org', async () => {
		const fetchMock = vi.fn();
		await expect(
			load({
				params: { orgSlug: 'acme' },
				fetch: fetchMock,
				locals: locals({ email: 'user@example.com' }, undefined),
			} as any)
		).rejects.toMatchObject({ status: 403 });
		expect(fetchMock).not.toHaveBeenCalled();
	});

	it('403s when currentOrg belongs to a different slug than the URL', async () => {
		const fetchMock = vi.fn();
		await expect(
			load({
				params: { orgSlug: 'other-org' },
				fetch: fetchMock,
				locals: locals({ email: 'user@example.com' }, ORG),
			} as any)
		).rejects.toMatchObject({ status: 403 });
		expect(fetchMock).not.toHaveBeenCalled();
	});

	it('returns keys with project names attached for the Target column (D37), for an authorized member', async () => {
		dbMock.then.mockImplementationOnce((resolve: any) =>
			resolve([{ id: 'proj-1', name: 'Web App' }])
		);
		const fetchMock = vi.fn().mockResolvedValue({
			ok: true,
			json: async () => ({
				keys: [
					{ id: 'key-1', projectId: 'proj-1', name: 'Scoped Key' },
					{ id: 'key-2', projectId: null, name: 'Org Key' },
				],
			}),
		});

		const result: any = await load({
			params: { orgSlug: 'acme' },
			fetch: fetchMock,
			locals: locals({ email: 'user@example.com' }, ORG),
		} as any);

		expect(result.orgId).toBe('org-1');
		expect(result.keys).toEqual([
			{ id: 'key-1', projectId: 'proj-1', name: 'Scoped Key', targetProject: 'Web App' },
			{ id: 'key-2', projectId: null, name: 'Org Key' },
		]);
		expect(result.projects).toEqual([{ id: 'proj-1', name: 'Web App' }]);
	});

	it('propagates a failed keys fetch as an error', async () => {
		dbMock.then.mockImplementationOnce((resolve: any) => resolve([]));
		const fetchMock = vi.fn().mockResolvedValue({
			ok: false,
			status: 500,
			json: async () => ({ message: 'boom' }),
		});

		await expect(
			load({
				params: { orgSlug: 'acme' },
				fetch: fetchMock,
				locals: locals({ email: 'user@example.com' }, ORG),
			} as any)
		).rejects.toMatchObject({ status: 500 });
	});
});
