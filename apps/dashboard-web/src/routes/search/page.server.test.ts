import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * sentinel-project-environments §4.5 change 1 — contract §11's `GET /search` page load, for the one
 * thing this loader now owes the page: `data.environments`.
 *
 * What this file owns is the loader's contract with the selector — §4.1's registry for a selected
 * project, §4.3a's guarded union for none, `hidden` and out-of-scope rows gone, and nothing at all
 * for a project the caller cannot reach. What the SEARCH then does with the filters is
 * `searchErrors`'s claim and is asserted against a real Postgres in
 * `src/lib/server/read-guards.integration.test.ts`; a mocked query layer cannot tell a composed
 * predicate from a constructed-and-dropped one.
 */

const requireAuthMock = vi.fn();
const searchErrorsMock = vi.fn();
const getAccessibleProjectsMock = vi.fn();
const resolveEnvironmentScopesMock = vi.fn();
const resolveEnvironmentUnionMock = vi.fn();
const listProjectEnvironmentsMock = vi.fn();

vi.mock('$lib/server/auth', () => ({ requireAuth: requireAuthMock }));
vi.mock('$lib/server/search', () => ({ searchErrors: searchErrorsMock }));
vi.mock('$lib/server/issue-access', () => ({ getAccessibleProjects: getAccessibleProjectsMock }));
vi.mock('$lib/server/environment-union', () => ({
	resolveEnvironmentUnion: resolveEnvironmentUnionMock,
}));
vi.mock('$lib/server/environment-access', async () => {
	const actual = await vi.importActual<typeof import('$lib/server/environment-access')>(
		'$lib/server/environment-access'
	);
	// `scopeForProject` stays real: mocking it would let a test pass while the loader read the map
	// the wrong way round (`map.get(id) ?? []`), which is the bug that silently empties every list.
	return { ...actual, resolveEnvironmentScopes: resolveEnvironmentScopesMock };
});
vi.mock('$lib/db/queries/environments', async () => {
	const actual = await vi.importActual<typeof import('$lib/db/queries/environments')>(
		'$lib/db/queries/environments'
	);
	return { ...actual, listProjectEnvironments: listProjectEnvironmentsMock };
});
vi.mock('$lib/server/db', () => ({
	db: {
		select: () => ({
			from: () => ({ innerJoin: () => ({ where: async () => [] }) }),
		}),
	},
}));

const { load } = await import('./+page.server');

const PROJECT_A = 'aaaaaaaa-0000-0000-0000-000000000001';
const PROJECT_B = 'bbbbbbbb-0000-0000-0000-000000000002';

function registryRow(name: string, extra: Record<string, unknown> = {}) {
	return {
		id: `env-${name}`,
		projectId: PROJECT_A,
		name,
		displayName: name,
		isDefault: false,
		hidden: false,
		firstSeenAt: null,
		lastSeenAt: null,
		createdAt: new Date('2026-01-01T00:00:00Z'),
		...extra,
	};
}

async function loadWith(query: string) {
	return (await load({ url: new URL(`http://x/search${query}`), locals: {} } as any)) as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	requireAuthMock.mockResolvedValue({ id: 'user-1' });
	searchErrorsMock.mockResolvedValue({ results: [], total: 0 });
	getAccessibleProjectsMock.mockResolvedValue([
		{ id: PROJECT_A, name: 'Alpha', organizationId: 'org-1' },
		{ id: PROJECT_B, name: 'Beta', organizationId: 'org-1' },
	]);
	resolveEnvironmentScopesMock.mockResolvedValue(new Map());
	resolveEnvironmentUnionMock.mockResolvedValue([]);
	listProjectEnvironmentsMock.mockResolvedValue([]);
});

describe('GET /search page load (§4.5 change 1)', () => {
	it('offers the selected project’s registry, without its hidden rows', async () => {
		listProjectEnvironmentsMock.mockResolvedValue([
			registryRow('production'),
			registryRow('canary', { hidden: true }),
			registryRow('staging'),
		]);

		const data = await loadWith(`?project=${PROJECT_A}`);

		expect(listProjectEnvironmentsMock).toHaveBeenCalledWith(PROJECT_A);
		expect(data.environments.map((e: { name: string }) => e.name)).toEqual([
			'production',
			'staging',
		]);
	});

	it('drops a registry row the caller’s grant does not cover', async () => {
		resolveEnvironmentScopesMock.mockResolvedValue(new Map([[PROJECT_A, ['staging']]]));
		listProjectEnvironmentsMock.mockResolvedValue([
			registryRow('production'),
			registryRow('staging'),
		]);

		const data = await loadWith(`?project=${PROJECT_A}`);

		// A filter for data you cannot read is a control that does nothing, and offering the name is
		// itself the leak decision 12 closes.
		expect(data.environments.map((e: { name: string }) => e.name)).toEqual(['staging']);
	});

	it('uses §4.3a’s guarded union when no project is selected, and publishes the name only', async () => {
		resolveEnvironmentUnionMock.mockResolvedValue(['production', 'staging']);

		const data = await loadWith('');

		expect(resolveEnvironmentUnionMock).toHaveBeenCalledWith('user-1');
		expect(listProjectEnvironmentsMock).not.toHaveBeenCalled();
		// Two projects' `production` are two rows with two labels, so §4.3a publishes no
		// `displayName`, no `id` and no `isDefault` — one project's label for a union entry would be
		// a lie the page tells for free.
		expect(data.environments).toEqual([{ name: 'production' }, { name: 'staging' }]);
	});

	it('offers nothing for a project the caller cannot reach', async () => {
		const foreign = 'cccccccc-0000-0000-0000-000000000003';

		const data = await loadWith(`?project=${foreign}`);

		expect(data.environments).toEqual([]);
		expect(listProjectEnvironmentsMock).not.toHaveBeenCalled();
	});

	it('returns [] rather than failing when the project has no registry rows', async () => {
		// Under decision 8 no registry row is ever created by traffic, so this is every project's
		// first day and it is a normal answer, not an error.
		const data = await loadWith(`?project=${PROJECT_A}`);

		expect(data.environments).toEqual([]);
	});
});
