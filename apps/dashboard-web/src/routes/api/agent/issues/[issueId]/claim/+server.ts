import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { resolveAgentIssueScope } from '$lib/server/agent-issue-scope';
import { claimIssue, releaseClaim, ClaimConflictError } from '$lib/db/queries/reports';
import { writeAgentAuditLog } from '$lib/server/agent-audit';
import { sendIssueNotificationEmails } from '$lib/server/notify';

// Manual Issues M5 stage 2 (design §7 step 2, §6). POST claims (atomic conditional UPDATE, 409 on
// conflict); DELETE releases the agent's OWN claim -- an agent can never force-release (that stays
// owner/admin-only through the session-authenticated route per §9).

export const POST: RequestHandler = async ({ request, params, url }) => {
	const ctx = await authenticateAgentRequest(request);
	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	await resolveAgentIssueScope(issueId, ctx.organizationId);

	try {
		const { issue: updated, notified } = await claimIssue(issueId, 'agent', ctx.agentId);
		await sendIssueNotificationEmails(notified, { issueId, origin: url.origin });
		await writeAgentAuditLog(ctx, 'agent.issue.claimed', 'issue', issueId, {});
		return json({ success: true, issue: updated });
	} catch (err) {
		if (err instanceof ClaimConflictError) {
			throw error(409, 'Issue is already claimed');
		}
		throw err;
	}
};

export const DELETE: RequestHandler = async ({ request, params, url }) => {
	const ctx = await authenticateAgentRequest(request);
	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	await resolveAgentIssueScope(issueId, ctx.organizationId);

	try {
		// Non-force: releaseClaim's own conditional UPDATE (`WHERE assigned_to = $agentId`) is what
		// actually enforces "only the current claimant may release" -- an agent cannot pass someone
		// else's id here since `ctx.agentId` comes from the credential (B7), not the request.
		const { issue: updated, notified } = await releaseClaim(issueId, ctx.agentId, { actorType: 'agent' });
		await sendIssueNotificationEmails(notified, { issueId, origin: url.origin });
		await writeAgentAuditLog(ctx, 'agent.issue.claim_released', 'issue', issueId, {});
		return json({ success: true, issue: updated });
	} catch (err) {
		if (err instanceof ClaimConflictError) {
			throw error(409, 'Issue is not claimed by this agent');
		}
		throw err;
	}
};
