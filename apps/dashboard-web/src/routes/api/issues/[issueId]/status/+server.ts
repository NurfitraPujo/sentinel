import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { updateIssueStatus } from '$lib/db/queries/issues';
import { requireIssueAccess, validateResolvedInVersion } from '$lib/server/issue-access';
import { sendIssueNotificationEmails } from '$lib/server/notify';
import { acknowledgeEscalationQuietly } from '$lib/server/escalation';

const VALID_STATUSES = ['unresolved', 'resolved', 'ignored'] as const;
type IssueStatus = (typeof VALID_STATUSES)[number];

function isValidStatus(value: unknown): value is IssueStatus {
	return typeof value === 'string' && (VALID_STATUSES as readonly string[]).includes(value);
}

export const PATCH: RequestHandler = async ({ request, params, locals, url }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const issueId = params.issueId;

	const body = await request.json().catch(() => ({}));
	const { status, resolvedInVersion } = body;

	if (!isValidStatus(status)) {
		throw error(400, `status must be one of: ${VALID_STATUSES.join(', ')}`);
	}

	// Validate before touching the DB so an oversized value 400s rather than reaching
	// updateIssueStatus and 500ing on the varchar(100) column constraint (D10 item 3).
	const validatedResolvedInVersion = validateResolvedInVersion(resolvedInVersion);

	// D10/D23: same role allowlist as the bulk endpoint, plus project membership.
	await requireIssueAccess(userId, issueId, 'write');

	const { changed, notified } = await updateIssueStatus(
		issueId,
		status,
		validatedResolvedInVersion ?? undefined,
		'user',
		userId
	);
	if (changed) {
		await sendIssueNotificationEmails(notified, { issueId, origin: url.origin });

		// §4.4's implicit acknowledgement: "the assignee claims, comments on, or changes the status
		// of that issue … Someone who is already working on it must not be reassigned for not
		// pressing a button." Post-commit and never throwing — a failed stamp costs one extra
		// reassignment, which the chain is bounded against, and must not fail this request.
		await acknowledgeEscalationQuietly(issueId, userId);
	}

	return json({ success: true, status, changed });
};
