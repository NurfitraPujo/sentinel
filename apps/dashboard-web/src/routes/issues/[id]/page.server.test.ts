import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * sentinel-project-environments §4.5a — `GET /issues/{id}`, the FIRST of the two routes to one
 * issue. `routes/[orgSlug]/projects/[projectId]/issues/[issueId]/page.server.test.ts` is the second,
 * and both are asserted because §2.2's table lists the second one as "the door a reviewer forgets".
 *
 * Two layers, and they fail differently on purpose:
 *
 *  - the PROJECT dimension throws contract §7's project errors (decisions 35 and 46 — this loader
 *    used `checkProjectAccess`, which reads `project_members`, a table nothing in this application
 *    writes, so it `403`'d every user in any real deployment);
 *  - the ENVIRONMENT dimension throws `404 Issue not found`, never `403` (decision 20).
 */

const authMock = vi.fn();
const requireProjectAccessMock = vi.fn();
const resolveEnvironmentScopeMock = vi.fn();
// sentinel-project-environments §2.2, decision 37: the loader also resolves the caller's
// multi-project scopes and hands them to `getIssueRelations`, so a relation to an issue the viewer
// may not see comes back as a redacted placeholder rather than whole.
const resolveEnvironmentScopesMock = vi.fn(() => Promise.resolve(new Map()));
const resolveEnvironmentDisplayNameMock = vi.fn();
const getIssueByIdMock = vi.fn();
const getProjectByIdMock = vi.fn();
const getOccurrencesByIssueIdMock = vi.fn();
const countOccurrencesByIssueIdMock = vi.fn();
const getIssueRelationsMock = vi.fn();
// sentinel-issue-trend §4.2: the detail's series reader. What it returns for an issue against a real
// Postgres is issue-queries.trend.integration.test.ts's boundary; this file owns WHEN it is asked —
// after both access checks — and that the page data carries both series through.
const trendForIssueMock = vi.fn();

// sentinel-affected-users decision 21: `retentionDays` comes from `$lib/server/retention-window`,
// which reads `DATA_RETENTION_DAYS` fresh from `$env/dynamic/private` on every call — a mutable env
// object varied per test, and the helper spied on (not replaced) so the assertion is that the loader
// asked the SHARED helper the cron route and `/` read.
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

vi.mock('$lib/server/issue-access', () => ({
	requireProjectAccess: requireProjectAccessMock,
}));

vi.mock('$lib/server/environment-access', () => ({
	resolveEnvironmentScope: resolveEnvironmentScopeMock,
	resolveEnvironmentScopes: resolveEnvironmentScopesMock,
}));

vi.mock('$lib/db/queries/environments', () => ({
	resolveEnvironmentDisplayName: resolveEnvironmentDisplayNameMock,
}));

vi.mock('$lib/server/queries/issue-queries', () => ({
	issueQueries: {
		getIssueById: getIssueByIdMock,
		getProjectById: getProjectByIdMock,
		getOccurrencesByIssueId: getOccurrencesByIssueIdMock,
		countOccurrencesByIssueId: countOccurrencesByIssueIdMock,
		trendForIssue: trendForIssueMock,
	},
}));

vi.mock('$lib/db/queries/issues', () => ({
	getIssueRelations: getIssueRelationsMock,
}));

const { load } = await import('./+page.server');

const ISSUE = {
	id: '11111111-1111-1111-1111-111111111111',
	projectId: 'proj-1',
	errorClass: 'TypeError',
	message: 'boom',
	environment: 'production',
	status: 'unresolved',
};

const HOUR = 60 * 60 * 1000;
const DAY = 24 * HOUR;

// A literal of the reader's shape (§4.2): 24 hourly and 30 daily points, dense, oldest first.
function emptyTrend() {
	return {
		hourly: Array.from({ length: 24 }, (_, i) => ({
			start: new Date(Date.UTC(2026, 8, 12, 15) + i * HOUR).toISOString(),
			count: 0,
		})),
		daily: Array.from({ length: 30 }, (_, i) => ({
			start: new Date(Date.UTC(2026, 7, 15) + i * DAY).toISOString(),
			count: 0,
		})),
	};
}

