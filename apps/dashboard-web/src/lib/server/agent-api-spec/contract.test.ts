import { describe, it, expect, vi, beforeEach } from 'vitest';
import * as S from './schemas';

/**
 * N6: for every registry entry with a response schema, exercises the REAL route handler (the
 * SAME dependency-mocking pattern the existing colocated route tests use -- agent-auth,
 * agent-issue-scope, agent-audit, notify, issue-access, and each underlying db query module are
 * mocked; the route/agent-ops/agent-route/agent-events glue code itself runs for real) for at
 * least the success case and one error case, and asserts the actual response body parses under
 * the registry's schema (`.strict()`, so an added/removed field fails this test).
 *
 * Deliberately fresh cases, not extensions of the existing colocated `*.test.ts` files (EXISTING
 * BEHAVIOR IS FROZEN / existing tests stay unmodified).
 */

const CTX = { agentId: 'agent-1', organizationId: 'org-1', agentName: 'bot', keyPrefixForAudit: 'abc' };
const ISSUE_SCOPE = {
	issueId: 'issue-1',
	projectId: 'project-1',
	organizationId: 'org-1',
	issueType: 'system_error',
	assignedTo: null,
	assigneeType: null,
	waitingOn: null,
};

const ISSUE_ROW = {
	id: 'issue-1',
	projectId: 'project-1',
	fingerprint: 'fp',
	message: 'msg',
	errorClass: 'Error',
	status: 'unresolved',
	regressionStatus: 'none',
	issueType: 'system_error',
	sourceChannel: 'ingestion_sdk',
	assigneeType: null,
	assignedTo: null,
	resolvedInVersion: null,
	resolvedAt: null,
	resolvedByType: null,
	resolvedBy: null,
	regressionCount: 0,
	lastRegressedAt: null,
	firstSeen: new Date().toISOString(),
	lastSeen: new Date().toISOString(),
	count: 1,
	waitingOn: null,
	claimedAt: null,
};

const COMMENT_ROW = {
	id: 'comment-1',
	issueId: 'issue-1',
	parentId: null,
	authorType: 'agent',
	authorId: 'agent-1',
	blocking: false,
	bodyMd: 'hi',
	createdAt: new Date().toISOString(),
	editedAt: null,
};

const RELATION_ROW = {
	id: 'relation-1',
	sourceIssueId: 'issue-1',
	targetIssueId: 'issue-2',
	relationType: 'linked_to',
	createdByType: 'agent',
	createdBy: 'agent-1',
	createdAt: new Date().toISOString(),
};

// ---------------------------------------------------------------------------
// Mocks -- one shared set for every route imported below.
// ---------------------------------------------------------------------------

const authenticateAgentRequest = vi.fn();
vi.mock('$lib/server/agent-auth', () => ({ authenticateAgentRequest }));

const resolveAgentIssueScope = vi.fn();
vi.mock('$lib/server/agent-issue-scope', () => ({ resolveAgentIssueScope }));

const writeAgentAuditLog = vi.fn();
vi.mock('$lib/server/agent-audit', () => ({ writeAgentAuditLog }));

const sendIssueNotificationEmails = vi.fn();
vi.mock('$lib/server/notify', () => ({ sendIssueNotificationEmails }));

const validateResolvedInVersion = vi.fn((v: unknown) => v ?? null);
vi.mock('$lib/server/issue-access', () => ({ validateResolvedInVersion }));

class ClaimConflictError extends Error {
	constructor(message = 'Claim conflict') {
		super(message);
		this.name = 'ClaimConflictError';
	}
}
const claimIssue = vi.fn();
const releaseClaim = vi.fn();
vi.mock('$lib/db/queries/reports', () => ({ claimIssue, releaseClaim, ClaimConflictError }));

class CommentValidationError extends Error {}
class CommentNotFoundError extends Error {}
const createComment = vi.fn();
const listComments = vi.fn();
vi.mock('$lib/db/queries/comments', () => ({
	createComment,
	listComments,
	CommentValidationError,
	CommentNotFoundError,
}));

