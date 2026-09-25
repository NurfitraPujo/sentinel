import { describe, it, expect, vi, beforeEach } from 'vitest';
import { error } from '@sveltejs/kit';

// sentinel-issue-tag-facets spec §7.1 (TASK-02): the issue page's loader reads the tag rollup ONCE per
// load, AFTER both access checks (the project dimension and the environment dimension), and never when
// either refuses; the blocks come back at top level as `tagFacets`, beside `trend`. `load` is called
// without a `url`, which the loader must tolerate.

const order: string[] = [];
const authMock = vi.fn();
const requireProjectAccessMock = vi.fn(async (...args: unknown[]) => {
	order.push(`requireProjectAccess:${String(args[2])}`);
	return { organizationId: 'org-1' };
});
const resolveEnvironmentScopeMock = vi.fn(async () => {
	order.push('resolveEnvironmentScope');
	return null as string[] | null;
});
const getTagFacetsMock = vi.fn(async (_issueId: string) => {
	order.push('getTagFacets');
	return [{ key: 'release', value: '1.0', count: 3, keyTotal: 3, derivedOrd: 1 }];
});

vi.mock('$env/dynamic/private', () => ({ env: {} }));
vi.mock('$lib/server/issue-access', () => ({ requireProjectAccess: requireProjectAccessMock }));
vi.mock('$lib/server/environment-access', () => ({
	resolveEnvironmentScope: resolveEnvironmentScopeMock,
	resolveEnvironmentScopes: vi.fn(async () => new Map()),
}));
vi.mock('$lib/db/queries/environments', () => ({ resolveEnvironmentDisplayName: vi.fn(async () => 'Production') }));
vi.mock('$lib/server/queries/issue-queries', () => ({
	issueQueries: {
		getIssueById: vi.fn(async () => ISSUE),
		getProjectById: vi.fn(async () => ({ id: 'proj-1', name: 'P' })),
		getOccurrencesByIssueId: vi.fn(async () => []),
		countOccurrencesByIssueId: vi.fn(async () => 0),
		trendForIssue: vi.fn(async () => ({ hourly: [], daily: [] })),
		getTagFacets: getTagFacetsMock,
	},
}));
vi.mock('$lib/db/queries/issues', () => ({ getIssueRelations: vi.fn(async () => []) }));
vi.mock('$lib/db/queries/releases', () => ({ listPickerReleases: vi.fn(async () => []) }));

const ISSUE = {
	id: '11111111-1111-1111-1111-111111111111',
	projectId: 'proj-1',
	errorClass: 'TypeError',
	message: 'boom',
	environment: 'production',
	status: 'unresolved',
};

const { load } = await import('./+page.server');

function loadAs(userId = 'user-1') {
	authMock.mockResolvedValue({ user: { id: userId } });
	return load({ params: { id: ISSUE.id }, locals: { auth: authMock } } as never) as Promise<Record<string, unknown>>;
}

beforeEach(() => {
	vi.clearAllMocks();
	order.length = 0;
});

describe('GET /issues/{id} — the tag distribution panel (sentinel-issue-tag-facets §7.1)', () => {
	it('reads the rollup exactly once, after both access checks, and returns tagFacets beside trend', async () => {
		const data = await loadAs();
		expect(getTagFacetsMock).toHaveBeenCalledTimes(1);
		expect(getTagFacetsMock).toHaveBeenCalledWith(ISSUE.id);
		const at = order.indexOf('getTagFacets');
		expect(order.indexOf('requireProjectAccess:read')).toBeLessThan(at);
		expect(order.indexOf('resolveEnvironmentScope')).toBeLessThan(at);
		expect(data.tagFacets).toEqual([
			{ key: 'release', total: 3, values: [{ value: '1.0', count: 3, percent: 100 }], other: null },
		]);
		expect(data).toHaveProperty('trend');
	});

	it('never reads the rollup when the project dimension refuses (403)', async () => {
		requireProjectAccessMock.mockImplementationOnce(async () => {
			throw error(403, 'Forbidden');
		});
		await expect(loadAs()).rejects.toMatchObject({ status: 403 });
		expect(getTagFacetsMock).not.toHaveBeenCalled();
	});

	it('never reads the rollup when the environment dimension refuses (404)', async () => {
		resolveEnvironmentScopeMock.mockImplementationOnce(async () => ['staging']);
		await expect(loadAs()).rejects.toMatchObject({ status: 404 });
		expect(getTagFacetsMock).not.toHaveBeenCalled();
	});
});