function makeLocals(userId: string | null) {
	return { auth: authMock.mockResolvedValue(userId ? { user: { id: userId } } : null) };
}

async function loadAs(userId: string | null) {
	return await load({ params: { id: ISSUE.id }, locals: makeLocals(userId) } as any);
}

async function catchLoad(userId: string | null) {
	try {
		await loadAs(userId);
	} catch (e) {
		return e as any;
	}
	return undefined;
}

beforeEach(() => {
	vi.clearAllMocks();
	delete mockEnv.DATA_RETENTION_DAYS;
	getIssueByIdMock.mockResolvedValue(ISSUE);
	getProjectByIdMock.mockResolvedValue({ id: 'proj-1', name: 'My Project' });
	getOccurrencesByIssueIdMock.mockResolvedValue([]);
	countOccurrencesByIssueIdMock.mockResolvedValue(0);
	resolveEnvironmentDisplayNameMock.mockResolvedValue('production');
	resolveEnvironmentScopeMock.mockResolvedValue(null);
	requireProjectAccessMock.mockResolvedValue({ projectId: 'proj-1', organizationId: 'org-1' });
	getIssueRelationsMock.mockResolvedValue([]);
	trendForIssueMock.mockResolvedValue(emptyTrend());
});

describe('/issues/[id] load (§4.5a)', () => {
	it('401s an unauthenticated request', async () => {
		const caught = await catchLoad(null);

		expect(caught).toBeDefined();
		expect(caught.status).toBe(401);
	});

	it('404s when the issue does not exist', async () => {
		getIssueByIdMock.mockResolvedValue(null);

		const caught = await catchLoad('user-1');

		expect(caught.status).toBe(404);
		expect(requireProjectAccessMock).not.toHaveBeenCalled();
	});

	// Decision 46: the swap is the fix. `checkProjectAccess(..., 'viewer')` compared a project
	// `roleHierarchy` that has no entry for an organization role, so widening it in place would have
	// been a silent no-op.
	it('resolves the project dimension through requireProjectAccess at read', async () => {
		const result: any = await loadAs('user-1');

		expect(requireProjectAccessMock).toHaveBeenCalledWith('user-1', 'proj-1', 'read');
		expect(result.issue).toEqual(ISSUE);
	});

	it('propagates the project-dimension refusal from the helper', async () => {
		requireProjectAccessMock.mockRejectedValue({ status: 403 });

		const caught = await catchLoad('user-2');

		expect(caught.status).toBe(403);
	});

	// §4.5a layer 2, decision 20 — flow `D5`.
	it('404s — never 403 — on an issue outside the caller’s environment grant', async () => {
		resolveEnvironmentScopeMock.mockResolvedValue(['staging']);

		const caught = await catchLoad('user-1');

		expect(caught.status).toBe(404);
		expect(getIssueRelationsMock).not.toHaveBeenCalled();
	});

	it('serves the issue when the grant covers its environment', async () => {
		resolveEnvironmentScopeMock.mockResolvedValue(['production']);

		const result: any = await loadAs('user-1');

		expect(result.issue).toEqual(ISSUE);
	});

	// Flow `D2`: the heading showed `occurrences.length`, a page size capped at 100, as a total.
	it('returns a real occurrence COUNT(*) beside the capped page of occurrences', async () => {
		getOccurrencesByIssueIdMock.mockResolvedValue(new Array(100).fill({ id: 'occ' }));
		countOccurrencesByIssueIdMock.mockResolvedValue(1755);

		const result: any = await loadAs('user-1');

		expect(result.occurrences).toHaveLength(100);
		expect(result.occurrenceTotal).toBe(1755);
	});

	// §4.5a: `environmentDisplayName`, resolved for THIS project, falling back to the raw name.
	it('returns the environment display name resolved for this project', async () => {
		resolveEnvironmentDisplayNameMock.mockResolvedValue('Production');

		const result: any = await loadAs('user-1');

		expect(resolveEnvironmentDisplayNameMock).toHaveBeenCalledWith('proj-1', 'production');
		expect(result.environmentDisplayName).toBe('Production');
	});
});

