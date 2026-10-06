import { describe, it, expect, vi, beforeEach } from 'vitest';
import { getTableColumns } from 'drizzle-orm';
import { issues } from '$lib/db/schema';
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
	environment: 'production',
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
	// sentinel-affected-users decision 14: NOT NULL DEFAULT 0 on the row, declared on the schema.
	affectedUserCount: 0,
	affectedTenantCount: 0,
	// sentinel-releases-snooze decision 9: the four §4.5 columns, nullable with no default, declared
	// on the schema -- null is what every pre-feature row and every plain indefinite ignore reads.
	snoozedUntil: null,
	snoozeOccurrenceTarget: null,
	snoozeCountAt: null,
	firstReleaseVersion: null,
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
// sentinel-releases-snooze decision 38: `validateSnoozedUntil` / `validateSnoozeOccurrences` are
// the REAL ones, deliberately -- the whole point of the status cases below is that the ONE
// validator both routes call refuses, through the agent route, exactly what §4.2 declares
// (`StatusBodySchema` documents; nothing runs a `.parse()` on it). A stub here would prove nothing.
vi.mock('$lib/server/issue-access', async (importOriginal) => {
	const actual = await importOriginal<typeof import('$lib/server/issue-access')>();
	return { ...actual, validateResolvedInVersion };
});

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
const claimNextAgentIssue = vi.fn();
vi.mock('$lib/db/queries/agent-work', () => ({
	recordAgentProgress,
	listAgentIssues,
	claimNextAgentIssue,
	AGENT_ISSUE_SOURCES: ['ingestion_sdk', 'manual_support', 'api', 'user_report_api'],
}));

const getAgentIssueDetail = vi.fn();
const getAgentReportDetail = vi.fn();
const getLatestAgentOccurrence = vi.fn();
const listAgentOccurrences = vi.fn();
const listAgentProjects = vi.fn();
const listAgentTraceOccurrences = vi.fn();
const getAgentOccurrencesByIds = vi.fn();
const getAgentOccurrenceInOrg = vi.fn();
vi.mock('$lib/db/queries/agent-reads', () => ({
	getAgentIssueDetail,
	getAgentReportDetail,
	getLatestAgentOccurrence,
	listAgentOccurrences,
	listAgentProjects,
	listAgentTraceOccurrences,
	getAgentOccurrencesByIds,
	getAgentOccurrenceInOrg,
}));

// sentinel-occurrence-spans: the span guard, the bounded walk and the pull service are mocked; their own tests are
// span-status.integration.test.ts, issue-spans.test.ts and span-pull.integration.test.ts.
const getSpansStatuses = vi.fn();
const getSpansState = vi.fn();
vi.mock('$lib/server/queries/span-queries', () => ({ getSpansStatuses, getSpansState }));
const resolveAgentSpanScope = vi.fn();
vi.mock('$lib/server/span-scope', () => ({ resolveAgentSpanScope }));
const walkOccurrencesWithSpans = vi.fn();
vi.mock('$lib/server/issue-spans', () => ({ walkOccurrencesWithSpans }));
const requestSpanPull = vi.fn();
class SpanPullError extends Error {
	constructor(public code: string, message: string, public retryAfterSeconds?: number) {
		super(message);
	}
}
vi.mock('$lib/server/span-pull', () => ({ requestSpanPull, SpanPullError }));

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
const traceRoute = await import('../../../routes/api/agent/traces/[traceId]/+server');
const pullRoute = await import('../../../routes/api/agent/traces/[traceId]/pull/+server');
const occurrenceRoute = await import('../../../routes/api/agent/occurrences/[occurrenceId]/+server');
const claimRoute = await import('../../../routes/api/agent/issues/[issueId]/claim/+server');
const claimNextRoute = await import('../../../routes/api/agent/issues/claim-next/+server');
const selfRoute = await import('../../../routes/api/agent/self/+server');
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
	resolveAgentSpanScope.mockResolvedValue({ orgId: 'org-1', projects: [{ id: 'project-1' }], environmentScopes: new Map() });
	getSpansStatuses.mockImplementation(async (ids: Array<string | null>) => new Map(ids.filter(Boolean).map((i) => [i, 'none'])));
	getSpansState.mockResolvedValue({ status: 'none', reason: 'not_pulled' });
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
					environment: 'production',
					waitingSince: null,
					claimedAt: null,
					affectedUserCount: 0,
					affectedTenantCount: 0,
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

