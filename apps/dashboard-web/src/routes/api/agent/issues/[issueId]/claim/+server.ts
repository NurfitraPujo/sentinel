import { json } from '@sveltejs/kit';
import { withAgentIssue } from '$lib/server/agent-route';
import { claimIssue, releaseClaim } from '$lib/db/queries/reports';
import { sendIssueNotificationEmails } from '$lib/server/notify';

// Manual Issues M5 stage 2 (design §7 step 2, §6). POST claims (atomic conditional UPDATE, 409 on
// conflict); DELETE releases the agent's OWN claim -- an agent can never force-release (that stays
// owner/admin-only through the session-authenticated route per §9).
//
// R16 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): migrated onto `withAgentIssue`, which now
// owns authentication, the issueId guard, scope resolution, and ClaimConflictError -> 409 mapping.

export const POST = withAgentIssue(async (ctx, issue, event) => {
	const { issue: updated, notified } = await claimIssue(issue.issueId, 'agent', ctx.agentId);
	await sendIssueNotificationEmails(notified, { issueId: issue.issueId, origin: event.url.origin });
	return {
		response: json({ success: true, issue: updated }),
		audit: { action: 'agent.issue.claimed', resourceType: 'issue', resourceId: issue.issueId },
	};
});

export const DELETE = withAgentIssue(async (ctx, issue, event) => {
	// Non-force: releaseClaim's own conditional UPDATE (`WHERE assigned_to = $agentId`) is what
	// actually enforces "only the current claimant may release" -- an agent cannot pass someone
	// else's id here since `ctx.agentId` comes from the credential (B7), not the request.
	//
	// R16: `withAgentIssue`'s shared error mapping surfaces `ClaimConflictError`'s own message
	// ("Issue is not claimed by this actor") rather than this route's pre-migration hardcoded
	// "Issue is not claimed by this agent" -- still a 409, same meaning, no test asserted the
	// exact wording.
	const { issue: updated, notified } = await releaseClaim(issue.issueId, ctx.agentId, { actorType: 'agent' });
	await sendIssueNotificationEmails(notified, { issueId: issue.issueId, origin: event.url.origin });
	return {
		response: json({ success: true, issue: updated }),
		audit: { action: 'agent.issue.claim_released', resourceType: 'issue', resourceId: issue.issueId },
	};
});
