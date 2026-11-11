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

/**
 * sentinel-issue-tag-facets §6.7 (tfr-search-page-behaviour-unproven): the page-level tag behaviours the
 * spec requires but no test failed if removed — the MAX_TAG_FILTERS cap, dedupe, removeTagHref dropping
 * exactly the clicked tag, and the loader never emitting a raw {@html}-able string.
 */
describe('GET /search tag filter collection (§6.7)', () => {
	it('keeps only the first MAX_TAG_FILTERS distinct tags, listing the rest as ignored', async () => {
		const params = Array.from({ length: 12 }, (_, i) => `tag=k${i}:v${i}`).join('&');
		const data = await loadWith(`?${params}`);

		expect(data.filters.tags).toHaveLength(10);
		expect(data.filters.tags.map((t: { key: string }) => t.key)).toEqual(
			Array.from({ length: 10 }, (_, i) => `k${i}`)
		);
		expect(data.ignoredTagTerms).toEqual(['k10:v10', 'k11:v11']);
	});

	it('de-duplicates identical (key, value) pairs across tag= params and box tokens', async () => {
		const data = await loadWith(`?tag=release:1.0&tag=release:1.0&search=${encodeURIComponent('release:1.0 boom')}`);

		expect(data.filters.tags).toHaveLength(1);
		expect(data.filters.tags[0]).toMatchObject({ key: 'release', value: '1.0' });
		// data.filters.search is the box's raw original text (what re-populates the input); the box's
		// remainder after pulling the tag token out is what searchErrors actually receives as `search`.
		expect(data.filters.search).toBe('release:1.0 boom');
		const searchErrorsFilters = searchErrorsMock.mock.calls[0][1];
		expect(searchErrorsFilters.search.trim()).toBe('boom');
	});

	it("removeHref keeps every OTHER active tag and drops exactly the clicked one", async () => {
		const data = await loadWith('?tag=release:1.0&tag=os.name:linux&environment=staging');
		const tags = data.filters.tags as { key: string; value: string; removeHref: string }[];
		expect(tags).toHaveLength(2);

		const releaseTag = tags.find((t) => t.key === 'release')!;
		const url = new URL(`http://x${releaseTag.removeHref}`);
		const remaining = url.searchParams.getAll('tag');

		expect(remaining).toEqual(['os.name:linux']);
		expect(remaining).not.toContain('release:1.0');
		expect(url.searchParams.get('environment')).toBe('staging');
	});

	it('TAG_KEY_PATTERN rejects a poisoned key outright, so it never reaches filters.tags', async () => {
		// This is a loader-level test: it proves the key is REJECTED, not that any rendering is safe (the
		// loader renders nothing). The {@html} rendering guarantee itself — that the raw ignoredTagTerms
		// text this loader hands to the page is never interpreted as markup — is proven at the component
		// level, in page.svelte.test.ts (tfr2-search-html-test-decoration).
		const data = await loadWith(`?${new URLSearchParams({ tag: '<img src=x onerror=alert(1)>:v' }).toString()}`);
		expect(data.filters.tags).toEqual([]);
		expect(data.ignoredTagTerms.some((t: string) => t.includes('<img'))).toBe(true);
	});
});
