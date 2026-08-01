import { describe, it, expect, vi, beforeEach } from 'vitest';

// A chainable Drizzle-query double (same pattern used across this repo's other loader tests).
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
	organizations: { id: 'id', name: 'name', slug: 'slug' },
	projects: { id: 'id', name: 'name', organizationId: 'organizationId' },
}));

const { load } = await import('./+page.server');

function resetDbMock() {
	vi.clearAllMocks();
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.then.mockReset();
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
}

function locals(session: { email: string } | null, currentOrg?: { id: string; name: string; slug: string }) {
	return {
		auth: async () => (session ? { user: { email: session.email } } : null),
		currentOrg,
	} as any;
}

const ORG = { id: 'org-1', name: 'Acme', slug: 'acme' };

describe('[orgSlug]/projects/[projectId]/settings/keys +page.server load', () => {
	beforeEach(() => {
		resetDbMock();
	});

	// D39: same anonymous org-existence-oracle concern as the org-level keys loader — auth must be
	// checked before any org/project lookup.
	it('401s an anonymous request before doing any lookup', async () => {
		const fetchMock = vi.fn();
		await expect(
			load({
				params: { orgSlug: 'acme', projectId: 'proj-1' },
				fetch: fetchMock,
				locals: locals(null),
			} as any)
		).rejects.toMatchObject({ status: 401 });
		expect(fetchMock).not.toHaveBeenCalled();
		expect(dbMock.select).not.toHaveBeenCalled();
	});

	it('403s when currentOrg does not match the URL slug', async () => {
		const fetchMock = vi.fn();
		await expect(
			load({
				params: { orgSlug: 'acme', projectId: 'proj-1' },
				fetch: fetchMock,
				locals: locals({ email: 'user@example.com' }, undefined),
			} as any)
		).rejects.toMatchObject({ status: 403 });
		expect(fetchMock).not.toHaveBeenCalled();
	});

	it('404s when the project does not belong to the organization', async () => {
		dbMock.then.mockImplementationOnce((resolve: any) => resolve([])); // project lookup: no row
		const fetchMock = vi.fn();
		await expect(
			load({
				params: { orgSlug: 'acme', projectId: 'proj-missing' },
				fetch: fetchMock,
				locals: locals({ email: 'user@example.com' }, ORG),
			} as any)
		).rejects.toMatchObject({ status: 404 });
		expect(fetchMock).not.toHaveBeenCalled();
	});

	it('filters org keys down to this project and returns the project name for an authorized member', async () => {
		dbMock.then.mockImplementationOnce((resolve: any) =>
			resolve([{ id: 'proj-1', name: 'Web App' }])
		);
		const fetchMock = vi.fn().mockResolvedValue({
			ok: true,
			json: async () => ({
				keys: [
					{ id: 'key-1', projectId: 'proj-1', name: 'Scoped Key' },
					{ id: 'key-2', projectId: 'proj-2', name: 'Other Project Key' },
					{ id: 'key-3', projectId: null, name: 'Org Key' },
				],
			}),
		});

		const result: any = await load({
			params: { orgSlug: 'acme', projectId: 'proj-1' },
			fetch: fetchMock,
			locals: locals({ email: 'user@example.com' }, ORG),
		} as any);

		expect(result.projectId).toBe('proj-1');
		expect(result.projectName).toBe('Web App');
		expect(result.keys).toEqual([{ id: 'key-1', projectId: 'proj-1', name: 'Scoped Key' }]);
		expect(result.projects).toEqual([{ id: 'proj-1', name: 'Web App' }]);
	});
});