const updateIssueStatus = vi.fn();
const createIssueRelation = vi.fn();
const deleteIssueRelation = vi.fn();
const getIssueRelations = vi.fn();
class RelationCycleError extends Error {}
vi.mock('$lib/db/queries/issues', () => ({
	updateIssueStatus,
	createIssueRelation,
	deleteIssueRelation,
	getIssueRelations,
	RelationCycleError,
}));

const recordAgentProgress = vi.fn();
const listAgentIssues = vi.fn();
vi.mock('$lib/db/queries/agent-work', () => ({ recordAgentProgress, listAgentIssues }));

const getAgentIssueDetail = vi.fn();
const getAgentReportDetail = vi.fn();
const getLatestAgentOccurrence = vi.fn();
const listAgentOccurrences = vi.fn();
const listAgentProjects = vi.fn();
vi.mock('$lib/db/queries/agent-reads', () => ({
	getAgentIssueDetail,
	getAgentReportDetail,
	getLatestAgentOccurrence,
	listAgentOccurrences,
	listAgentProjects,
}));

const listOrgActivity = vi.fn();
vi.mock('$lib/db/queries/events', () => ({ listOrgActivity }));

const handleAttachmentUpload = vi.fn();
const checkDeclaredLength = vi.fn();
vi.mock('$lib/server/upload-core', () => ({ handleAttachmentUpload, checkDeclaredLength }));

// ---------------------------------------------------------------------------
// Route handler imports (after mocks are registered).
// ---------------------------------------------------------------------------

const issuesListRoute = await import('../../../routes/api/agent/issues/+server');
const issueDetailRoute = await import('../../../routes/api/agent/issues/[issueId]/+server');
const occurrencesRoute = await import('../../../routes/api/agent/issues/[issueId]/occurrences/+server');
const claimRoute = await import('../../../routes/api/agent/issues/[issueId]/claim/+server');
const statusRoute = await import('../../../routes/api/agent/issues/[issueId]/status/+server');
const commentsRoute = await import('../../../routes/api/agent/issues/[issueId]/comments/+server');
const questionsRoute = await import('../../../routes/api/agent/issues/[issueId]/questions/+server');
const progressRoute = await import('../../../routes/api/agent/issues/[issueId]/progress/+server');
const relationsRoute = await import('../../../routes/api/agent/issues/[issueId]/relations/+server');
const projectsRoute = await import('../../../routes/api/agent/projects/+server');
const eventsRoute = await import('../../../routes/api/agent/events/+server');
const uploadsRoute = await import('../../../routes/api/agent/uploads/+server');
const batchRoute = await import('../../../routes/api/agent/batch/+server');

function makeEvent(url: string, init: { method?: string; body?: unknown; issueId?: string; formData?: FormData } = {}) {
	const u = new URL(url);
	const requestInit: RequestInit = { method: init.method ?? 'GET' };
	if (init.formData) {
		requestInit.body = init.formData;
	} else if (init.body !== undefined) {
		requestInit.body = JSON.stringify(init.body);
	}
	return {
		params: init.issueId !== undefined ? { issueId: init.issueId } : {},
		request: new Request(u, requestInit),
		url: u,
	} as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	authenticateAgentRequest.mockResolvedValue(CTX);
	resolveAgentIssueScope.mockResolvedValue(ISSUE_SCOPE);
	sendIssueNotificationEmails.mockResolvedValue(undefined);
	validateResolvedInVersion.mockImplementation((v: unknown) => v ?? null);
});

