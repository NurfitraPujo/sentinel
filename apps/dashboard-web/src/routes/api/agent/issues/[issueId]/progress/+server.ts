import { json, error } from '@sveltejs/kit';
import { withAgentIssue } from '$lib/server/agent-route';
import { recordAgentProgress } from '$lib/db/queries/agent-work';

// Manual Issues M5 stage 2 (design §7 step 3, Q7). In-app notification only -- no email, per
// notify.ts's EMAILABLE_KINDS (deliberately omits 'progress_update'), so unlike claim/questions
// this route never calls sendIssueNotificationEmails.
//
// R16 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): migrated onto `withAgentIssue`.

export const POST = withAgentIssue(async (ctx, issue, event) => {
	const body = await event.request.json().catch(() => null);
	if (!body || typeof body !== 'object' || typeof body.message_md !== 'string' || body.message_md.trim().length === 0) {
		throw error(400, 'message_md is required');
	}

	await recordAgentProgress(issue.issueId, ctx.agentId, body.message_md.trim());

	return {
		response: json({ success: true }, { status: 201 }),
		audit: { action: 'agent.issue.progress_update', resourceType: 'issue', resourceId: issue.issueId },
	};
});
