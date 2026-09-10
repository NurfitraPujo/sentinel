import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * sentinel-project-environments §4.4 — contract §11's `GET /` page load boundary.
 *
 * What this file owns is the LOADER's contract with the page: that it reads `environment`, echoes it
 * back in `filters`, returns `environments: []` when `project=all`, feeds the queries the widened
 * authorised set and the resolved scopes, and scopes the project picker to that same set. What the
 * QUERIES then do with those filters is a query-shape claim, asserted against a real Postgres in
 * `src/lib/server/read-guards.integration.test.ts` — a mocked query layer cannot tell a composed
 * predicate from a constructed-and-dropped one.
 */

const getUserMock = vi.fn();
const getAccessibleProjectsMock = vi.fn();
const resolveEnvironmentScopesMock = vi.fn();
const listProjectEnvironmentsMock = vi.fn();
const listIssuesMock = vi.fn();
const countIssuesMock = vi.fn();
const listProjectsMock = vi.fn();

vi.mock('$lib/server/auth', () => ({ getUser: getUserMock }));
vi.mock('$lib/server/issue-access', () => ({ getAccessibleProjects: getAccessibleProjectsMock }));
vi.mock('$lib/server/environment-access', async () => {
	const actual = await vi.importActual<typeof import('$lib/server/environment-access')>(
		'$lib/server/environment-access'
	);
	// `scopeForProject` is the map's accessor and is deliberately real: mocking it would let a test
	// pass while the loader read the map the wrong way round (`map.get(id) ?? []`), which is the bug
	// that silently empties every list.
	return { ...actual, resolveEnvironmentScopes: resolveEnvironmentScopesMock };
});
vi.mock('$lib/db/queries/environments', async () => {
	const actual =
		await vi.importActual<typeof import('$lib/db/queries/environments')>(
			'$lib/db/queries/environments'
		);
	return { ...actual, listProjectEnvironments: listProjectEnvironmentsMock };
});
vi.mock('$lib/server/queries/issue-queries', () => ({
	issueQueries: {
		listIssues: listIssuesMock,
		countIssues: countIssuesMock,
		listProjects: listProjectsMock,
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
	return (await load({
		url: new URL(`http://x/${query}`),
		locals: {},
	} as any)) as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	getUserMock.mockResolvedValue({ id: 'user-1' });
	getAccessibleProjectsMock.mockResolvedValue([
		{ id: PROJECT_A, name: 'Alpha', organizationId: 'org-1' },
		{ id: PROJECT_B, name: 'Beta', organizationId: 'org-1' },
	]);
	resolveEnvironmentScopesMock.mockResolvedValue(new Map());
	listProjectEnvironmentsMock.mockResolvedValue([]);
	listIssuesMock.mockResolvedValue([]);
	countIssuesMock.mockResolvedValue(0);
	listProjectsMock.mockResolvedValue([
		{ id: PROJECT_A, name: 'Alpha' },
		{ id: PROJECT_B, name: 'Beta' },
	]);
});

describe('GET / page load (§4.4)', () => {
	it('passes the environment filter through to the list and the count, and echoes it back', async () => {
		const data = await loadWith(`?project=${PROJECT_A}&environment=production`);

		expect(data.filters.environment).toBe('production');
		expect(listIssuesMock.mock.calls[0][0]).toMatchObject({ environment: 'production' });
		// A total that counts rows the list is not allowed to show is the same leak in a smaller
		// font, so the count takes the same filters object.
		expect(countIssuesMock.mock.calls[0][0]).toMatchObject({ environment: 'production' });
	});

	it('echoes "all" when the caller did not ask for an environment', async () => {
		const data = await loadWith('');

		expect(data.filters.environment).toBe('all');
	});

	it('returns environments: [] when project=all, and the guard is still passed', async () => {
		resolveEnvironmentScopesMock.mockResolvedValue(new Map([[PROJECT_A, ['staging']]]));

		const data = await loadWith('?project=all');

		// An environment name is only meaningful inside a project — two projects may both report
		// `production` and they are not the same environment — so no cross-project list is offered.
		expect(data.environments).toEqual([]);
		expect(listProjectEnvironmentsMock).not.toHaveBeenCalled();
		// The GUARD still applies on project=all; only the offered FILTER does not.
		expect(listIssuesMock.mock.calls[0][0].environmentScopes).toEqual(
			new Map([[PROJECT_A, ['staging']]])
		);
	});

	it('offers the selected project’s registry, minus hidden rows and minus anything outside the grant', async () => {
		resolveEnvironmentScopesMock.mockResolvedValue(new Map([[PROJECT_A, ['staging']]]));
		listProjectEnvironmentsMock.mockResolvedValue([
			registryRow('production'),
			registryRow('staging'),
			registryRow('canary', { hidden: true }),
		]);

		const data = await loadWith(`?project=${PROJECT_A}`);

		// A filter for data you cannot read is a control that does nothing.
		expect(data.environments.map((e: { name: string }) => e.name)).toEqual(['staging']);
	});

	it('offers the whole non-hidden registry to a caller with no grant row — absence means all', async () => {
		listProjectEnvironmentsMock.mockResolvedValue([
			registryRow('production'),
			registryRow('canary', { hidden: true }),
		]);

		const data = await loadWith(`?project=${PROJECT_A}`);

		expect(data.environments.map((e: { name: string }) => e.name)).toEqual(['production']);
	});

	it('does not read the registry of a project the caller cannot reach', async () => {
		const data = await loadWith('?project=cccccccc-0000-0000-0000-000000000003');

		expect(listProjectEnvironmentsMock).not.toHaveBeenCalled();
		expect(data.environments).toEqual([]);
	});

	// Decision 35: the derivation widened, and the picker is scoped to the same set as the list —
	// `listProjects` was `SELECT id, name FROM projects LIMIT 100` with no WHERE clause at all.
	it('derives the authorised set from getAccessibleProjects and scopes the project picker to it', async () => {
		await loadWith('');

		expect(getAccessibleProjectsMock).toHaveBeenCalledWith('user-1');
		expect(listIssuesMock.mock.calls[0][0].projectIds).toEqual([PROJECT_A, PROJECT_B]);
		expect(listProjectsMock).toHaveBeenCalledWith([PROJECT_A, PROJECT_B]);
	});

	// This asserted that the EMPTY authorised set was passed through to the queries rather than
	// omitted, because omitting it is what turned `/` into "every issue in the database". `main`'s
	// nav-guard work (#10) then made an empty set redirect to `/onboarding` before any query runs —
	// a user who reaches no project has nothing to render and no way to act on it. The two intents
	// agree on the outcome and `main`'s is stricter: the empty set now denies by never reaching the
	// query at all, which is asserted here.
	//
	// The original property — that `projectIds: []` composes to a predicate matching no row, and is
	// never dropped — is not lost. It belongs to the query layer and is asserted there against a
	// real Postgres: src/lib/server/read-guards.integration.test.ts:340, 'an empty authorised set
	// DENIES rather than admitting every issue in the database'. A mocked query layer cannot tell a
	// composed predicate from a constructed-and-dropped one anyway (see this file's header).
	it('redirects an empty authorised set to /onboarding without querying issues at all', async () => {
		getAccessibleProjectsMock.mockResolvedValue([]);

		await expect(loadWith('')).rejects.toMatchObject({
			status: 303,
			location: '/onboarding',
		});

		expect(listIssuesMock).not.toHaveBeenCalled();
		expect(countIssuesMock).not.toHaveBeenCalled();
		expect(listProjectsMock).not.toHaveBeenCalled();
	});
});
