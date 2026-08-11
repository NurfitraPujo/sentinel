import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { resolveAgentIssueScope } from '$lib/server/agent-issue-scope';
import { createComment, CommentValidationError, CommentNotFoundError } from '$lib/db/queries/comments';
import { writeAgentAuditLog } from '$lib/server/agent-audit';
import { sendIssueNotificationEmails } from '$lib/server/notify';

// Manual Issues M5 stage 2 (design §7 step 4, Q11). A blocking question: `blocking: true` comment
// row + `issues.waiting_on = audience` set in the SAME transaction (createComment) + fans out
// kind 'question_asked', which notify.ts's BLOCKING_BYPASS_KIND makes bypass the 15-min email
// throttle unconditionally (Q11: "a direct question deserves immediate email").

const VALID_AUDIENCES = ['reporter', 'team'] as const;
type Audience = (typeof VALID_AUDIENCES)[number];

function isValidAudience(value: unknown): value is Audience {
	return typeof value === 'string' && (VALID_AUDIENCES as readonly string[]).includes(value);
}

export const POST: RequestHandler = async ({ request, params, url }) => {
	const ctx = await authenticateAgentRequest(request);
	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	await resolveAgentIssueScope(issueId, ctx.organizationId);

	const body = await request.json().catch(() => null);
	if (!body || typeof body !== 'object') {
		throw error(400, 'Expected a JSON body');
	}
	if (typeof body.body_md !== 'string' || body.body_md.trim().length === 0) {
		throw error(400, 'body_md is required');
	}
	if (!isValidAudience(body.audience)) {
		throw error(400, `audience must be one of: ${VALID_AUDIENCES.join(', ')}`);
	}

	try {
		const { comment, notified } = await createComment({
			issueId,
			authorType: 'agent',
			authorId: ctx.agentId,
			bodyMd: body.body_md,
			blocking: true,
			waitingOnAudience: body.audience,
		});
		await sendIssueNotificationEmails(notified, { issueId, origin: url.origin });
		await writeAgentAuditLog(ctx, 'agent.issue.question_asked', 'issue', issueId, {
			commentId: comment.id,
			audience: body.audience,
		});

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
