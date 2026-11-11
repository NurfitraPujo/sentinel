import { error, json } from '@sveltejs/kit';
import type { RequestEvent } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { runAgentOp } from '$lib/server/agent-ops';
import { writeAgentAuditLog } from '$lib/server/agent-audit';

// A08 (N7e, docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md): PATCH/DELETE
// /api/agent/issues/[issueId]/comments/[commentId] -- own-comment edit/delete for agents, gated
// author-only (mirrors the human route at api/issues/[issueId]/comments/[commentId]/+server.ts,
// minus its moderator carve-out -- see agent-ops.ts's `loadOwnedAgentComment` doc comment).
//
// This does NOT use `agentOpRoute` (agent-ops.ts) directly: that helper's route signature only
// carries `issueId`, but these ops need a second id. Instead it does the same
// authenticate -> run op -> audit -> respond sequence by hand, folding `commentId` (from the URL)
// into the op's `params` as `comment_id` -- the SAME field name a batch caller would supply in its
// own `params`, so `agentOps['comments.edit'|'comments.delete']` sees one consistent shape either
// way.

async function handle(op: string, event: RequestEvent): Promise<Response> {
	const ctx = await authenticateAgentRequest(event.request);

	const { issueId, commentId } = event.params as { issueId?: string; commentId?: string };
	if (!issueId || !commentId) {
		throw error(400, 'Missing issueId or commentId');
	}

	const rawBody = op === 'comments.edit' ? await event.request.json().catch(() => ({})) : {};
	const params = { ...(rawBody as Record<string, unknown>), comment_id: commentId };

	const result = await runAgentOp(op, ctx, issueId, params, event.url.origin);

	if (result.audit) {
		await writeAgentAuditLog(
			ctx,
			result.audit.action,
			result.audit.resourceType,
			result.audit.resourceId,
			result.audit.metadata
		);
	}

	return json(result.body, { status: result.status });
}

export const PATCH: RequestHandler = (event) => handle('comments.edit', event);
export const DELETE: RequestHandler = (event) => handle('comments.delete', event);