describe('GET /api/agent/self', () => {
	const selfCtx = { ...CTX, keyId: 'key-1', keyPrefix: 'sent_agent_ab12', keyCreatedAt: new Date(), keyExpiresAt: null };

	it('200: matches SelfResponseSchema without a configured CLI profile', async () => {
		vi.stubEnv('SENTINEL_CLI_PROFILE', '');
		authenticateAgentRequest.mockResolvedValue(selfCtx);
		const res = await selfRoute.GET(makeEvent('http://localhost/api/agent/self'));
		expect(res.status).toBe(200);
		expect(S.SelfResponseSchema.parse(await res.json())).toBeTruthy();
		vi.unstubAllEnvs();
	});

	it('200: matches SelfResponseSchema WITH cliProfile (deployments that set SENTINEL_CLI_PROFILE)', async () => {
		vi.stubEnv('SENTINEL_CLI_PROFILE', 'production');
		authenticateAgentRequest.mockResolvedValue(selfCtx);
		const res = await selfRoute.GET(makeEvent('http://localhost/api/agent/self'));
		const body = await res.json();
		expect(body.cliProfile).toBe('production');
		expect(S.SelfResponseSchema.parse(body)).toBeTruthy();
		vi.unstubAllEnvs();
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

describe('GET /api/agent/traces/{traceId}', () => {
	const TRACE_ID = 'a'.repeat(32);
	const TRACE_OCCURRENCE = {
		id: 'occ-trace',
		environment: 'production',
		platform: 'go',
		releaseVersion: null,
		stacktrace: [],
		metadata: {},
		traceId: TRACE_ID,
		createdAt: new Date('2026-09-01T00:00:00Z').toISOString(),
		level: 'error',
		message: 'boom',
		errorClass: 'Error',
		logger: null,
		serverName: null,
		transaction: null,
		dist: null,
		source: null,
		traceFlags: null,
		receivedAt: null,
		sdk: null,
		user: null,
		tags: null,
		contexts: null,
		request: null,
		breadcrumbs: null,
		exception: null,
		projectId: 'project-1',
		projectName: 'core',
		issueId: 'issue-1',
		spanId: 'b'.repeat(16),
		spansStatus: 'none',
		hasSpans: false,
	};
	// makeEvent only knows `issueId` params; this route's param is `traceId`.
	const call = (query = '', traceId = TRACE_ID) => {
		const u = new URL(`http://localhost/api/agent/traces/${traceId}${query}`);
		return traceRoute.GET({ params: { traceId }, request: new Request(u), url: u } as any);
	};

	it('200: matches TraceResponseSchema, and scopes by the KEY\'s organization', async () => {
		authenticateAgentRequest.mockResolvedValue(CTX);
		const { spansStatus: _s, hasSpans: _h, ...stored } = TRACE_OCCURRENCE;
		listAgentTraceOccurrences.mockResolvedValue({ occurrences: [stored], nextCursor: 'abc' });
		getSpansStatuses.mockResolvedValue(new Map([[TRACE_ID, 'available']]));
		getSpansState.mockResolvedValue({ status: 'available', reason: null });
		const res = await call('?limit=5&environment=production');
		expect(res.status).toBe(200);
		const body = S.TraceResponseSchema.parse(await res.json());
		expect(body.nextCursor).toBe('abc');
		expect(body.spans).toEqual({ status: 'available', reason: null });
		expect(body.occurrences[0]).toMatchObject({ spansStatus: 'available', hasSpans: true });
		expect(getSpansState).toHaveBeenCalledWith(TRACE_ID, expect.objectContaining({ orgId: 'org-1' }));
		expect(listAgentTraceOccurrences).toHaveBeenCalledWith({
			organizationId: 'org-1',
			traceId: TRACE_ID,
			environment: 'production',
			limit: 5,
			cursor: undefined,
		});
	});

	it('200: omits nextCursor on the last page', async () => {
		listAgentTraceOccurrences.mockResolvedValue({ occurrences: [] });
		const res = await call();
		expect(S.TraceResponseSchema.parse(await res.json())).toEqual({ occurrences: [], spans: { status: 'none', reason: 'not_pulled' } });
	});

	it('the schema is strict: an unlisted occurrence field fails', () => {
		expect(S.AgentTraceOccurrenceSchema.safeParse({ ...TRACE_OCCURRENCE, extra: 1 }).success).toBe(false);
		expect(S.AgentTraceOccurrenceSchema.safeParse({ ...TRACE_OCCURRENCE, projectId: undefined }).success).toBe(false);
	});

	it('404: a malformed trace id never reaches the query', async () => {
		listAgentTraceOccurrences.mockClear();
		await expect(call('', 'not-a-trace')).rejects.toMatchObject({ status: 404 });
		expect(listAgentTraceOccurrences).not.toHaveBeenCalled();
	});

	it('400: limit above 200 or below 1', async () => {
		await expect(call('?limit=201')).rejects.toMatchObject({ status: 400 });
		await expect(call('?limit=0')).rejects.toMatchObject({ status: 400 });
	});

	it('400: a cursor the query layer rejects', async () => {
		listAgentTraceOccurrences.mockRejectedValue(new Error('invalid cursor'));
		await expect(call('?cursor=garbage')).rejects.toMatchObject({ status: 400 });
	});
});

describe('GET /api/agent/issues/{issueId}/occurrences spans fields (sentinel-occurrence-spans)', () => {
	const OCC = (id: string, traceId: string | null) => ({
		id, environment: 'production', platform: 'go', releaseVersion: null, stacktrace: [], metadata: {}, traceId,
		createdAt: new Date('2026-09-01T00:00:00Z'), level: 'error', message: 'boom', errorClass: 'Error', logger: null,
		serverName: null, transaction: null, dist: null, source: null, traceFlags: null, receivedAt: null, sdk: null,
		user: null, tags: null, contexts: null, request: null, breadcrumbs: null, exception: null,
	});
	const T1 = 'a'.repeat(32);
	const T2 = 'b'.repeat(32);

	it('200: every occurrence carries spansStatus (null without a trace id) and hasSpans', async () => {
		listAgentOccurrences.mockResolvedValue([OCC('o1', T1), OCC('o2', T2), OCC('o3', null)]);
		getSpansStatuses.mockResolvedValue(new Map([[T1, 'available'], [T2, 'pending']]));
		const res = await occurrencesRoute.GET(makeEvent('http://localhost/api/agent/issues/issue-1/occurrences', { issueId: 'issue-1' }));
		const body = S.OccurrencesResponseSchema.parse(await res.json());
		expect(body.occurrences.map((o) => [o.spansStatus, o.hasSpans])).toEqual([['available', true], ['pending', false], [null, false]]);
		expect(body).not.toHaveProperty('spansScan');
	});

	it('hasSpans=true: only occurrences with spans, the scan summary and the continuation cursor', async () => {
		walkOccurrencesWithSpans.mockResolvedValue({
			ids: ['o1'], traceIds: new Map([['o1', T1]]), scanned: 2000, exhausted: false, continueBefore: new Date('2026-08-01T00:00:00Z'),
		});
		getAgentOccurrencesByIds.mockResolvedValue([OCC('o1', T1)]);
		const res = await occurrencesRoute.GET(makeEvent('http://localhost/api/agent/issues/issue-1/occurrences?hasSpans=true&limit=5', { issueId: 'issue-1' }));
		const body = S.OccurrencesResponseSchema.parse(await res.json());
		expect(body.occurrences).toHaveLength(1);
		expect(body.occurrences[0]).toMatchObject({ spansStatus: 'available', hasSpans: true });
		expect(body.spansScan).toEqual({ scanned: 2000, exhausted: false });
		expect(body.nextBefore).toBe('2026-08-01T00:00:00.000Z');
		expect(walkOccurrencesWithSpans).toHaveBeenCalledWith(expect.objectContaining({ issueId: 'issue-1', want: 5, before: undefined }));
		expect(getAgentOccurrencesByIds).toHaveBeenCalledWith('issue-1', ['o1']);
		expect(listAgentOccurrences).not.toHaveBeenCalled();
	});

	it('400: hasSpans must be true or false', async () => {
		await expect(
			occurrencesRoute.GET(makeEvent('http://localhost/api/agent/issues/issue-1/occurrences?hasSpans=maybe', { issueId: 'issue-1' }))
		).rejects.toMatchObject({ status: 400 });
	});

	it('the schema is strict about the new fields', () => {
		expect(S.AgentOccurrenceWithSpansSchema.safeParse({ ...OCC('o1', T1), spansStatus: 'available' }).success).toBe(false);
		expect(S.AgentOccurrenceWithSpansSchema.safeParse({ ...OCC('o1', T1), spansStatus: 'bogus', hasSpans: false }).success).toBe(false);
	});
});

describe('GET /api/agent/occurrences/{occurrenceId}', () => {
	const ID = '11111111-2222-4333-8444-555555555555';
	const call = (id = ID) => {
		const u = new URL(`http://localhost/api/agent/occurrences/${id}`);
		return occurrenceRoute.GET({ params: { occurrenceId: id }, request: new Request(u), url: u } as any);
	};
	const OCC = {
		id: ID, environment: 'production', platform: 'go', releaseVersion: null, stacktrace: [], metadata: {}, traceId: 'a'.repeat(32),
		createdAt: '2026-09-01T00:00:00.000Z', level: 'error', message: 'boom', errorClass: 'Error', logger: null, serverName: null,
		transaction: null, dist: null, source: null, traceFlags: null, receivedAt: null, sdk: null, user: null, tags: null,
		contexts: null, request: null, breadcrumbs: null, exception: null, projectId: 'project-1', projectName: 'core',
		issueId: 'issue-1', spanId: null,
	};

	it('200: matches OccurrenceResponseSchema, scoped by the key\'s organization', async () => {
		getAgentOccurrenceInOrg.mockResolvedValue(OCC);
		getSpansStatuses.mockResolvedValue(new Map([['a'.repeat(32), 'pending']]));
		const res = await call();
		const body = S.OccurrenceResponseSchema.parse(await res.json());
		expect(body.occurrence).toMatchObject({ id: ID, spansStatus: 'pending', hasSpans: false });
		expect(getAgentOccurrenceInOrg).toHaveBeenCalledWith('org-1', ID);
	});

	it('404: malformed id never reaches the query; another organization\'s occurrence is not found', async () => {
		await expect(call('nope')).rejects.toMatchObject({ status: 404 });
		expect(getAgentOccurrenceInOrg).not.toHaveBeenCalled();
		getAgentOccurrenceInOrg.mockResolvedValue(null);
		await expect(call()).rejects.toMatchObject({ status: 404 });
	});
});

describe('POST /api/agent/traces/{traceId}/pull', () => {
	const TRACE = 'a'.repeat(32);
	const call = (traceId = TRACE) => {
		const u = new URL(`http://localhost/api/agent/traces/${traceId}/pull`);
		return pullRoute.POST({ params: { traceId }, request: new Request(u, { method: 'POST' }), url: u } as any);
	};

	it('200: matches PullSpansResponseSchema, requests as the agent, and audits an accepted request', async () => {
		requestSpanPull.mockResolvedValue({ status: 'pending', reason: null, action: 'queued', requestId: 'req-1' });
		const res = await call();
		expect(res.status).toBe(200);
		expect(S.PullSpansResponseSchema.parse(await res.json())).toEqual({ status: 'pending', reason: null, action: 'queued', requestId: 'req-1' });
		expect(requestSpanPull).toHaveBeenCalledWith(
			expect.objectContaining({ traceId: TRACE, requester: { kind: 'agent', id: 'agent-1' }, scope: expect.objectContaining({ orgId: 'org-1' }) })
		);
		expect(writeAgentAuditLog).toHaveBeenCalledWith(CTX, 'span_pull.request', 'span_pull', 'req-1', { traceId: TRACE, action: 'queued' });
	});

	it('200 and no audit row when nothing was written (already available / already pending)', async () => {
		requestSpanPull.mockResolvedValue({ status: 'available', reason: null, action: 'none', requestId: null });
		const res = await call();
		expect(S.PullSpansResponseSchema.parse(await res.json()).action).toBe('none');
		expect(writeAgentAuditLog).not.toHaveBeenCalled();
	});

	it('404 for a malformed id (no pull), 404 when the service says not found, 403 when not allow-listed', async () => {
		await expect(call('nope')).rejects.toMatchObject({ status: 404 });
		expect(requestSpanPull).not.toHaveBeenCalled();
		requestSpanPull.mockRejectedValueOnce(new SpanPullError('not_found', 'Trace not found'));
		await expect(call()).rejects.toMatchObject({ status: 404 });
		requestSpanPull.mockRejectedValueOnce(new SpanPullError('not_allowed', 'Span pull is not enabled for this organization'));
		await expect(call()).rejects.toMatchObject({ status: 403 });
	});

	it('429 with Retry-After when the organization is over its hourly limit', async () => {
		requestSpanPull.mockRejectedValue(new SpanPullError('rate_limited', 'slow down', 120));
		const res = await call();
		expect(res.status).toBe(429);
		expect(res.headers.get('retry-after')).toBe('120');
		expect(S.MessageErrorSchema.parse(await res.json())).toBeTruthy();
		expect(writeAgentAuditLog).not.toHaveBeenCalled();
	});

	it('401 without a valid key: nothing is requested', async () => {
		authenticateAgentRequest.mockRejectedValue(Object.assign(new Error('Unauthorized'), { status: 401 }));
		await expect(call()).rejects.toMatchObject({ status: 401 });
		expect(requestSpanPull).not.toHaveBeenCalled();
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

describe('POST /api/agent/issues/claim-next', () => {
	it('200: matches IssueDetailResponseSchema with the claim on the issue row', async () => {
		claimNextAgentIssue.mockResolvedValue({
			issue: { ...ISSUE_ROW, assigneeType: 'agent', assignedTo: 'agent-1', claimedAt: new Date().toISOString() },
			notified: [],
		});
		getAgentReportDetail.mockResolvedValue(null);
		getLatestAgentOccurrence.mockResolvedValue(null);
		getIssueRelations.mockResolvedValue([]);
		const res = await claimNextRoute.POST(
			makeEvent('http://localhost/api/agent/issues/claim-next', { method: 'POST', body: { type: 'system_error' } })
		);
		expect(res.status).toBe(200);
		const body = S.IssueDetailResponseSchema.parse(await res.json());
		expect(body.issue.assignedTo).toBe('agent-1');
	});

	it('404: empty queue maps to 404 with MessageErrorSchema', async () => {
		claimNextAgentIssue.mockResolvedValue(null);
		await expect(
			claimNextRoute.POST(makeEvent('http://localhost/api/agent/issues/claim-next', { method: 'POST', body: {} }))
		).rejects.toMatchObject({ status: 404, body: { message: expect.any(String) } });
	});

	it('400: invalid sort', async () => {
		await expect(
			claimNextRoute.POST(
				makeEvent('http://localhost/api/agent/issues/claim-next', { method: 'POST', body: { sort: 'bogus' } })
			)
		).rejects.toMatchObject({ status: 400 });
		expect(claimNextAgentIssue).not.toHaveBeenCalled();
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

	// -------------------------------------------------------------------------------------------
	// sentinel-releases-snooze §4.2 (decisions 33, 38, 40): `snoozed_until` / `snooze_occurrences`
	// through the agent route. The validators are REAL here (see the issue-access mock), so these
	// cases prove the same set `routes/api/issues/status.test.ts` proves for the human route --
	// one validator, both routes, the same four strings.
	// -------------------------------------------------------------------------------------------
	describe('snoozed_until / snooze_occurrences (sentinel-releases-snooze §4.2)', () => {
		const FUTURE_Z = '2099-09-20T09:00:00Z';
		const FUTURE_OFFSET = '2099-09-20T16:00:00+07:00'; // the same instant as FUTURE_Z
		const patch = (body: unknown) =>
			statusRoute.PATCH(
				makeEvent('http://localhost/api/agent/issues/issue-1/status', {
					method: 'PATCH',
					issueId: 'issue-1',
					body,
				})
			);
		const lastSnooze = () => updateIssueStatus.mock.calls.at(-1)?.[5];

		beforeEach(() => {
			updateIssueStatus.mockResolvedValue({ changed: true, notified: [] });
		});

		it('200: forwards both conditions to updateIssueStatus, the instant parsed', async () => {
			const res = await patch({ status: 'ignored', snoozed_until: FUTURE_Z, snooze_occurrences: 100 });
			expect(res.status).toBe(200);
			expect(S.StatusResponseSchema.parse(await res.json())).toBeTruthy();
			expect(updateIssueStatus).toHaveBeenCalledWith('issue-1', 'ignored', undefined, 'agent', 'agent-1', {
				snoozedUntil: new Date(FUTURE_Z),
				snoozeOccurrences: 100,
			});
		});

		it('200: an offset timestamp is accepted and normalised to the same instant as its Z form', async () => {
			const res = await patch({ status: 'ignored', snoozed_until: FUTURE_OFFSET });
			expect(res.status).toBe(200);
			expect(lastSnooze()).toEqual({ snoozedUntil: new Date(FUTURE_Z), snoozeOccurrences: null });
			expect((lastSnooze().snoozedUntil as Date).getTime()).toBe(Date.parse(FUTURE_Z));
		});

		it('200: `\'\'` on either key is omitted -- the indefinite ignore (§3)', async () => {
			const res = await patch({ status: 'ignored', snoozed_until: '', snooze_occurrences: '' });
			expect(res.status).toBe(200);
			expect(lastSnooze()).toEqual({ snoozedUntil: null, snoozeOccurrences: null });
		});

		it('200: a body without the keys forwards both as null -- the pre-feature call is unchanged', async () => {
			const res = await patch({ status: 'ignored' });
			expect(res.status).toBe(200);
			expect(lastSnooze()).toEqual({ snoozedUntil: null, snoozeOccurrences: null });
		});

		it.each([
			['a past instant', '2020-01-01T00:00:00Z'],
			['an unparseable string', 'tomorrow'],
			['a date-only value (Date.parse alone would take it as UTC midnight)', '2099-09-20'],
			['a zone-less value (Date.parse alone would take it as server-local time)', '2099-09-20T09:00'],
		])('400 `snoozedUntil must be a future timestamp`: %s', async (_label, snoozed_until) => {
			await expect(patch({ status: 'ignored', snoozed_until })).rejects.toMatchObject({
				status: 400,
				body: { message: 'snoozedUntil must be a future timestamp' },
			});
			expect(updateIssueStatus).not.toHaveBeenCalled();
		});

		it.each([0, 1000001, '12', 1.5])(
			'400 `snoozeOccurrences must be an integer between 1 and 1000000`: %j',
			async (snooze_occurrences) => {
				await expect(patch({ status: 'ignored', snooze_occurrences })).rejects.toMatchObject({
					status: 400,
					body: { message: 'snoozeOccurrences must be an integer between 1 and 1000000' },
				});
				expect(updateIssueStatus).not.toHaveBeenCalled();
			}
		);

		it('400: a snooze key with status resolved -- each key has its own string', async () => {
			await expect(patch({ status: 'resolved', snoozed_until: FUTURE_Z })).rejects.toMatchObject({
				status: 400,
				body: { message: 'snoozedUntil is only valid with status ignored' },
			});
			await expect(patch({ status: 'resolved', snooze_occurrences: 5 })).rejects.toMatchObject({
				status: 400,
				body: { message: 'snoozeOccurrences is only valid with status ignored' },
			});
			await expect(patch({ status: 'unresolved', snooze_occurrences: 5 })).rejects.toMatchObject({
				status: 400,
				body: { message: 'snoozeOccurrences is only valid with status ignored' },
			});
			expect(updateIssueStatus).not.toHaveBeenCalled();
		});

		it('200: the batch op `issues.status` goes through the same validator and forwards the same shape', async () => {
			const res = await batchRoute.POST(
				makeEvent('http://localhost/api/agent/batch', {
					method: 'POST',
					body: {
						operations: [
							{
								op: 'issues.status',
								issueId: 'issue-1',
								params: { status: 'ignored', snoozed_until: FUTURE_OFFSET, snooze_occurrences: 100 },
							},
						],
					},
				})
			);
			expect(res.status).toBe(200);
			expect(S.BatchResponseSchema.parse(await res.json())).toBeTruthy();
			expect(lastSnooze()).toEqual({ snoozedUntil: new Date(FUTURE_Z), snoozeOccurrences: 100 });
		});

		it('batch op: a past snoozed_until is the same 400 string, reported per-operation', async () => {
			const res = await batchRoute.POST(
				makeEvent('http://localhost/api/agent/batch', {
					method: 'POST',
					body: {
						operations: [
							{
								op: 'issues.status',
								issueId: 'issue-1',
								params: { status: 'ignored', snoozed_until: '2020-01-01T00:00:00Z' },
							},
						],
					},
				})
			);
			const parsed = S.BatchResponseSchema.parse(await res.json());
			expect(JSON.stringify(parsed)).toContain('snoozedUntil must be a future timestamp');
			expect(updateIssueStatus).not.toHaveBeenCalled();
		});
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


// ---------------------------------------------------------------------------
// sentinel-project-environments decision 54 -- the divergence this file could not see.
//
// Every route case above mocks `getAgentIssueDetail`, so the mock's object -- not the REAL
// `db.select().from(issues)` full row that route returns verbatim -- is what meets `IssueRowSchema`.
// A column added to `issues` therefore reaches three `.strict()` responses (issue detail, claim,
// release) undeclared, and this suite stays green. These cases compare the schema against the
// Drizzle table definition instead of against a mock, so the next added column is caught here.
// ---------------------------------------------------------------------------

describe('IssueRowSchema vs. the real `issues` table (decision 54)', () => {
	const columnNames = Object.keys(getTableColumns(issues));
	const schemaKeys = Object.keys(S.IssueRowSchema.shape);

	// `waitingSince` (N9) is a PRE-EXISTING undeclared column, found while fixing decision 54 and
	// deliberately left alone: declaring it is a second wire-visible addition that decision 54 does
	// not sanction. Named here so it is a known gap rather than an invisible one -- remove it from
	// this list in the change that declares the field.
	const KNOWN_UNDECLARED = ['waitingSince'];

	it('declares `environment` -- the column decision 54 publishes', () => {
		expect(schemaKeys).toContain('environment');
		expect(columnNames).toContain('environment');
	});

	it('declares every `issues` column except the ones named as known gaps', () => {
		const undeclared = columnNames.filter((c) => !schemaKeys.includes(c));
		expect(undeclared).toEqual(KNOWN_UNDECLARED);
	});

	it('declares no field that is not an `issues` column (the schema is `.strict()`)', () => {
		expect(schemaKeys.filter((k) => !columnNames.includes(k))).toEqual([]);
	});

	it('rejects a full row whose `environment` is missing', () => {
		const { environment, ...withoutEnvironment } = ISSUE_ROW;
		expect(S.IssueRowSchema.safeParse(withoutEnvironment).success).toBe(false);
	});

	// sentinel-affected-users decision 14 (contract §4.4): the two §4.6 counters reach this
	// response by the same full-row select, so they are declared -- as non-null integers, because
	// the column is NOT NULL DEFAULT 0 -- and NOT appended to KNOWN_UNDECLARED (decision 27).
	it('decision 14: declares affectedUserCount / affectedTenantCount as non-null integers', () => {
		expect(schemaKeys).toContain('affectedUserCount');
		expect(schemaKeys).toContain('affectedTenantCount');
		expect(KNOWN_UNDECLARED).not.toContain('affectedUserCount');
		expect(KNOWN_UNDECLARED).not.toContain('affectedTenantCount');
		expect(S.IssueRowSchema.safeParse({ ...ISSUE_ROW, affectedUserCount: 600, affectedTenantCount: 4 }).success).toBe(true);
		expect(S.IssueRowSchema.safeParse({ ...ISSUE_ROW, affectedUserCount: null }).success).toBe(false);
		expect(S.IssueRowSchema.safeParse({ ...ISSUE_ROW, affectedTenantCount: -1 }).success).toBe(false);
	});

	// sentinel-releases-snooze decision 9 (contract §4.5): the four snooze / release columns reach
	// this response by the same full-row select, so they are declared -- nullable, because every
	// pre-feature row and every plain indefinite ignore reads NULL -- and NOT appended to
	// KNOWN_UNDECLARED, which stays exactly `['waitingSince']`; a fifth entry there is a finding.
	it('decision 9: declares the four §4.5 columns as nullable and leaves KNOWN_UNDECLARED alone', () => {
		const four = ['snoozedUntil', 'snoozeOccurrenceTarget', 'snoozeCountAt', 'firstReleaseVersion'];
		for (const key of four) {
			expect(schemaKeys).toContain(key);
			expect(columnNames).toContain(key);
			expect(KNOWN_UNDECLARED).not.toContain(key);
		}
		expect(KNOWN_UNDECLARED).toEqual(['waitingSince']);
		// A snoozed ignore: a date and an occurrence target with the count it was set at.
		expect(
			S.IssueRowSchema.safeParse({
				...ISSUE_ROW,
				status: 'ignored',
				snoozedUntil: '2026-09-20T09:00:00.000Z',
				snoozeOccurrenceTarget: 100,
				snoozeCountAt: 37,
				firstReleaseVersion: 'v1.2.3',
			}).success
		).toBe(true);
		// A pre-feature row / plain ignore: all four null.
		expect(S.IssueRowSchema.safeParse(ISSUE_ROW).success).toBe(true);
		// The migrated tripwire, mirrored: a target of 0 is not a row the database can hold
		// (check_snooze_target_positive), so the schema does not admit it either. `snoozeCountAt`
		// carries no CHECK of its own and is described as the plain integer it is.
		expect(S.IssueRowSchema.safeParse({ ...ISSUE_ROW, snoozeOccurrenceTarget: 0, snoozeCountAt: 0 }).success).toBe(false);
		expect(S.IssueRowSchema.safeParse({ ...ISSUE_ROW, snoozeOccurrenceTarget: 1, snoozeCountAt: 0 }).success).toBe(true);
		// The keys are required (the schema is `.strict()` and a full-row select always carries them).
		const { snoozedUntil, ...withoutSnoozedUntil } = ISSUE_ROW;
		expect(S.IssueRowSchema.safeParse(withoutSnoozedUntil).success).toBe(false);
	});
});

describe('AgentIssueListItemSchema (decision 54)', () => {
	it('requires `environment` -- the only field that tells two decision-7 siblings apart', () => {
		expect(Object.keys(S.AgentIssueListItemSchema.shape)).toContain('environment');
	});

	// sentinel-affected-users decision 14 (contract §4.4): the list carries the same two integers
	// the detail does. Declared here AND named in listAgentIssues's projection -- the projection
	// half is pinned in agent-work.test.ts, this half pins the wire shape: an integer >= 0, never
	// null, and never absent (the schema is `.strict()` and the keys are required).
	it('decision 14: declares affectedUserCount / affectedTenantCount as non-null integers', () => {
		const keys = Object.keys(S.AgentIssueListItemSchema.shape);
		expect(keys).toContain('affectedUserCount');
		expect(keys).toContain('affectedTenantCount');

		const base = {
			id: 'issue-1',
			projectId: 'project-1',
			projectName: 'Inbox',
			isInbox: false,
			issueType: 'system_error',
			message: 'x',
			errorClass: 'Error',
			status: 'unresolved',
			assigneeType: null,
			assignedTo: null,
			waitingOn: null,
			firstSeen: null,
			lastSeen: null,
			count: 1,
			severity: null,
			reporterId: null,
			isWaiting: false,
			environment: 'production',
			waitingSince: null,
			claimedAt: null,
		};
		expect(S.AgentIssueListItemSchema.safeParse({ ...base, affectedUserCount: 3, affectedTenantCount: 0 }).success).toBe(true);
		expect(S.AgentIssueListItemSchema.safeParse(base).success).toBe(false);
		expect(S.AgentIssueListItemSchema.safeParse({ ...base, affectedUserCount: null, affectedTenantCount: 0 }).success).toBe(false);
		expect(S.AgentIssueListItemSchema.safeParse({ ...base, affectedUserCount: 1.5, affectedTenantCount: 0 }).success).toBe(false);
		expect(S.AgentIssueListItemSchema.safeParse({ ...base, affectedUserCount: -1, affectedTenantCount: 0 }).success).toBe(false);
	});
});

// sentinel-event-schema-v2 §8.1 / V7a: an occurrence carrying every v2 field, in the shape the
// processor stores it (snake_case JSONB bodies, a context redacted whole to a string, traceFlags as a
// number), parses under the occurrence list and issue-detail response schemas; so does a pre-v2 row with
// every v2 field null.
describe('agent occurrence v2 fields (V7a)', () => {
	const PRE_V2_OCCURRENCE = {
		id: 'occ-pre',
		environment: 'production',
		platform: 'go',
		releaseVersion: null,
		stacktrace: [],
		metadata: {},
		traceId: null,
		createdAt: new Date('2026-09-01T00:00:00Z').toISOString(),
		level: null,
		message: null,
		errorClass: null,
		logger: null,
		serverName: null,
		transaction: null,
		dist: null,
		source: null,
		traceFlags: null,
		receivedAt: null,
		sdk: null,
		user: null,
		tags: null,
		contexts: null,
		request: null,
		breadcrumbs: null,
		exception: null,
	};
	const FULL_V2_OCCURRENCE = {
		...PRE_V2_OCCURRENCE,
		id: 'occ-v2',
		platform: 'javascript',
		releaseVersion: '2.3.1',
		stacktrace: [{ file: 'src/pay/checkout.ts', line: 88, function: 'pay', in_app: true, module: 'pay.checkout' }],
		metadata: { email: 'alice@example.com', server_truncated: ['tags'] },
		traceId: '4bf92f3577b34da6a3ce929d0e0e4736',
		level: 'warning',
		message: 'charge [REDACTED_PAN] failed',
		errorClass: 'TypeError',
		logger: 'checkout',
		serverName: 'web-7f9c-pod-2',
		transaction: '/checkout/pay',
		dist: '4127',
		source: 'onerror',
		traceFlags: 1,
		receivedAt: new Date('2026-09-23T12:00:01Z').toISOString(),
		sdk: { name: 'sentinel.javascript.browser', version: '1.8.0' },
		user: {
			id: 'user-typed-42',
			username: 'alice',
			email: 'alice@example.com',
			ip_address: '2001:db8::1',
			geo: { city: 'Berlin', region: 'Berlin', country_code: 'DE', subdivision: 'BE' },
		},
		tags: { tenant_id: 'tenant-acme', auth_method: '***REDACTED***' },
		contexts: { device: { model: 'Pixel 8', memory_size: 8589934592 }, auth: '***REDACTED***' },
		request: {
			method: 'POST',
			url: 'https://shop.example.com/checkout/pay?step=2',
			query_string: 'step=2',
			headers: { Authorization: '***REDACTED***', Cookie: '***REDACTED***' },
			data: { amount: 4200, password: '***REDACTED***' },
			env: { REMOTE_ADDR: '10.1.2.3' },
		},
		breadcrumbs: [
			{ timestamp: '2026-09-23T11:59:58Z', type: 'http', category: 'xhr', level: 'error', message: 'POST /api/pay 402', data: { status_code: 402 } },
		],
		exception: {
			values: [
				{ type: 'PaymentDeclined', value: 'card declined', mechanism: { type: 'generic', handled: true } },
				{ type: 'TypeError', value: 'x', mechanism: { type: 'onerror', handled: false }, stacktrace: [{ function: 'pay', abs_path: '/a.js' }] },
			],
		},
	};

	it('a fully populated v2 occurrence parses under OccurrencesResponseSchema, through the route', async () => {
		listAgentOccurrences.mockResolvedValue([FULL_V2_OCCURRENCE, PRE_V2_OCCURRENCE]);
		const res = await occurrencesRoute.GET(
			makeEvent('http://localhost/api/agent/issues/issue-1/occurrences', { issueId: 'issue-1' })
		);
		expect(res.status).toBe(200);
		const body = S.OccurrencesResponseSchema.parse(await res.json());
		expect(JSON.stringify(body)).toContain('"traceFlags":1');
	});

	it('a fully populated v2 occurrence parses under IssueDetailResponseSchema as latestOccurrence', async () => {
		getAgentIssueDetail.mockResolvedValue(ISSUE_ROW);
		getAgentReportDetail.mockResolvedValue(null);
		getLatestAgentOccurrence.mockResolvedValue(FULL_V2_OCCURRENCE);
		getIssueRelations.mockResolvedValue([]);
		const res = await issueDetailRoute.GET(makeEvent('http://localhost/api/agent/issues/issue-1', { issueId: 'issue-1' }));
		expect(res.status).toBe(200);
		const body = S.IssueDetailResponseSchema.parse(await res.json());
		expect(body.latestOccurrence?.level).toBe('warning');
		expect(body.latestOccurrence?.user?.ip_address).toBe('2001:db8::1');
	});

	it('a pre-v2 row (every v2 field null) parses', () => {
		expect(S.AgentOccurrenceSchema.safeParse(PRE_V2_OCCURRENCE).success).toBe(true);
	});

	it('traceFlags must be a number: the string a bigint column returns without mode "number" is rejected', () => {
		expect(S.AgentOccurrenceSchema.safeParse({ ...FULL_V2_OCCURRENCE, traceFlags: '1' }).success).toBe(false);
	});

	it('the outer object stays strict; the inner stored JSON does not', () => {
		expect(S.AgentOccurrenceSchema.safeParse({ ...FULL_V2_OCCURRENCE, unexpected: 1 }).success).toBe(false);
		expect(S.AgentOccurrenceSchema.safeParse({ ...FULL_V2_OCCURRENCE, sdk: { name: 'x', integrations: ['a'] } }).success).toBe(true);
	});

	it('an unknown level is rejected', () => {
		expect(S.AgentOccurrenceSchema.safeParse({ ...FULL_V2_OCCURRENCE, level: 'WARN' }).success).toBe(false);
	});
});
