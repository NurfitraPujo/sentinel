import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { resolveAgentIssueScope } from '$lib/server/agent-issue-scope';
import { createComment, listComments, CommentValidationError, CommentNotFoundError } from '$lib/db/queries/comments';
import { writeAgentAuditLog } from '$lib/server/agent-audit';
import { sendIssueNotificationEmails } from '$lib/server/notify';

// Manual Issues M5 stage 2 (design §7 step 3/5). Non-blocking agent comments (reuses M3's
// createComment with authorType 'agent'), and GET .../comments?after= -- the SAME polling
// query M3's session route uses (design §7 step 5: "agents poll GET .../comments?after= to read
// answers -- pull model, Q6").

export const GET: RequestHandler = async ({ request, params, url }) => {
	const ctx = await authenticateAgentRequest(request);
	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	await resolveAgentIssueScope(issueId, ctx.organizationId);

	const afterParam = url.searchParams.get('after');
	let after: Date | undefined;
	if (afterParam !== null) {
		const parsed = new Date(afterParam);
		if (Number.isNaN(parsed.getTime())) {
			throw error(400, 'after must be a valid ISO 8601 timestamp');
		}
		after = parsed;
	}

	const comments = await listComments(issueId, { after });
	return json({ comments });
};

export const POST: RequestHandler = async ({ request, params, url }) => {
	const ctx = await authenticateAgentRequest(request);
	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	await resolveAgentIssueScope(issueId, ctx.organizationId);

	const body = await request.json().catch(() => null);
	if (!body || typeof body !== 'object' || typeof body.body_md !== 'string' || body.body_md.trim().length === 0) {
		throw error(400, 'body_md is required');
	}

	let attachmentIds: string[] | undefined;
	if (body.attachmentIds !== undefined) {
		if (!Array.isArray(body.attachmentIds) || !body.attachmentIds.every((id: unknown) => typeof id === 'string')) {
			throw error(400, 'attachmentIds must be an array of strings');
		}
		attachmentIds = body.attachmentIds;
	}

	try {
		const { comment, notified } = await createComment({
			issueId,
			authorType: 'agent',
			authorId: ctx.agentId,
			bodyMd: body.body_md,
			attachmentIds,
		});
		await sendIssueNotificationEmails(notified, { issueId, origin: url.origin });
		await writeAgentAuditLog(ctx, 'agent.issue.commented', 'issue', issueId, { commentId: comment.id });

		return json({ comment }, { status: 201 });
	} catch (err) {
		if (err instanceof CommentValidationError) {
			throw error(400, err.message);
		}
		if (err instanceof CommentNotFoundError) {
			throw error(404, err.message);
		}
		throw err;
	}
};
