import { json, error } from '@sveltejs/kit';
import { withAgentIssue } from '$lib/server/agent-route';
import { createComment } from '$lib/db/queries/comments';
import { sendIssueNotificationEmails } from '$lib/server/notify';
import { IdempotencyKeyOpMismatchError } from '$lib/server/agent-idempotency';

const MAX_IDEMPOTENCY_KEY_LENGTH = 255;

// Manual Issues M5 stage 2 (design §7 step 4, Q11). A blocking question: `blocking: true` comment
// row + `issues.waiting_on = audience` set in the SAME transaction (createComment) + fans out
// kind 'question_asked', which notify.ts's BLOCKING_BYPASS_KIND makes bypass the 15-min email
// throttle unconditionally (Q11: "a direct question deserves immediate email").
//
// R16 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): migrated onto `withAgentIssue`.

const VALID_AUDIENCES = ['reporter', 'team'] as const;
type Audience = (typeof VALID_AUDIENCES)[number];

function isValidAudience(value: unknown): value is Audience {
	return typeof value === 'string' && (VALID_AUDIENCES as readonly string[]).includes(value);
}

export const POST = withAgentIssue(async (ctx, issue, event) => {
	const body = await event.request.json().catch(() => null);
	if (!body || typeof body !== 'object') {
		throw error(400, 'Expected a JSON body');
	}
	if (typeof body.body_md !== 'string' || body.body_md.trim().length === 0) {
		throw error(400, 'body_md is required');
	}
	if (!isValidAudience(body.audience)) {
		throw error(400, `audience must be one of: ${VALID_AUDIENCES.join(', ')}`);
	}

	// N9 (D21): a client-supplied idempotency key makes a retried question replay the ORIGINAL
	// question's comment id with no second waiting_on write and -- crucially -- no second email. A
	// blocking question bypasses the 15-min throttle (question_asked), so without this a crashed
	// agent's retry double-notifies the reporter; the deduplicated replay carries `notified: []`.
	let idempotencyKey: string | undefined;
	if (body.idempotency_key !== undefined && body.idempotency_key !== null) {
		if (typeof body.idempotency_key !== 'string' || body.idempotency_key.trim().length === 0) {
			throw error(400, 'idempotency_key must be a non-empty string');
		}
		if (body.idempotency_key.length > MAX_IDEMPOTENCY_KEY_LENGTH) {
			throw error(400, `idempotency_key must not exceed ${MAX_IDEMPOTENCY_KEY_LENGTH} characters`);
		}
		idempotencyKey = body.idempotency_key;
	}

	let result;
	try {
		result = await createComment({
			issueId: issue.issueId,
			authorType: 'agent',
			authorId: ctx.agentId,
			bodyMd: body.body_md,
			blocking: true,
			waitingOnAudience: body.audience,
			idempotencyKey,
		});
	} catch (err) {
		if (err instanceof IdempotencyKeyOpMismatchError) {
			throw error(409, err.message);
		}
		throw err;
	}

	const { comment, notified, deduplicated } = result;
	// Deduplicated replay ⇒ `notified: []`, so this send is a no-op: exactly one email per question.
	await sendIssueNotificationEmails(notified, { issueId: issue.issueId, origin: event.url.origin });

	return {
		response: json(deduplicated ? { comment, deduplicated: true } : { comment }, { status: 201 }),
		audit: {
			action: 'agent.issue.question_asked',
			resourceType: 'issue',
			resourceId: issue.issueId,
			metadata: { commentId: comment.id, audience: body.audience, deduplicated },
		},
	};
});