// sentinel-affected-users §4.2, decision 21 — flow `D1`'s number. The two counts arrive on `issue`
// with the row (`getIssueById` has no projection); the loader's one change is `retentionDays`.
describe('/issues/[id] load — retentionDays (sentinel-affected-users §4.2)', () => {
	it('returns retentionDays 30 beside issue when DATA_RETENTION_DAYS is unset, from the shared helper', async () => {
		const result: any = await loadAs('user-1');

		expect(result.retentionDays).toBe(30);
		expect(result.issue).toEqual(ISSUE);
		expect(getRetentionDaysSpy).toHaveBeenCalled();
	});

	it('returns the parsed DATA_RETENTION_DAYS when it is set, as an integer', async () => {
		mockEnv.DATA_RETENTION_DAYS = '90';

		const result: any = await loadAs('user-1');

		expect(result.retentionDays).toBe(90);
		expect(Number.isInteger(result.retentionDays)).toBe(true);
	});
});

/**
 * sentinel-issue-trend §4.2, decision 11 — flow `D1`'s page data. The reader's densification is
 * asserted against a real Postgres in `issue-queries.trend.integration.test.ts`; this file owns that
 * the loader asks for THIS issue over the 30-day calendar window, only after both access checks, and
 * returns what the reader gave it as `trend` beside `issue`.
 */
describe('/issues/[id] load — trend (sentinel-issue-trend §4.2)', () => {
	it('returns trend.hourly (24) and trend.daily (30) beside issue, from the reader', async () => {
		const trend = emptyTrend();
		trend.hourly[23].count = 17;
		trend.daily[29].count = 212;
		trendForIssueMock.mockResolvedValue(trend);

		const result: any = await loadAs('user-1');

		expect(result.issue).toEqual(ISSUE);
		expect(result.trend.hourly).toHaveLength(24);
		expect(result.trend.daily).toHaveLength(30);
		expect(result.trend.hourly[23].count).toBe(17);
		expect(result.trend.daily[29].count).toBe(212);
		expect(result.trend).toBe(trend);
	});

	// §3's windows, taken from one instant: `windowEnd = trunc(now)`, `windowStart` = 00:00:00Z of
	// (UTC today − 29 days) — 29 full UTC days plus today's hours so far.
	it('asks the reader for this issue over the 30 UTC calendar days ending at the current hour', async () => {
		const before = Date.now();
		await loadAs('user-1');

		expect(trendForIssueMock).toHaveBeenCalledTimes(1);
		const [issueId, windowStart, windowEnd] = trendForIssueMock.mock.calls[0];
		expect(issueId).toBe(ISSUE.id);
		expect(windowEnd.getTime()).toBe(Math.floor(before / HOUR) * HOUR);
		expect(windowStart.getTime() % DAY).toBe(0);
		expect(windowStart.getTime()).toBe(Math.floor(windowEnd.getTime() / DAY) * DAY - 29 * DAY);
	});

	// §2: the trend query runs AFTER `requireProjectAccess` and the environment `404`, keyed on the
	// issue the loader has already been allowed to read. Neither refusal may reach the bucket table.
	it('does not read the buckets when requireProjectAccess refuses', async () => {
		requireProjectAccessMock.mockRejectedValue({ status: 403 });

		const caught = await catchLoad('user-2');

		expect(caught.status).toBe(403);
		expect(trendForIssueMock).not.toHaveBeenCalled();
	});

	it('does not read the buckets on the environment 404', async () => {
		resolveEnvironmentScopeMock.mockResolvedValue(['staging']);

		const caught = await catchLoad('user-1');

		expect(caught.status).toBe(404);
		expect(trendForIssueMock).not.toHaveBeenCalled();
	});

	it('reads the buckets only once both checks have passed', async () => {
		const order: string[] = [];
		requireProjectAccessMock.mockImplementation(async () => {
			order.push('project');
			return { projectId: 'proj-1', organizationId: 'org-1' };
		});
		resolveEnvironmentScopeMock.mockImplementation(async () => {
			order.push('environment');
			return null;
		});
		trendForIssueMock.mockImplementation(async () => {
			order.push('trend');
			return emptyTrend();
		});

		await loadAs('user-1');

		expect(order).toEqual(['project', 'environment', 'trend']);
	});
});
