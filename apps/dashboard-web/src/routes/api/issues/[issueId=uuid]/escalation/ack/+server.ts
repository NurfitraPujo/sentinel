import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { requireReportAccessForIssue } from '$lib/server/report-access';
import { acknowledgeEscalation, getLatestEscalation } from '$lib/server/escalation';
import { log } from '$lib/server/observability/log';

/**
 * sentinel-user-report-issue §4.4a — `POST /api/issues/{issueId}/escalation/ack`.
 *
 * "The assignee presses Acknowledge … Any of those stamps `acknowledged_at` and stops the clock."
 * The other three ways to acknowledge are implicit and live at their own routes (claim, comment,
 * status change), all of them calling `acknowledgeEscalationQuietly`; this is the explicit one.
 *
 * §4.4a fixes exactly one code here — **`403` if the caller is not the current assignee** — and
 * this route is a dashboard route, so it uses SvelteKit's `error(status, 'message')` shape and not
 * §7's envelope (§4.4a's note on errors, and §7's own).
 *
 * The order of the checks is deliberate. Tenancy first, through
 * `requireReportAccessForIssue(..., 'read')`: an issue id belonging to another organization —
 * or a `system_error` issue id, which that helper 404s by construction (§9 strict separation) —
 * must be indistinguishable from one that does not exist, so a caller cannot use the `403` to
 * learn that a report exists and who is on it. Only then does the assignee check run.
 *
 * `read` rather than `write` is the right bar: acknowledging is not a mutation of the report, and
 * §4.4's own rule is stricter anyway — you must BE the assignee, which no role grants.
 */
export const POST: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	await requireReportAccessForIssue(userId, issueId, 'read');

	const latest = await getLatestEscalation(issueId);
	if (!latest) {
		throw error(404, 'This report has no escalation to acknowledge');
	}

	if (latest.assigneeId !== userId) {
		throw error(403, 'Only the current escalation assignee may acknowledge');
	}

	// Already acknowledged is a success, not a conflict: pressing Acknowledge twice must not
	// produce an error, and `acknowledgeEscalation`'s conditional UPDATE deliberately matches
	// nothing the second time rather than moving the timestamp.
	const stamped = await acknowledgeEscalation(issueId, userId);
	const acknowledgedAt = stamped?.acknowledgedAt ?? latest.acknowledgedAt;

	if (stamped) {
		log.info('escalation.acknowledged', { issueId, userId, hop: stamped.hop, implicit: false });
	}

	return json({
		success: true,
		escalation: {
			scheduleId: latest.scheduleId,
			assigneeId: latest.assigneeId,
			escalatedAt: latest.escalatedAt,
			ackDeadlineAt: latest.ackDeadlineAt,
			acknowledgedAt,
			hop: latest.hop,
			exhausted: latest.exhausted,
		},
	});
};
