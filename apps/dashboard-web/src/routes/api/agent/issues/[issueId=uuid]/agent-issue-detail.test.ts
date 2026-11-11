import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * N1c (agent read endpoints). GET /api/agent/issues/[issueId]. Mirrors claim.test.ts's pattern
 * of mocking the shared wrapper (`withAgentIssue`) directly rather than re-testing its auth/scope
 * behavior here -- that belongs to agent-route.ts's own responsibility. This file tests the
 * handler `withAgentIssue` wraps: user_report vs system_error branching and response shape.
 */

const getAgentIssueDetail = vi.fn();
const getAgentReportDetail = vi.fn();
const getLatestAgentOccurrence = vi.fn();
const getIssueRelations = vi.fn();

vi.mock('$lib/db/queries/agent-reads', () => ({
	getAgentIssueDetail,
	getAgentReportDetail,
	getLatestAgentOccurrence,
}));
vi.mock('$lib/db/queries/issues', () => ({ getIssueRelations }));

// withAgentIssue itself is exercised for real -- only its dependencies (auth + scope resolution)
// are mocked -- so this also proves the 401/404 contract this route inherits from it.
const authenticateAgentRequest = vi.fn();
const resolveAgentIssueScope = vi.fn();
const writeAgentAuditLog = vi.fn();

vi.mock('$lib/server/agent-auth', () => ({ authenticateAgentRequest }));
vi.mock('$lib/server/agent-issue-scope', () => ({ resolveAgentIssueScope }));
vi.mock('$lib/server/agent-audit', () => ({ writeAgentAuditLog }));

const { GET } = await import('./+server');

function makeEvent(issueId = 'issue-1') {
	return {
		params: { issueId },
		request: new Request(`http://localhost/api/agent/issues/${issueId}`),
		url: new URL(`http://localhost/api/agent/issues/${issueId}`),
	} as any;
}

beforeEach(() => {
	vi.clearAllMocks();
	authenticateAgentRequest.mockResolvedValue({
		agentId: 'agent-1',
		organizationId: 'org-1',
		agentName: 'bot',
		keyPrefixForAudit: 'abc',
	});
	getIssueRelations.mockResolvedValue([]);
	getAgentReportDetail.mockResolvedValue(null);
	getLatestAgentOccurrence.mockResolvedValue(null);
});

describe('GET /api/agent/issues/[issueId]', () => {
	it('401s when unauthenticated (delegated to withAgentIssue)', async () => {
		authenticateAgentRequest.mockRejectedValue(Object.assign(new Error('Unauthorized'), { status: 401 }));

		await expect(GET(makeEvent())).rejects.toMatchObject({ status: 401 });
		expect(getAgentIssueDetail).not.toHaveBeenCalled();
	});

	it('404s for a cross-org issue (delegated to withAgentIssue -> resolveAgentIssueScope)', async () => {
		resolveAgentIssueScope.mockRejectedValue(Object.assign(new Error('Issue not found'), { status: 404 }));

		await expect(GET(makeEvent())).rejects.toMatchObject({ status: 404 });
		expect(getAgentIssueDetail).not.toHaveBeenCalled();
	});

	it('returns report detail and null occurrence for a user_report issue', async () => {
		resolveAgentIssueScope.mockResolvedValue({
			issueId: 'issue-1',
			projectId: 'project-1',
			organizationId: 'org-1',
			issueType: 'user_report',
			assignedTo: null,
			assigneeType: null,
			waitingOn: null,
		});
		getAgentIssueDetail.mockResolvedValue({ id: 'issue-1', issueType: 'user_report' });
		getAgentReportDetail.mockResolvedValue({ bodyMd: 'body', severity: 'high', reporterId: 'user-1' });

		const res = await GET(makeEvent());
		expect(res.status).toBe(200);
		const body = await res.json();

		expect(body.issue).toEqual({ id: 'issue-1', issueType: 'user_report' });
		expect(body.report).toEqual({ bodyMd: 'body', severity: 'high', reporterId: 'user-1' });
		expect(body.latestOccurrence).toBeNull();
		expect(getAgentReportDetail).toHaveBeenCalledWith('issue-1');
		expect(getLatestAgentOccurrence).not.toHaveBeenCalled();
	});

	it('returns latest occurrence and null report for a system_error issue', async () => {
		resolveAgentIssueScope.mockResolvedValue({
			issueId: 'issue-2',
			projectId: 'project-1',
			organizationId: 'org-1',
			issueType: 'system_error',
			assignedTo: null,
			assigneeType: null,
			waitingOn: null,
		});
		getAgentIssueDetail.mockResolvedValue({ id: 'issue-2', issueType: 'system_error' });
		getLatestAgentOccurrence.mockResolvedValue({
			id: 'occ-1',
			environment: 'prod',
			platform: 'node',
			releaseVersion: '1.0.0',
			stacktrace: [],
			metadata: {},
			traceId: 'trace-1',
			createdAt: new Date('2026-01-01T00:00:00Z'),
		});

		const res = await GET(makeEvent('issue-2'));
		expect(res.status).toBe(200);
		const body = await res.json();

		expect(body.report).toBeNull();
		expect(body.latestOccurrence.id).toBe('occ-1');
		expect(getLatestAgentOccurrence).toHaveBeenCalledWith('issue-2');
		expect(getAgentReportDetail).not.toHaveBeenCalled();
	});

	it('does not write an audit log entry (read-only)', async () => {
		resolveAgentIssueScope.mockResolvedValue({
			issueId: 'issue-1',
			projectId: 'project-1',
			organizationId: 'org-1',
			issueType: 'system_error',
			assignedTo: null,
			assigneeType: null,
			waitingOn: null,
		});
		getAgentIssueDetail.mockResolvedValue({ id: 'issue-1', issueType: 'system_error' });

		await GET(makeEvent());
		expect(writeAgentAuditLog).not.toHaveBeenCalled();
	});
});
