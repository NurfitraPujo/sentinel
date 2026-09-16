import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * sentinel-agent-claim-next-issue: POST /api/agent/issues/claim-next. Mirrors agent-issue-detail
 * .test.ts's pattern -- auth, audit, notify and every query module are mocked, the route body
 * runs for real. The atomic select+claim itself is proven against a real Postgres in
 * src/lib/db/queries/agent-work.claim-next.integration.test.ts; this file covers the HTTP
 * contract around it: body validation, credential-derived scope (B7), the 404 for an empty
 * queue, the detail-shaped 200, and the audit/notification side effects.
 */

const claimNextAgentIssue = vi.fn();
vi.mock('$lib/db/queries/agent-work', () => ({
	claimNextAgentIssue,
	AGENT_ISSUE_SOURCES: ['ingestion_sdk', 'manual_support', 'api', 'user_report_api'],
}));

const getAgentReportDetail = vi.fn();
const getLatestAgentOccurrence = vi.fn();
vi.mock('$lib/db/queries/agent-reads', () => ({ getAgentReportDetail, getLatestAgentOccurrence }));

const getIssueRelations = vi.fn();
vi.mock('$lib/db/queries/issues', () => ({ getIssueRelations }));

const authenticateAgentRequest = vi.fn();
vi.mock('$lib/server/agent-auth', () => ({ authenticateAgentRequest }));

const writeAgentAuditLog = vi.fn();
vi.mock('$lib/server/agent-audit', () => ({ writeAgentAuditLog }));

const sendIssueNotificationEmails = vi.fn();
vi.mock('$lib/server/notify', () => ({ sendIssueNotificationEmails }));

const { POST } = await import('./+server');

const CTX = { agentId: 'agent-1', organizationId: 'org-1', agentName: 'bot', keyPrefixForAudit: 'abc' };

function makeEvent(body?: unknown, raw?: string) {
	const url = new URL('http://localhost/api/agent/issues/claim-next');
	const init: RequestInit = { method: 'POST' };
	if (raw !== undefined) init.body = raw;
	else if (body !== undefined) init.body = JSON.stringify(body);
	return { params: {}, request: new Request(url, init), url } as any;
}

function claimedIssue(overrides: Record<string, unknown> = {}) {
	return {
		id: 'issue-1',
		issueType: 'system_error',
		assigneeType: 'agent',
		assignedTo: 'agent-1',
		claimedAt: new Date('2026-09-16T00:00:00Z'),
		...overrides,
	};
}

beforeEach(() => {
	vi.clearAllMocks();
	authenticateAgentRequest.mockResolvedValue(CTX);
	getIssueRelations.mockResolvedValue([]);
	getAgentReportDetail.mockResolvedValue(null);
	getLatestAgentOccurrence.mockResolvedValue(null);
	sendIssueNotificationEmails.mockResolvedValue(undefined);
	writeAgentAuditLog.mockResolvedValue(undefined);
});

describe('POST /api/agent/issues/claim-next -- auth and scope', () => {
	it('401s when unauthenticated and claims nothing', async () => {
		authenticateAgentRequest.mockRejectedValue(Object.assign(new Error('Unauthorized'), { status: 401 }));
		await expect(POST(makeEvent({}))).rejects.toMatchObject({ status: 401 });
		expect(claimNextAgentIssue).not.toHaveBeenCalled();
	});

	it('derives organizationId and agentId from the credential, never the body (B7)', async () => {
		claimNextAgentIssue.mockResolvedValue({ issue: claimedIssue(), notified: [] });
		await POST(makeEvent({ organizationId: 'org-evil', agentId: 'agent-evil' }));
		expect(claimNextAgentIssue).toHaveBeenCalledTimes(1);
		expect(claimNextAgentIssue).toHaveBeenCalledWith(
			expect.objectContaining({ organizationId: 'org-1', agentId: 'agent-1' })
		);
	});
});