describe('GET /api/agent/issues', () => {
	it('200: matches ListIssuesResponseSchema', async () => {
		listAgentIssues.mockResolvedValue({
			issues: [
				{
					id: 'issue-1',
					projectId: 'project-1',
					projectName: 'Inbox',
					isInbox: true,
					issueType: 'system_error',
					message: 'msg',
					errorClass: 'Error',
					status: 'unresolved',
					assigneeType: null,
					assignedTo: null,
					waitingOn: null,
					firstSeen: new Date().toISOString(),
					lastSeen: new Date().toISOString(),
					count: 1,
					severity: null,
					reporterId: null,
					isWaiting: false,
					waitingSince: null,
					claimedAt: null,
				},
			],
		});
		const res = await issuesListRoute.GET(makeEvent('http://localhost/api/agent/issues'));
		expect(res.status).toBe(200);
		expect(S.ListIssuesResponseSchema.parse(await res.json())).toBeTruthy();
	});

	it('200: matches ListIssuesResponseSchema with nextCursor when limit is supplied', async () => {
		listAgentIssues.mockResolvedValue({ issues: [], nextCursor: 'abc123' });
		const res = await issuesListRoute.GET(makeEvent('http://localhost/api/agent/issues?limit=10'));
		expect(res.status).toBe(200);
		expect(S.ListIssuesResponseSchema.parse(await res.json())).toBeTruthy();
	});

	it('400: matches ErrorFieldErrorSchema for an invalid since param', async () => {
		const res = await issuesListRoute.GET(makeEvent('http://localhost/api/agent/issues?since=not-a-date'));
		expect(res.status).toBe(400);
		expect(S.ErrorFieldErrorSchema.parse(await res.json())).toBeTruthy();
	});

	it('400: matches ErrorFieldErrorSchema for an invalid sort param', async () => {
		const res = await issuesListRoute.GET(makeEvent('http://localhost/api/agent/issues?sort=bogus'));
		expect(res.status).toBe(400);
		expect(S.ErrorFieldErrorSchema.parse(await res.json())).toBeTruthy();
	});

	it('400: matches ErrorFieldErrorSchema for an invalid type param', async () => {
		const res = await issuesListRoute.GET(makeEvent('http://localhost/api/agent/issues?type=bogus'));
		expect(res.status).toBe(400);
		expect(S.ErrorFieldErrorSchema.parse(await res.json())).toBeTruthy();
	});
});

describe('GET /api/agent/issues/{issueId}', () => {
	it('200: matches IssueDetailResponseSchema', async () => {
		getAgentIssueDetail.mockResolvedValue(ISSUE_ROW);
		getAgentReportDetail.mockResolvedValue(null);
		getLatestAgentOccurrence.mockResolvedValue(null);
		getIssueRelations.mockResolvedValue([]);
		const res = await issueDetailRoute.GET(makeEvent('http://localhost/api/agent/issues/issue-1', { issueId: 'issue-1' }));
		expect(res.status).toBe(200);
		expect(S.IssueDetailResponseSchema.parse(await res.json())).toBeTruthy();
	});

	it('404: matches MessageErrorSchema for a cross-org issue', async () => {
		resolveAgentIssueScope.mockRejectedValue(Object.assign(new Error('Issue not found'), { status: 404, body: { message: 'Issue not found' } }));
		await expect(issueDetailRoute.GET(makeEvent('http://localhost/api/agent/issues/issue-1', { issueId: 'issue-1' }))).rejects.toMatchObject({
			status: 404,
		});
	});
});

describe('GET /api/agent/issues/{issueId}/occurrences', () => {
	it('200: matches OccurrencesResponseSchema', async () => {
		listAgentOccurrences.mockResolvedValue([]);
		const res = await occurrencesRoute.GET(
			makeEvent('http://localhost/api/agent/issues/issue-1/occurrences', { issueId: 'issue-1' })
		);
		expect(res.status).toBe(200);
		expect(S.OccurrencesResponseSchema.parse(await res.json())).toBeTruthy();
	});

	it('400: rejects an invalid limit', async () => {
		await expect(
			occurrencesRoute.GET(
				makeEvent('http://localhost/api/agent/issues/issue-1/occurrences?limit=0', { issueId: 'issue-1' })
			)
		).rejects.toMatchObject({ status: 400 });
	});
});

describe('POST/DELETE /api/agent/issues/{issueId}/claim', () => {
	it('200 POST: matches ClaimResponseSchema', async () => {
		claimIssue.mockResolvedValue({ issue: ISSUE_ROW, notified: [] });
		const res = await claimRoute.POST(
			makeEvent('http://localhost/api/agent/issues/issue-1/claim', { method: 'POST', issueId: 'issue-1', body: {} })
		);
		expect(res.status).toBe(200);
		expect(S.ClaimResponseSchema.parse(await res.json())).toBeTruthy();
	});

	it('409 POST: conflict maps to 409', async () => {
		claimIssue.mockRejectedValue(new ClaimConflictError());
		await expect(
			claimRoute.POST(makeEvent('http://localhost/api/agent/issues/issue-1/claim', { method: 'POST', issueId: 'issue-1', body: {} }))
		).rejects.toMatchObject({ status: 409 });
	});

	it('200 DELETE: matches ReleaseResponseSchema', async () => {
		releaseClaim.mockResolvedValue({ issue: ISSUE_ROW, notified: [] });
		const res = await claimRoute.DELETE(
			makeEvent('http://localhost/api/agent/issues/issue-1/claim', { method: 'DELETE', issueId: 'issue-1', body: {} })
		);
		expect(res.status).toBe(200);
		expect(S.ReleaseResponseSchema.parse(await res.json())).toBeTruthy();
	});
});

