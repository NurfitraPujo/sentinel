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
// sentinel-issue-trend §4.1: the per-card series reader. Mocked like the queries — what it returns
// for a set of ids against a real Postgres is issue-queries.trend.integration.test.ts's boundary;
// this file owns WHICH ids it is asked about, and that it is never asked about none.
const trendForIssuesMock = vi.fn();
const getClaimantFacetCountsMock = vi.fn();
const listOrgUsersMock = vi.fn();
const listOrgAgentsMock = vi.fn();

// sentinel-affected-users decision 21: `retentionDays` is read through `$lib/server/retention-window`,
// which reads `DATA_RETENTION_DAYS` fresh from `$env/dynamic/private` on every call — so the env is a
// mutable object varied per test (email.test.ts's pattern), and the helper is spied on rather than
// replaced so the assertion is "the loader asked the SHARED helper", not "some number came back".
const mockEnv: Record<string, string | undefined> = {};
vi.mock('$env/dynamic/private', () => ({ env: mockEnv }));
const getRetentionDaysSpy = vi.fn();
vi.mock('$lib/server/retention-window', async () => {
	const actual = await vi.importActual<typeof import('$lib/server/retention-window')>(
		'$lib/server/retention-window'
	);
	getRetentionDaysSpy.mockImplementation(actual.getRetentionDays);
	return { ...actual, getRetentionDays: getRetentionDaysSpy };
});

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
		trendForIssues: trendForIssuesMock,
		getClaimantFacetCounts: getClaimantFacetCountsMock,
		listOrgUsers: listOrgUsersMock,
		listOrgAgents: listOrgAgentsMock,
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
	delete mockEnv.DATA_RETENTION_DAYS;
	getUserMock.mockResolvedValue({ id: 'user-1' });
	getAccessibleProjectsMock.mockResolvedValue([
		{ id: PROJECT_A, name: 'Alpha', organizationId: 'org-1' },
		{ id: PROJECT_B, name: 'Beta', organizationId: 'org-1' },
	]);
	resolveEnvironmentScopesMock.mockResolvedValue(new Map());
	listProjectEnvironmentsMock.mockResolvedValue([]);
	listIssuesMock.mockResolvedValue([]);
	countIssuesMock.mockResolvedValue(0);
	trendForIssuesMock.mockResolvedValue(new Map());
	getClaimantFacetCountsMock.mockResolvedValue({
		total: 0,
		unclaimed: 0,
		claimedMe: 0,
		claimedAgent: 0,
		byClaimant: {},
		claimantTypes: {},
	});
	listOrgUsersMock.mockResolvedValue([]);
	listOrgAgentsMock.mockResolvedValue([]);
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

	it('returns the authenticated currentUser in page data', async () => {
		getUserMock.mockResolvedValueOnce({ id: 'user-42', email: 'fitrapujo@gmail.com' });
		const data = await loadWith('');

		expect(data.currentUser).toEqual({ id: 'user-42', email: 'fitrapujo@gmail.com' });
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

	// Decision 55 (`OQ-41` = (a)): on `project=all` the loader coerces `environment` to `all` and
	// adds NO environment predicate. The state is reachable by an ordinary route — flow `L7` makes
	// the parameter ride on the paging anchors, so it survives a switch to `project=all` and on the
	// reload of a bookmark — and since `environments: []` means the control is not rendered, the
	// list would be silently narrowed by a filter with no on-screen affordance to clear it, on the
	// page whose empty state is required to name the environment.
	it('coerces environment to "all" on project=all, for the list, the count and the echo', async () => {
		const data = await loadWith('?project=all&environment=production');

		expect(listIssuesMock.mock.calls[0][0]).toMatchObject({ environment: 'all' });
		// Same filter object reaches the count: a total that counts rows the list does not show is
		// the same inconsistency in a smaller font.
		expect(countIssuesMock.mock.calls[0][0]).toMatchObject({ environment: 'all' });
		// The echo is what the page builds every anchor from (`href()`), so the coercion has to be
		// visible there too or the parameter keeps riding on the links.
		expect(data.filters.environment).toBe('all');
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

/**
 * sentinel-affected-users §4.1 — the `sort` parameter (decision 10) and `retentionDays` (decision 21),
 * the loader half of flows `L1`–`L7`. What the QUERY does with `sort` is asserted against a real
 * Postgres in `src/lib/server/queries/issue-queries.sort.integration.test.ts`; this file owns the
 * parse, the coercion, the echo, and which query receives it.
 */
describe('GET / page load — sort and retentionDays (sentinel-affected-users §4.1)', () => {
	it('passes sort=affected_users to listIssues and echoes it on filters.sort', async () => {
		const data = await loadWith('?sort=affected_users');

		expect(data.filters.sort).toBe('affected_users');
		expect(listIssuesMock.mock.calls[0][0]).toMatchObject({ sort: 'affected_users' });
	});

	// `sort` changes the ORDER BY of the guarded query and nothing about its WHERE; `pagination.total`
	// does not change with the order the same rows are listed in, so the count never sees it.
	it('does not pass sort to countIssues — the total does not depend on the order', async () => {
		await loadWith('?sort=affected_users');

		expect(countIssuesMock).toHaveBeenCalledTimes(1);
		expect(countIssuesMock.mock.calls[0][0]).not.toHaveProperty('sort');
		// And the list still got everything the count got — the same filters, plus the sort.
		const { sort: _sort, ...listFilters } = listIssuesMock.mock.calls[0][0];
		expect(listFilters).toEqual(countIssuesMock.mock.calls[0][0]);
	});

	it('defaults to last_seen — today’s order — when sort is absent', async () => {
		const data = await loadWith('');

		expect(data.filters.sort).toBe('last_seen');
		expect(listIssuesMock.mock.calls[0][0]).toMatchObject({ sort: 'last_seen' });
	});

	// Flow `L7`: the echo is what `href()` builds every anchor from, so echoing `banana` would keep
	// it riding on links the reader has no control to clear it from — the same rule the environment
	// echo follows on `project=all`.
	it('coerces an unknown sort to last_seen for the query AND the echo, never the bad string', async () => {
		const data = await loadWith('?sort=banana');

		expect(data.filters.sort).toBe('last_seen');
		expect(listIssuesMock.mock.calls[0][0]).toMatchObject({ sort: 'last_seen' });
	});

	it('returns retentionDays 30 when DATA_RETENTION_DAYS is unset, from the shared helper', async () => {
		const data = await loadWith('');

		expect(data.retentionDays).toBe(30);
		expect(getRetentionDaysSpy).toHaveBeenCalled();
	});

	it('returns the parsed DATA_RETENTION_DAYS when it is set, as an integer', async () => {
		mockEnv.DATA_RETENTION_DAYS = '45';

		const data = await loadWith('');

		expect(data.retentionDays).toBe(45);
		expect(Number.isInteger(data.retentionDays)).toBe(true);
	});
});

/**
 * sentinel-issue-trend §4.1 — the `new` filter (decision 6), the two projection fields (decision 22)
 * and the per-card series (decision 10): the loader half of flows `L4`–`L7` and the page-data table.
 * What the QUERIES do with `firstSeenAfter` and what the reader returns for a set of ids are query
 * claims, asserted against a real Postgres in
 * `src/lib/server/queries/issue-queries.trend.integration.test.ts`; this file owns the parse, the
 * coercion, the echo, that ONE instant reaches both queries, and what the page data carries.
 */
describe('GET / page load — new filter, regression fields and trend24h (sentinel-issue-trend §4.1)', () => {
	const ISSUE_1 = 'cccccccc-0000-0000-0000-000000000001';
	const ISSUE_2 = 'cccccccc-0000-0000-0000-000000000002';

	function listedIssue(id: string, extra: Record<string, unknown> = {}) {
		return {
			id,
			projectId: PROJECT_A,
			fingerprint: `fp-${id}`,
			message: 'boom',
			errorClass: 'TypeError',
			environment: 'production',
			status: 'open',
			firstSeen: new Date('2026-09-13T09:00:00Z'),
			lastSeen: new Date('2026-09-13T13:00:00Z'),
			count: 7,
			affectedUserCount: 0,
			affectedTenantCount: 0,
			regressionStatus: 'none',
			lastRegressedAt: null,
			...extra,
		};
	}

	// Flow `L7`: the echo is what `href()` builds every anchor from, so `banana` must echo `all` —
	// and `all` must mean NO predicate on either query, or the bad bookmark narrows the list.
	it('coerces an unknown new to all for the echo, and passes no first_seen bound to either query', async () => {
		const data = await loadWith('?new=banana');

		expect(data.filters.new).toBe('all');
		expect(listIssuesMock.mock.calls[0][0].firstSeenAfter).toBeUndefined();
		expect(countIssuesMock.mock.calls[0][0].firstSeenAfter).toBeUndefined();
	});

	it('echoes all when new is absent, with no bound', async () => {
		const data = await loadWith('');

		expect(data.filters.new).toBe('all');
		expect(listIssuesMock.mock.calls[0][0].firstSeenAfter).toBeUndefined();
	});

	// Flow `L4`, decision 6: `24h` is a FILTER, so it reaches the count too — a total that counts
	// rows the list does not show is the same inconsistency in a smaller font — and it is the SAME
	// instant on both, taken once in the loader, not `new Date()` twice.
	it('passes one identical first_seen lower bound, now − 24h, to listIssues AND countIssues on new=24h', async () => {
		const before = Date.now();
		const data = await loadWith('?new=24h');
		const after = Date.now();

		expect(data.filters.new).toBe('24h');
		const listBound = listIssuesMock.mock.calls[0][0].firstSeenAfter;
		const countBound = countIssuesMock.mock.calls[0][0].firstSeenAfter;
		expect(listBound).toBeInstanceOf(Date);
		expect(countBound).toBe(listBound);
		const DAY = 24 * 60 * 60 * 1000;
		expect(listBound.getTime()).toBeGreaterThanOrEqual(before - DAY);
		expect(listBound.getTime()).toBeLessThanOrEqual(after - DAY);
	});

	it('keeps the sort out of the count and the bound in it — the two filters compose', async () => {
		await loadWith('?new=24h&sort=count');

		expect(listIssuesMock.mock.calls[0][0]).toMatchObject({ sort: 'count' });
		expect(countIssuesMock.mock.calls[0][0]).not.toHaveProperty('sort');
		const { sort: _sort, ...listFilters } = listIssuesMock.mock.calls[0][0];
		expect(listFilters).toEqual(countIssuesMock.mock.calls[0][0]);
	});

	it('passes sort=count and sort=first_seen to listIssues and echoes them', async () => {
		for (const sort of ['count', 'first_seen'] as const) {
			vi.clearAllMocks();
			listIssuesMock.mockResolvedValue([]);
			countIssuesMock.mockResolvedValue(0);
			listProjectsMock.mockResolvedValue([]);

			const data = await loadWith(`?sort=${sort}`);

			expect(data.filters.sort).toBe(sort);
			expect(listIssuesMock.mock.calls[0][0]).toMatchObject({ sort });
		}
	});

	// Decision 10: ONE reader call for the page, keyed on exactly the ids the list returned — never a
	// project, never a user — over the 24 hourly slots ending at the current UTC hour.
	it('asks the reader for exactly the listed ids over the 24-hour window and attaches trend24h of length 24', async () => {
		listIssuesMock.mockResolvedValue([listedIssue(ISSUE_1), listedIssue(ISSUE_2)]);
		const series1 = new Array(24).fill(0);
		series1[23] = 5;
		trendForIssuesMock.mockResolvedValue(
			new Map([
				[ISSUE_1, series1],
				[ISSUE_2, new Array(24).fill(0)],
			])
		);

		const before = Date.now();
		const data = await loadWith('');

		expect(trendForIssuesMock).toHaveBeenCalledTimes(1);
		const [ids, windowStart, windowEnd] = trendForIssuesMock.mock.calls[0];
		expect(ids).toEqual([ISSUE_1, ISSUE_2]);
		const HOUR = 60 * 60 * 1000;
		expect(windowEnd.getTime() % HOUR).toBe(0);
		expect(windowEnd.getTime()).toBe(Math.floor(before / HOUR) * HOUR);
		expect(windowStart.getTime()).toBe(windowEnd.getTime() - 23 * HOUR);

		expect(data.issues).toHaveLength(2);
		for (const issue of data.issues) {
			expect(issue.trend24h).toHaveLength(24);
			expect(issue.trend24h.every((n: number) => Number.isInteger(n))).toBe(true);
		}
		expect(data.issues[0].trend24h[23]).toBe(5);
		expect(data.issues[1].trend24h).toEqual(new Array(24).fill(0));
	});

	// Decision 22: the two existing columns ride on every listed row — the projection is the query's
	// (asserted against Postgres), the page data carrying them through unchanged is the loader's.
	it('carries regressionStatus and lastRegressedAt on every list issue', async () => {
		const regressedAt = new Date('2026-09-13T12:30:00Z');
		listIssuesMock.mockResolvedValue([
			listedIssue(ISSUE_1, { regressionStatus: 'regressed', lastRegressedAt: regressedAt }),
			listedIssue(ISSUE_2),
		]);
		trendForIssuesMock.mockResolvedValue(
			new Map([
				[ISSUE_1, new Array(24).fill(0)],
				[ISSUE_2, new Array(24).fill(0)],
			])
		);

		const data = await loadWith('');

		expect(data.issues[0]).toMatchObject({ regressionStatus: 'regressed', lastRegressedAt: regressedAt });
		expect(data.issues[1]).toMatchObject({ regressionStatus: 'none', lastRegressedAt: null });
		for (const issue of data.issues) {
			expect(issue).toHaveProperty('regressionStatus');
			expect(issue).toHaveProperty('lastRegressedAt');
		}
	});

	// The `inArray([])` hazard (§4.1): an empty page — `new=24h` with nothing new, a page past the
	// end — has nothing to attach a series to and must not reach the reader with `[]`.
	it('never calls the reader with [] when the list is empty, and still returns issues: []', async () => {
		listIssuesMock.mockResolvedValue([]);

		const data = await loadWith('?new=24h&page=99');

		expect(trendForIssuesMock).not.toHaveBeenCalled();
		expect(data.issues).toEqual([]);
		expect(data.filters.new).toBe('24h');
	});
});

describe('GET / page load — pagination limit', () => {
	it('defaults to limit=20 when limit query param is absent', async () => {
		const data = await loadWith('');
		expect(data.pagination.limit).toBe(20);
		expect(listIssuesMock).toHaveBeenCalledWith(expect.anything(), 1, 20);
	});

	it('accepts allowed limits 50 and 100', async () => {
		const data50 = await loadWith('?limit=50');
		expect(data50.pagination.limit).toBe(50);
		expect(listIssuesMock).toHaveBeenCalledWith(expect.anything(), 1, 50);

		const data100 = await loadWith('?limit=100');
		expect(data100.pagination.limit).toBe(100);
		expect(listIssuesMock).toHaveBeenCalledWith(expect.anything(), 1, 100);
	});

	it('falls back to 20 for invalid or unallowed limits', async () => {
		const dataInvalid = await loadWith('?limit=999');
		expect(dataInvalid.pagination.limit).toBe(20);

		const dataNegative = await loadWith('?limit=-10');
		expect(dataNegative.pagination.limit).toBe(20);

		const dataNaN = await loadWith('?limit=foo');
		expect(dataNaN.pagination.limit).toBe(20);
	});
});

describe('GET / page load — claimant facets and organization users', () => {
	it('passes claim filter through to listIssues and countIssues and echoes in filters.claim', async () => {
		const data = await loadWith('?claim=claimed-me');
		expect(data.filters.claim).toBe('claimed-me');
		expect(listIssuesMock.mock.calls[0][0]).toMatchObject({ claim: 'claimed-me' });
		expect(countIssuesMock.mock.calls[0][0]).toMatchObject({ claim: 'claimed-me' });
	});

	it('returns claimantFacets and maps orgUsers and usersById with human-readable attributes', async () => {
		getClaimantFacetCountsMock.mockResolvedValueOnce({
			total: 5,
			unclaimed: 2,
			claimedMe: 1,
			claimedAgent: 1,
			byClaimant: { 'user-1': 1, 'agent-1': 1, 'unclaimed': 2 },
			claimantTypes: { 'agent-1': 'agent', 'user-1': 'user' },
		});
		listOrgUsersMock.mockResolvedValueOnce([
			{ id: 'user-2', name: 'Alice Smith', email: 'alice@example.com' },
			{ id: 'user-3', name: null, email: 'bob@example.com' },
		]);
		listOrgAgentsMock.mockResolvedValueOnce([
			{ id: 'agent-1', name: 'AutoFix Agent' },
		]);
		getUserMock.mockResolvedValueOnce({ id: 'user-1', email: 'me@example.com', name: 'Current User' });

		const data = await loadWith('');
		expect(data.claimantFacets).toEqual({
			total: 5,
			unclaimed: 2,
			claimedMe: 1,
			claimedAgent: 1,
			byClaimant: { 'user-1': 1, 'agent-1': 1, 'unclaimed': 2 },
			claimantTypes: { 'agent-1': 'agent', 'user-1': 'user' },
		});
		expect(data.usersById['user-2']).toEqual({ id: 'user-2', name: 'Alice Smith', email: 'alice@example.com' });
		expect(data.usersById['user-1']).toMatchObject({ id: 'user-1', email: 'me@example.com' });
		expect(data.agentsById['agent-1']).toEqual({ id: 'agent-1', name: 'AutoFix Agent' });
		expect(data.orgAgents).toEqual([
			{ id: 'agent-1', name: 'AutoFix Agent' },
		]);
		expect(data.orgUsers).toEqual(
			expect.arrayContaining([
				expect.objectContaining({ id: 'user-2', name: 'Alice Smith' }),
				expect.objectContaining({ id: 'user-3', email: 'bob@example.com' }),
			])
		);
	});
});