describe('POST /api/agent/issues/claim-next -- body validation', () => {
	it('accepts an absent body and forwards every filter as undefined', async () => {
		claimNextAgentIssue.mockResolvedValue({ issue: claimedIssue(), notified: [] });
		const res = await POST(makeEvent());
		expect(res.status).toBe(200);
		expect(claimNextAgentIssue).toHaveBeenCalledWith({
			organizationId: 'org-1',
			agentId: 'agent-1',
			type: undefined,
			projectId: undefined,
			environment: undefined,
			source: undefined,
			sort: undefined,
		});
	});

	it('treats a malformed JSON body as empty rather than 500ing', async () => {
		claimNextAgentIssue.mockResolvedValue({ issue: claimedIssue(), notified: [] });
		const res = await POST(makeEvent(undefined, '{not json'));
		expect(res.status).toBe(200);
	});

	it('forwards every recognised filter', async () => {
		claimNextAgentIssue.mockResolvedValue({ issue: claimedIssue(), notified: [] });
		await POST(
			makeEvent({ type: 'user_report', project: 'p-1', environment: 'staging', source: 'api', sort: 'firstSeen' })
		);
		expect(claimNextAgentIssue).toHaveBeenCalledWith({
			organizationId: 'org-1',
			agentId: 'agent-1',
			type: 'user_report',
			projectId: 'p-1',
			environment: 'staging',
			source: 'api',
			sort: 'firstSeen',
		});
	});

	it('treats type "any" as no type filter (list-route parity)', async () => {
		claimNextAgentIssue.mockResolvedValue({ issue: claimedIssue(), notified: [] });
		await POST(makeEvent({ type: 'any' }));
		expect(claimNextAgentIssue).toHaveBeenCalledWith(expect.objectContaining({ type: undefined }));
	});

	it('400s on an invalid type, source or sort, and on a non-string filter, without claiming', async () => {
		const bad: Array<[Record<string, unknown>, string]> = [
			[{ type: 'bogus' }, 'type must be one of: any, user_report, system_error'],
			[{ source: 'bogus' }, 'source must be one of: ingestion_sdk, manual_support, api, user_report_api'],
			[{ sort: 'bogus' }, 'sort must be one of: firstSeen, lastSeen'],
			[{ project: 42 }, 'project must be a string'],
		];
		for (const [body, message] of bad) {
			claimNextAgentIssue.mockClear();
			await expect(POST(makeEvent(body))).rejects.toMatchObject({ status: 400, body: { message } });
			expect(claimNextAgentIssue).not.toHaveBeenCalled();
		}
	});
});

describe('POST /api/agent/issues/claim-next -- outcomes', () => {
	it('404s when the queue is empty, with no audit row and no email', async () => {
		claimNextAgentIssue.mockResolvedValue(null);
		await expect(POST(makeEvent({}))).rejects.toMatchObject({
			status: 404,
			body: { message: 'No unclaimed issue matches' },
		});
		expect(writeAgentAuditLog).not.toHaveBeenCalled();
		expect(sendIssueNotificationEmails).not.toHaveBeenCalled();
	});

	it('200s with the detail shape for a system_error issue (latest occurrence, null report)', async () => {
		claimNextAgentIssue.mockResolvedValue({ issue: claimedIssue(), notified: [] });
		getLatestAgentOccurrence.mockResolvedValue({ id: 'occ-1' });
		getIssueRelations.mockResolvedValue([{ id: 'rel-1' }]);

		const res = await POST(makeEvent({}));
		expect(res.status).toBe(200);
		const body = await res.json();
		expect(body.issue).toMatchObject({ id: 'issue-1', assignedTo: 'agent-1', assigneeType: 'agent' });
		expect(body.report).toBeNull();
		expect(body.latestOccurrence).toEqual({ id: 'occ-1' });
		expect(body.relations).toEqual([{ id: 'rel-1' }]);
		expect(getLatestAgentOccurrence).toHaveBeenCalledWith('issue-1');
		expect(getAgentReportDetail).not.toHaveBeenCalled();
		expect(getIssueRelations).toHaveBeenCalledWith('issue-1');
	});

	it('200s with the report companion for a user_report issue (null occurrence)', async () => {
		claimNextAgentIssue.mockResolvedValue({ issue: claimedIssue({ id: 'issue-2', issueType: 'user_report' }), notified: [] });
		getAgentReportDetail.mockResolvedValue({ bodyMd: 'body', severity: 'high' });

		const res = await POST(makeEvent({}));
		const body = await res.json();
		expect(body.report).toEqual({ bodyMd: 'body', severity: 'high' });
		expect(body.latestOccurrence).toBeNull();
		expect(getAgentReportDetail).toHaveBeenCalledWith('issue-2');
		expect(getLatestAgentOccurrence).not.toHaveBeenCalled();
	});

	it('sends the claim notification and writes an agent.issue.claimed audit row tagged claimNext', async () => {
		const notified = [{ userId: 'u-1' }];
		claimNextAgentIssue.mockResolvedValue({ issue: claimedIssue(), notified });

		await POST(makeEvent({ type: 'system_error' }));

		expect(sendIssueNotificationEmails).toHaveBeenCalledWith(notified, {
			issueId: 'issue-1',
			origin: 'http://localhost',
		});
		expect(writeAgentAuditLog).toHaveBeenCalledTimes(1);
		expect(writeAgentAuditLog).toHaveBeenCalledWith(
			CTX,
			'agent.issue.claimed',
			'issue',
			'issue-1',
			expect.objectContaining({ claimNext: true, filters: expect.objectContaining({ type: 'system_error' }) })
		);
	});
});