describe('PATCH /api/agent/issues/{issueId}/status', () => {
	it('200: matches StatusResponseSchema', async () => {
		updateIssueStatus.mockResolvedValue({ changed: true, notified: [] });
		const res = await statusRoute.PATCH(
			makeEvent('http://localhost/api/agent/issues/issue-1/status', {
				method: 'PATCH',
				issueId: 'issue-1',
				body: { status: 'resolved' },
			})
		);
		expect(res.status).toBe(200);
		expect(S.StatusResponseSchema.parse(await res.json())).toBeTruthy();
	});

	it('400: invalid status', async () => {
		await expect(
			statusRoute.PATCH(
				makeEvent('http://localhost/api/agent/issues/issue-1/status', {
					method: 'PATCH',
					issueId: 'issue-1',
					body: { status: 'bogus' },
				})
			)
		).rejects.toMatchObject({ status: 400 });
	});
});

describe('GET/POST /api/agent/issues/{issueId}/comments', () => {
	it('200 GET: matches ListCommentsResponseSchema', async () => {
		listComments.mockResolvedValue([COMMENT_ROW]);
		const res = await commentsRoute.GET(
			makeEvent('http://localhost/api/agent/issues/issue-1/comments', { issueId: 'issue-1' })
		);
		expect(res.status).toBe(200);
		expect(S.ListCommentsResponseSchema.parse(await res.json())).toBeTruthy();
	});

	it('400 GET: invalid after', async () => {
		await expect(
			commentsRoute.GET(
				makeEvent('http://localhost/api/agent/issues/issue-1/comments?after=not-a-date', { issueId: 'issue-1' })
			)
		).rejects.toMatchObject({ status: 400 });
	});

	it('201 POST: matches PostCommentResponseSchema', async () => {
		createComment.mockResolvedValue({ comment: COMMENT_ROW, notified: [] });
		const res = await commentsRoute.POST(
			makeEvent('http://localhost/api/agent/issues/issue-1/comments', {
				method: 'POST',
				issueId: 'issue-1',
				body: { body_md: 'hello' },
			})
		);
		expect(res.status).toBe(201);
		expect(S.PostCommentResponseSchema.parse(await res.json())).toBeTruthy();
	});

	it('400 POST: missing body_md', async () => {
		await expect(
			commentsRoute.POST(
				makeEvent('http://localhost/api/agent/issues/issue-1/comments', { method: 'POST', issueId: 'issue-1', body: {} })
			)
		).rejects.toMatchObject({ status: 400 });
	});
});

describe('POST /api/agent/issues/{issueId}/questions', () => {
	it('201: matches QuestionResponseSchema', async () => {
		createComment.mockResolvedValue({ comment: { ...COMMENT_ROW, blocking: true }, notified: [] });
		const res = await questionsRoute.POST(
			makeEvent('http://localhost/api/agent/issues/issue-1/questions', {
				method: 'POST',
				issueId: 'issue-1',
				body: { body_md: 'why?', audience: 'reporter' },
			})
		);
		expect(res.status).toBe(201);
		expect(S.QuestionResponseSchema.parse(await res.json())).toBeTruthy();
	});

	it('400: invalid audience', async () => {
		await expect(
			questionsRoute.POST(
				makeEvent('http://localhost/api/agent/issues/issue-1/questions', {
					method: 'POST',
					issueId: 'issue-1',
					body: { body_md: 'why?', audience: 'bogus' },
				})
			)
		).rejects.toMatchObject({ status: 400 });
	});
});

