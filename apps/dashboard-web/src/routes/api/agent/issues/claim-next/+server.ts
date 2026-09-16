import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { writeAgentAuditLog } from '$lib/server/agent-audit';
import { sendIssueNotificationEmails } from '$lib/server/notify';
import { getIssueRelations } from '$lib/db/queries/issues';
import { getAgentReportDetail, getLatestAgentOccurrence } from '$lib/db/queries/agent-reads';
import {
	claimNextAgentIssue,
	AGENT_ISSUE_SOURCES,
	type AgentIssuesSort,
	type AgentIssueSource,
} from '$lib/db/queries/agent-work';

// sentinel-agent-claim-next-issue: POST /api/agent/issues/claim-next -- the three-call
// "list ?claimed=false -> claim -> get" head of the triage recipe (SENTINEL_AGENT_GUIDE §5 steps
// 1-3) as ONE round trip, with the select+claim step atomic so two agents polling the same queue
// never both get the same issue (claimNextAgentIssue, agent-work.ts). The response is the SAME
// shape as GET /api/agent/issues/[issueId] (issue row, report, latestOccurrence, relations), with
// `issue` already carrying the claim, so an agent's existing detail parser reads it unchanged.
//
// Not an agent-ops.ts op on purpose: batch's contract is `{op, issueId, params}` and this call has
// no issueId until it has run. The single-issue `POST .../[issueId]/claim` is untouched.
//
// Filters arrive in the JSON body (a POST has no reason to smuggle them into the query string),
// snake_case-free because every name here is a single word, and they are exactly the list
// route's own vocabulary so discovery and claiming cannot disagree. Validation failures throw
// `error(400, message)` like every other agent mutation route. An empty queue is a 404
// (`No unclaimed issue matches`): for a worker loop "nothing to do" and "not there" are the same
// answer, and the CLI already maps 404 to its `not found` exit code, which is what a shell loop
// needs to stop on.

const VALID_TYPES = ['user_report', 'system_error'] as const;
const VALID_SORTS: readonly AgentIssuesSort[] = ['firstSeen', 'lastSeen'];

function optionalString(body: Record<string, unknown>, key: string): string | undefined {
	const raw = body[key];
	if (raw === undefined || raw === null || raw === '') return undefined;
	if (typeof raw !== 'string') {
		throw error(400, `${key} must be a string`);
	}
	return raw;
}

export const POST: RequestHandler = async ({ request, url }) => {
	const ctx = await authenticateAgentRequest(request);

	const parsed: unknown = await request.json().catch(() => ({}));
	const body: Record<string, unknown> = parsed && typeof parsed === 'object' ? (parsed as Record<string, unknown>) : {};

	const typeParam = optionalString(body, 'type');
	let type: 'user_report' | 'system_error' | undefined;
	if (typeParam !== undefined && typeParam !== 'any') {
		if (!(VALID_TYPES as readonly string[]).includes(typeParam)) {
			throw error(400, `type must be one of: any, ${VALID_TYPES.join(', ')}`);
		}
		type = typeParam as 'user_report' | 'system_error';
	}

	const projectId = optionalString(body, 'project');
	const environment = optionalString(body, 'environment');

	const sourceParam = optionalString(body, 'source');
	let source: AgentIssueSource | undefined;
	if (sourceParam !== undefined) {
		if (!(AGENT_ISSUE_SOURCES as readonly string[]).includes(sourceParam)) {
			throw error(400, `source must be one of: ${AGENT_ISSUE_SOURCES.join(', ')}`);
		}
		source = sourceParam as AgentIssueSource;
	}

	const sortParam = optionalString(body, 'sort');
	let sort: AgentIssuesSort | undefined;
	if (sortParam !== undefined) {
		if (!(VALID_SORTS as readonly string[]).includes(sortParam)) {
			throw error(400, `sort must be one of: ${VALID_SORTS.join(', ')}`);
		}
		sort = sortParam as AgentIssuesSort;
	}

	const claimed = await claimNextAgentIssue({
		organizationId: ctx.organizationId,
		agentId: ctx.agentId,
		type,
		projectId,
		environment,
		source,
		sort,
	});

	if (!claimed) {
		throw error(404, 'No unclaimed issue matches');
	}

	const { issue, notified } = claimed;
	await sendIssueNotificationEmails(notified, { issueId: issue.id, origin: url.origin });

	// Same audit action as a single-issue claim (it IS one, as far as the compliance trail is
	// concerned), tagged so the two paths stay distinguishable when reading the log back.
	await writeAgentAuditLog(ctx, 'agent.issue.claimed', 'issue', issue.id, {
		claimNext: true,
		filters: { type, project: projectId, environment, source, sort },
	});

	// The detail companions, exactly as GET /api/agent/issues/[issueId] assembles them.
	const [report, latestOccurrence, relations] = await Promise.all([
		issue.issueType === 'user_report' ? getAgentReportDetail(issue.id) : Promise.resolve(null),
		issue.issueType === 'system_error' ? getLatestAgentOccurrence(issue.id) : Promise.resolve(null),
		getIssueRelations(issue.id),
	]);

	return json({ issue, report, latestOccurrence, relations });
};
