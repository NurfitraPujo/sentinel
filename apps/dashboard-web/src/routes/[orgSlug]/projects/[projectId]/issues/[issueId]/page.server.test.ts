import { describe, it, expect, vi, beforeEach } from 'vitest';

// D03: the route had no +page.server.ts at all, so `data` was undefined and the page fell through
// to hardcoded mock data. This test imports the REAL `load` function (unlike the fraudulent D13
// alerts test, which re-implemented its predicate inline and never imported `load` — deleting the
// loader would leave that test green). Deleting src/routes/.../+page.server.ts's `load` here makes
// every test below fail, because they call it directly.
//
// sentinel-project-environments §4.5a: this loader is the SECOND door to one issue, and it takes
// both of the first one's layers — decision 46's helper swap on the project dimension, and decision
// 20's `404` on the environment dimension.

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

function makeLocals(userId: string | null) {
	return { auth: authMock.mockResolvedValue(userId ? { user: { id: userId } } : null) };
}

const ISSUE = {
	id: '11111111-1111-1111-1111-111111111111',
	projectId: 'proj-1',
	errorClass: 'TypeError',
	message: 'boom',
	environment: 'production',
	status: 'unresolved',
	firstSeen: new Date('2026-01-01T00:00:00Z'),
	assigneeType: null,
	assignedTo: null,
};

beforeEach(() => {
	vi.clearAllMocks();
	getIssueByIdMock.mockResolvedValue(ISSUE);
	getProjectByIdMock.mockResolvedValue({ id: 'proj-1', name: 'My Project' });
	getOccurrencesByIssueIdMock.mockResolvedValue([]);
	countOccurrencesByIssueIdMock.mockResolvedValue(0);
	resolveEnvironmentDisplayNameMock.mockResolvedValue('production');
	// §2.2's absence-means-all: no grant row is the ordinary case, and it is unrestricted.
	resolveEnvironmentScopeMock.mockResolvedValue(null);
	requireProjectAccessMock.mockResolvedValue({ projectId: 'proj-1', organizationId: 'org-1' });
	getIssueRelationsMock.mockResolvedValue([
		{ id: 'rel-1', relationType: 'linked_to', direction: 'outgoing' },
	]);
});

async function loadAs(userId: string | null, projectId = 'proj-1') {
	return await load({
		params: { orgSlug: 'org', projectId, issueId: ISSUE.id },
		locals: makeLocals(userId),
	} as any);
}

async function catchLoad(userId: string | null, projectId = 'proj-1') {
	try {
		await loadAs(userId, projectId);
	} catch (e) {
		return e as any;
	}
	return undefined;
}

describe('/[orgSlug]/projects/[projectId]/issues/[issueId] load (D03)', () => {
	it('returns real issue, project, occurrence, and relation data to the page for an authorized member', async () => {
		// Cast to `any`: svelte-kit's generated $types resolves this loader's declared PageServerLoad
		// return type through App.PageData, which collapses to a `void` union in this repo regardless
		// of what the function actually returns (the same generated-types quirk tracked for the
		// observability loader). The function's actual runtime return value is asserted below exactly
		// as returned — this cast only works around svelte-check's static view of it in a test file.
		const result: any = await loadAs('user-1');

		expect(result.issue).toEqual(ISSUE);
		expect(result.project).toEqual({ id: 'proj-1', name: 'My Project' });
		expect(result.relations).toHaveLength(1);
		// Decision 46: the project check is `requireProjectAccess(userId, projectId, 'read')`, not
		// `checkProjectAccess(..., 'viewer')`. The old helper read `project_members` only — a table
		// nothing in this application writes — so this page threw `403` for every user in any real
		// deployment.
		expect(requireProjectAccessMock).toHaveBeenCalledWith('user-1', 'proj-1', 'read');
	});

	it('denies a user the project check refuses', async () => {
		requireProjectAccessMock.mockRejectedValue({ status: 403 });

		const caught = await catchLoad('user-2');

		expect(caught).toBeDefined();
		expect(caught.status).toBe(403);
	});

	it('401s an unauthenticated request', async () => {
		const caught = await catchLoad(null);

		expect(caught).toBeDefined();
		expect(caught.status).toBe(401);
	});

	it('404s when the issue does not belong to the requested project', async () => {
		getIssueByIdMock.mockResolvedValue({ ...ISSUE, projectId: 'some-other-project' });

		const caught = await catchLoad('user-1');

		expect(caught).toBeDefined();
		expect(caught.status).toBe(404);
	});

	// sentinel-project-environments §4.5a layer 2, decision 20 — flow `D6`, the second door.
	it('404s — never 403 — on an issue outside the caller’s environment grant', async () => {
		resolveEnvironmentScopeMock.mockResolvedValue(['staging']);

		const caught = await catchLoad('user-1');

		expect(caught).toBeDefined();
		// A `403` here would confirm that a production issue with this id exists, which is the fact
		// the boundary was drawn to withhold.
		expect(caught.status).toBe(404);
		expect(getIssueRelationsMock).not.toHaveBeenCalled();
	});

	it('serves the issue when the grant covers its environment', async () => {
		resolveEnvironmentScopeMock.mockResolvedValue(['production', 'staging']);

		const result: any = await loadAs('user-1');

		expect(result.issue).toEqual(ISSUE);
	});

	// §4.5a and flow `D2`: the heading was `Occurrence History ({occurrences.length})`, which is a
	// page size capped at 100 presented as a total.
	it('returns a real occurrence COUNT(*) beside the capped page of occurrences', async () => {
		getOccurrencesByIssueIdMock.mockResolvedValue(new Array(100).fill({ id: 'occ' }));
		countOccurrencesByIssueIdMock.mockResolvedValue(4213);

		const result: any = await loadAs('user-1');

		expect(result.occurrences).toHaveLength(100);
		expect(result.occurrenceTotal).toBe(4213);
	});

	// §4.5a: the registry's display name, falling back to the raw name. Decisions 8+9 make the
	// fallback the NORMAL path for a new environment, not an error case.
	it('returns the environment display name resolved for this project', async () => {
		resolveEnvironmentDisplayNameMock.mockResolvedValue('Production (EU)');

		const result: any = await loadAs('user-1');

		expect(resolveEnvironmentDisplayNameMock).toHaveBeenCalledWith('proj-1', 'production');
		expect(result.environmentDisplayName).toBe('Production (EU)');
	});
});
