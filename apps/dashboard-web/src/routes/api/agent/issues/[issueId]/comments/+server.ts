import { json, error } from '@sveltejs/kit';
import { withAgentIssue } from '$lib/server/agent-route';
import { createComment, listComments } from '$lib/db/queries/comments';
import { sendIssueNotificationEmails } from '$lib/server/notify';

// Manual Issues M5 stage 2 (design §7 step 3/5). Non-blocking agent comments (reuses M3's
// createComment with authorType 'agent'), and GET .../comments?after= -- the SAME polling
// query M3's session route uses (design §7 step 5: "agents poll GET .../comments?after= to read
// answers -- pull model, Q6").
//
// R16 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): migrated onto `withAgentIssue`. GET has
// nothing to audit (read-only), so its handler omits `audit` entirely.

export const GET = withAgentIssue(async (_ctx, issue, event) => {
	const afterParam = event.url.searchParams.get('after');
	let after: Date | undefined;
	if (afterParam !== null) {
		const parsed = new Date(afterParam);
		if (Number.isNaN(parsed.getTime())) {
			throw error(400, 'after must be a valid ISO 8601 timestamp');
		}
		after = parsed;
	}

	const comments = await listComments(issue.issueId, { after });
	return { response: json({ comments }) };
});

export const POST = withAgentIssue(async (ctx, issue, event) => {
	const body = await event.request.json().catch(() => null);
	if (!body || typeof body !== 'object' || typeof body.body_md !== 'string' || body.body_md.trim().length === 0) {
		throw error(400, 'body_md is required');
	}

	// R18 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): standardized on snake_case
	// (`attachment_ids`), matching `body_md` -- a clean break, no camelCase fallback.
	let attachmentIds: string[] | undefined;
	if (body.attachment_ids !== undefined) {
		if (!Array.isArray(body.attachment_ids) || !body.attachment_ids.every((id: unknown) => typeof id === 'string')) {
			throw error(400, 'attachment_ids must be an array of strings');
		}
		attachmentIds = body.attachment_ids;
	}

	const { comment, notified } = await createComment({
		issueId: issue.issueId,
		authorType: 'agent',
		authorId: ctx.agentId,
		bodyMd: body.body_md,
		attachmentIds,
	});
	await sendIssueNotificationEmails(notified, { issueId: issue.issueId, origin: event.url.origin });

	return {
		response: json({ comment }, { status: 201 }),
		audit: {
			action: 'agent.issue.commented',
			resourceType: 'issue',
			resourceId: issue.issueId,
			metadata: { commentId: comment.id },
		},
	};
});
