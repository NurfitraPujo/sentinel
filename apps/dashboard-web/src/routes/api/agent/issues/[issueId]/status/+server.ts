import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { resolveAgentIssueScope } from '$lib/server/agent-issue-scope';
import { updateIssueStatus } from '$lib/db/queries/issues';
import { validateResolvedInVersion } from '$lib/server/issue-access';
import { writeAgentAuditLog } from '$lib/server/agent-audit';
import { sendIssueNotificationEmails } from '$lib/server/notify';

// Manual Issues M5 stage 2 (design §7 step 6). PATCH .../status { status, resolved_in_version? }
// -> updateIssueStatus with actorType 'agent' (resolved_by_type='agent' when resolving).

const VALID_STATUSES = ['unresolved', 'resolved', 'ignored'] as const;
type IssueStatus = (typeof VALID_STATUSES)[number];

function isValidStatus(value: unknown): value is IssueStatus {
	return typeof value === 'string' && (VALID_STATUSES as readonly string[]).includes(value);
}

export const PATCH: RequestHandler = async ({ request, params, url }) => {
	const ctx = await authenticateAgentRequest(request);
	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	await resolveAgentIssueScope(issueId, ctx.organizationId);

	const body = await request.json().catch(() => ({}));
	const { status } = body;
	if (!isValidStatus(status)) {
		throw error(400, `status must be one of: ${VALID_STATUSES.join(', ')}`);
	}

	const validatedResolvedInVersion = validateResolvedInVersion(body.resolved_in_version);

	const notified = await updateIssueStatus(
		issueId,
		status,
		validatedResolvedInVersion ?? undefined,
		'agent',
		ctx.agentId
	);
	await sendIssueNotificationEmails(notified, { issueId, origin: url.origin });
	await writeAgentAuditLog(ctx, 'agent.issue.status_changed', 'issue', issueId, {
		status,
		resolvedInVersion: validatedResolvedInVersion ?? undefined,
	});

	return json({ success: true, status });
};