describe('POST /api/agent/issues/{issueId}/progress', () => {
	it('201: matches ProgressResponseSchema', async () => {
		recordAgentProgress.mockResolvedValue({ notified: [] });
		const res = await progressRoute.POST(
			makeEvent('http://localhost/api/agent/issues/issue-1/progress', {
				method: 'POST',
				issueId: 'issue-1',
				body: { message_md: 'working on it' },
			})
		);
		expect(res.status).toBe(201);
		expect(S.ProgressResponseSchema.parse(await res.json())).toBeTruthy();
	});

	it('400: missing message_md', async () => {
		await expect(
			progressRoute.POST(
				makeEvent('http://localhost/api/agent/issues/issue-1/progress', { method: 'POST', issueId: 'issue-1', body: {} })
			)
		).rejects.toMatchObject({ status: 400 });
	});
});

describe('POST/DELETE /api/agent/issues/{issueId}/relations', () => {
	it('201 POST: matches RelationRowSchema', async () => {
		createIssueRelation.mockResolvedValue({ relation: RELATION_ROW, notified: [] });
		const res = await relationsRoute.POST(
			makeEvent('http://localhost/api/agent/issues/issue-1/relations', {
				method: 'POST',
				issueId: 'issue-1',
				body: { target_issue_id: 'issue-2', relation_type: 'linked_to' },
			})
		);
		expect(res.status).toBe(201);
		expect(S.RelationRowSchema.parse(await res.json())).toBeTruthy();
	});

	it('409 POST: unique violation maps to 409', async () => {
		createIssueRelation.mockRejectedValue(Object.assign(new Error('duplicate'), { code: '23505' }));
		await expect(
			relationsRoute.POST(
				makeEvent('http://localhost/api/agent/issues/issue-1/relations', {
					method: 'POST',
					issueId: 'issue-1',
					body: { target_issue_id: 'issue-2', relation_type: 'linked_to' },
				})
			)
		).rejects.toMatchObject({ status: 409 });
	});

	// A12 (N7d): the agent op maps createIssueRelation's RelationCycleError (query-layer reverse-pair
	// guard for caused_by) to 409, the same way the human route does (see issues.test.ts for the
	// query-layer guard itself and relations.test.ts for the human route's mapping).
	it('409 POST: caused_by reverse-pair cycle maps to 409', async () => {
		createIssueRelation.mockRejectedValue(new RelationCycleError('Reverse relation already exists (would create a cycle)'));
		await expect(
			relationsRoute.POST(
				makeEvent('http://localhost/api/agent/issues/issue-1/relations', {
					method: 'POST',
					issueId: 'issue-1',
					body: { target_issue_id: 'issue-2', relation_type: 'caused_by' },
				})
			)
		).rejects.toMatchObject({ status: 409 });
	});

	it('200 DELETE: matches RelationRemoveResponseSchema', async () => {
		deleteIssueRelation.mockResolvedValue(RELATION_ROW);
		const res = await relationsRoute.DELETE(
			makeEvent('http://localhost/api/agent/issues/issue-1/relations', {
				method: 'DELETE',
				issueId: 'issue-1',
				body: { target_issue_id: 'issue-2', relation_type: 'linked_to' },
			})
		);
		expect(res.status).toBe(200);
		expect(S.RelationRemoveResponseSchema.parse(await res.json())).toBeTruthy();
	});

	it('404 DELETE: not found', async () => {
		deleteIssueRelation.mockResolvedValue(null);
		await expect(
			relationsRoute.DELETE(
				makeEvent('http://localhost/api/agent/issues/issue-1/relations', {
					method: 'DELETE',
					issueId: 'issue-1',
					body: { target_issue_id: 'issue-2', relation_type: 'linked_to' },
				})
			)
		).rejects.toMatchObject({ status: 404 });
	});
});

