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
	getIssueByIdMock.mockResolvedValue(ISSUE);
	getProjectByIdMock.mockResolvedValue({ id: 'proj-1', name: 'My Project' });
	getOccurrencesByIssueIdMock.mockResolvedValue([]);
	countOccurrencesByIssueIdMock.mockResolvedValue(0);
	resolveEnvironmentDisplayNameMock.mockResolvedValue('production');
	resolveEnvironmentScopeMock.mockResolvedValue(null);
	requireProjectAccessMock.mockResolvedValue({ projectId: 'proj-1', organizationId: 'org-1' });
	getIssueRelationsMock.mockResolvedValue([]);
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
