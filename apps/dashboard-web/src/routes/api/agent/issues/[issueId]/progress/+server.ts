import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { resolveAgentIssueScope } from '$lib/server/agent-issue-scope';
import { recordAgentProgress } from '$lib/db/queries/agent-work';
import { writeAgentAuditLog } from '$lib/server/agent-audit';

// Manual Issues M5 stage 2 (design §7 step 3, Q7). In-app notification only -- no email, per
// notify.ts's EMAILABLE_KINDS (deliberately omits 'progress_update'), so unlike claim/questions
// this route never calls sendIssueNotificationEmails.

export const POST: RequestHandler = async ({ request, params }) => {
	const ctx = await authenticateAgentRequest(request);
	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	await resolveAgentIssueScope(issueId, ctx.organizationId);

	const body = await request.json().catch(() => null);
	if (!body || typeof body !== 'object' || typeof body.message_md !== 'string' || body.message_md.trim().length === 0) {
		throw error(400, 'message_md is required');
	}

	await recordAgentProgress(issueId, ctx.agentId, body.message_md.trim());
	await writeAgentAuditLog(ctx, 'agent.issue.progress_update', 'issue', issueId, {});

	return json({ success: true }, { status: 201 });
};
