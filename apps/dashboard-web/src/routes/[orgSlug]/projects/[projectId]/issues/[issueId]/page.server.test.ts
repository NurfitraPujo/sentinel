import { describe, it, expect, vi, beforeEach } from 'vitest';

// D03: the route had no +page.server.ts at all, so `data` was undefined and the page fell through
// to hardcoded mock data. This test imports the REAL `load` function (unlike the fraudulent D13
// alerts test, which re-implemented its predicate inline and never imported `load` — deleting the
// loader would leave that test green). Deleting src/routes/.../+page.server.ts's `load` here makes
// every test below fail, because they call it directly.

const authMock = vi.fn();
const checkProjectAccessMock = vi.fn();
const getIssueByIdMock = vi.fn();
const getProjectByIdMock = vi.fn();
const getOccurrencesByIssueIdMock = vi.fn();
const getIssueRelationsMock = vi.fn();

vi.mock('$lib/server/projects', () => ({
	checkProjectAccess: checkProjectAccessMock,
}));

vi.mock('$lib/server/queries/issue-queries', () => ({
	issueQueries: {
		getIssueById: getIssueByIdMock,
		getProjectById: getProjectByIdMock,
		getOccurrencesByIssueId: getOccurrencesByIssueIdMock,
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
	getIssueRelationsMock.mockResolvedValue([
		{ id: 'rel-1', relationType: 'linked_to', direction: 'outgoing' },
	]);
});

describe('/[orgSlug]/projects/[projectId]/issues/[issueId] load (D03)', () => {
	it('returns real issue, project, occurrence, and relation data to the page for an authorized member', async () => {
		checkProjectAccessMock.mockResolvedValue(true);

		// Cast to `any`: svelte-kit's generated $types resolves this loader's declared PageServerLoad
		// return type through App.PageData, which collapses to a `void` union in this repo regardless
		// of what the function actually returns (the same generated-types quirk tracked for the
		// observability loader). The function's actual runtime return value is asserted below exactly
		// as returned — this cast only works around svelte-check's static view of it in a test file.
		const result: any = await load({
			params: { orgSlug: 'org', projectId: 'proj-1', issueId: ISSUE.id },
			locals: makeLocals('user-1'),
		} as any);

		expect(result.issue).toEqual(ISSUE);
		expect(result.project).toEqual({ id: 'proj-1', name: 'My Project' });
		expect(result.relations).toHaveLength(1);
		expect(checkProjectAccessMock).toHaveBeenCalledWith('user-1', 'proj-1', 'viewer');
	});

	it('denies a user who is not a member of the issue project', async () => {
		checkProjectAccessMock.mockResolvedValue(false);

		let caught: any;
		try {
			await load({
				params: { orgSlug: 'org', projectId: 'proj-1', issueId: ISSUE.id },
				locals: makeLocals('user-2'),
			} as any);
		} catch (e) {
			caught = e;
		}

		expect(caught).toBeDefined();
		expect(caught.status).toBe(403);
	});

	it('401s an unauthenticated request', async () => {
		let caught: any;
		try {
			await load({
				params: { orgSlug: 'org', projectId: 'proj-1', issueId: ISSUE.id },
				locals: makeLocals(null),
			} as any);
		} catch (e) {
			caught = e;
		}

		expect(caught).toBeDefined();
		expect(caught.status).toBe(401);
	});

	it('404s when the issue does not belong to the requested project', async () => {
		checkProjectAccessMock.mockResolvedValue(true);
		getIssueByIdMock.mockResolvedValue({ ...ISSUE, projectId: 'some-other-project' });

		let caught: any;
		try {
			await load({
				params: { orgSlug: 'org', projectId: 'proj-1', issueId: ISSUE.id },
				locals: makeLocals('user-1'),
			} as any);
		} catch (e) {
			caught = e;
		}

		expect(caught).toBeDefined();
		expect(caught.status).toBe(404);
	});
});