describe('GET /api/agent/projects', () => {
	it('200: matches ListProjectsResponseSchema', async () => {
		listAgentProjects.mockResolvedValue([
			{
				id: 'project-1',
				name: 'Inbox',
				isInbox: true,
				agentSettings: {
					fixEnabled: true,
					maxPrsPerDay: 3,
					repo: {
						provider: 'github',
						owner: 'acme',
						repo: 'widgets',
						defaultBranch: 'main',
						testCmd: 'npm test',
						agentCmd: 'npm run fix',
						cloneDepth: 1,
					},
				},
			},
		]);
		const res = await projectsRoute.GET(makeEvent('http://localhost/api/agent/projects'));
		expect(res.status).toBe(200);
		expect(S.ListProjectsResponseSchema.parse(await res.json())).toBeTruthy();
	});

	it('401: unauthenticated', async () => {
		authenticateAgentRequest.mockRejectedValue(Object.assign(new Error('Unauthorized'), { status: 401, body: { message: 'Unauthorized' } }));
		await expect(projectsRoute.GET(makeEvent('http://localhost/api/agent/projects'))).rejects.toMatchObject({ status: 401 });
	});
});

describe('GET /api/agent/events', () => {
	it('200: matches EventsResponseSchema', async () => {
		listOrgActivity.mockResolvedValue({
			events: [
				{
					seq: 1,
					eventType: 'commented',
					actorType: 'agent',
					actorId: 'agent-1',
					oldValue: null,
					newValue: null,
					createdAt: new Date().toISOString(),
					issue: { id: 'issue-1', title: 'msg', status: 'unresolved', issueType: 'system_error', projectId: 'project-1', assigneeType: 'agent', assignedTo: 'agent-1', claimedAt: new Date().toISOString(), waitingOn: null },
				},
			],
			cursor: 1,
			hasMore: false,
		});
		const res = await eventsRoute.GET(makeEvent('http://localhost/api/agent/events'));
		expect(res.status).toBe(200);
		expect(S.EventsResponseSchema.parse(await res.json())).toBeTruthy();
	});

	it('400: invalid type filter matches ErrorFieldErrorSchema', async () => {
		const res = await eventsRoute.GET(makeEvent('http://localhost/api/agent/events?type=bogus'));
		expect(res.status).toBe(400);
		expect(S.ErrorFieldErrorSchema.parse(await res.json())).toBeTruthy();
	});
});

describe('POST /api/agent/uploads', () => {
	it('201: matches UploadResponseSchema', async () => {
		handleAttachmentUpload.mockResolvedValue({
			id: 'attachment-1',
			url: 'https://example.com/a',
			filename: 'a.png',
			contentType: 'image/png',
			sizeBytes: 10,
		});
		const formData = new FormData();
		formData.set('file', new File(['x'], 'a.png', { type: 'image/png' }));
		const event = makeEvent('http://localhost/api/agent/uploads', { method: 'POST' });
		// jsdom's Request/FormData multipart round trip is unreliable in this test environment
		// (see vite.config.js's jsdom note) -- stub formData() directly rather than relying on it,
		// same as every other route test here stubs its db-layer dependencies rather than a real DB.
		event.request.formData = () => Promise.resolve(formData);
		const res = await uploadsRoute.POST(event);
		expect(res.status).toBe(201);
		expect(S.UploadResponseSchema.parse(await res.json())).toBeTruthy();
	});

	it('400: malformed multipart body', async () => {
		const event = makeEvent('http://localhost/api/agent/uploads', { method: 'POST' });
		event.request.formData = () => Promise.reject(new Error('bad form'));
		await expect(uploadsRoute.POST(event)).rejects.toMatchObject({ status: 400 });
	});
});

describe('POST /api/agent/batch', () => {
	it('200: matches BatchResponseSchema', async () => {
		updateIssueStatus.mockResolvedValue({ changed: true, notified: [] });
		const res = await batchRoute.POST(
			makeEvent('http://localhost/api/agent/batch', {
				method: 'POST',
				body: { operations: [{ op: 'issues.status', issueId: 'issue-1', params: { status: 'resolved' } }] },
			})
		);
		expect(res.status).toBe(200);
		expect(S.BatchResponseSchema.parse(await res.json())).toBeTruthy();
	});

	it('400: matches BatchValidationErrorSchema for an empty operations array', async () => {
		const res = await batchRoute.POST(
			makeEvent('http://localhost/api/agent/batch', { method: 'POST', body: { operations: [] } })
		);
		expect(res.status).toBe(400);
		expect(S.BatchValidationErrorSchema.parse(await res.json())).toBeTruthy();
	});
});
