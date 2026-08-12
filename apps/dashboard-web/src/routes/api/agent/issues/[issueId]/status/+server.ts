import { json, error } from '@sveltejs/kit';
import { withAgentIssue } from '$lib/server/agent-route';
import { updateIssueStatus } from '$lib/db/queries/issues';
import { validateResolvedInVersion } from '$lib/server/issue-access';
import { sendIssueNotificationEmails } from '$lib/server/notify';

// Manual Issues M5 stage 2 (design §7 step 6). PATCH .../status { status, resolved_in_version? }
// -> updateIssueStatus with actorType 'agent' (resolved_by_type='agent' when resolving).
//
// R16 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): migrated onto `withAgentIssue`.

const VALID_STATUSES = ['unresolved', 'resolved', 'ignored'] as const;
type IssueStatus = (typeof VALID_STATUSES)[number];

function isValidStatus(value: unknown): value is IssueStatus {
	return typeof value === 'string' && (VALID_STATUSES as readonly string[]).includes(value);
}

export const PATCH = withAgentIssue(async (ctx, issue, event) => {
	const body = await event.request.json().catch(() => ({}));
	const { status } = body;
	if (!isValidStatus(status)) {
		throw error(400, `status must be one of: ${VALID_STATUSES.join(', ')}`);
	}

	const validatedResolvedInVersion = validateResolvedInVersion(body.resolved_in_version);

	const notified = await updateIssueStatus(
		issue.issueId,
		status,
		validatedResolvedInVersion ?? undefined,
		'agent',
		ctx.agentId
	);
	await sendIssueNotificationEmails(notified, { issueId: issue.issueId, origin: event.url.origin });

	return {
		response: json({ success: true, status }),
		audit: {
			action: 'agent.issue.status_changed',
			resourceType: 'issue',
			resourceId: issue.issueId,
			metadata: { status, resolvedInVersion: validatedResolvedInVersion ?? undefined },
		},
	};
});
