import { json, error } from '@sveltejs/kit';
import { withAgentIssue } from '$lib/server/agent-route';
import { getAgentIssueDetail, getAgentReportDetail, getLatestAgentOccurrence } from '$lib/db/queries/agent-reads';
import { agentIssueRelations } from '$lib/server/agent-relations';

// N1c (agent read endpoints). GET /api/agent/issues/[issueId] -- read-only detail view spanning
// both issue types (like agent-work.ts's list route): full issue row, the user_report companion
// (null for system_error), the latest occurrence (null for user_report), and relations. No audit
// write, no activity write -- a GET has nothing to record.
//
// sentinel-cli-browser-login: a personal key reads only an issue its owner can see (withAgentIssue's 404), and the
// relations panel redacts a related issue outside the owner's projects/grants, as the dashboard's issue page does.

export const GET = withAgentIssue('read', async (ctx, issue) => {
	const issueDetail = await getAgentIssueDetail(issue.issueId);
	if (!issueDetail) {
		// Shouldn't happen -- withAgentIssue already resolved this issueId -- but keep the same
		// "not found" shape rather than surfacing a null-deref.
		throw error(404, 'Issue not found');
	}

	const [report, latestOccurrence, relations] = await Promise.all([
		issue.issueType === 'user_report' ? getAgentReportDetail(issue.issueId) : Promise.resolve(null),
		issue.issueType === 'system_error' ? getLatestAgentOccurrence(issue.issueId) : Promise.resolve(null),
		agentIssueRelations(ctx, issue.issueId),
	]);

	return {
		response: json({
			issue: issueDetail,
			report,
			latestOccurrence,
			relations,
		}),
	};
});

