import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { requireReportAccessForIssue } from '$lib/server/report-access';
import { requireIssueAccessAnyType } from '$lib/server/issue-access-dispatch';
import { claimIssue, releaseClaim, ClaimConflictError } from '$lib/db/queries/reports';
import { sendIssueNotificationEmails } from '$lib/server/notify';

// R4/R17 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): the claim DELETE route's per-issue-type
// access dispatch now goes through the shared `requireIssueAccessAnyType`
// (issue-access-dispatch.ts) -- `requireReportAccessForIssue` alone 404s on any `system_error`
// issue id (report-access.ts's §9 strict-separation guarantee), so an agent's claim on a
// system_error issue could never be force-released by a human before R4. `requireIssueAccessAnyType`
// covers both `write` and `force-release` (owner/admin-only, same rule report-access.ts's
// force-release enforces) for both issue types in one place, so this route no longer needs its
// own copy.

// Manual Issues M1 (design §7, §9): human claim/release through the session-authenticated API,
// same atomic-conditional-UPDATE mechanics the agent work-loop (M5) will use. `write` role is
// required per the permission matrix, and agents are out of scope for this API (session auth
// only) — the `/api/agent/*` surface is M5.
//
// M4 (§8): notification emails are sent AFTER claimIssue/releaseClaim's transaction has already
// committed (best-effort, never blocks or fails the response — mirrors the invitation pattern).

export const POST: RequestHandler = async ({ params, locals, url }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	await requireReportAccessForIssue(userId, issueId, 'write');

	try {
		const { issue: updated, notified } = await claimIssue(issueId, 'user', userId);
		await sendIssueNotificationEmails(notified, { issueId, origin: url.origin });
		return json({ success: true, issue: updated });
	} catch (err) {
		if (err instanceof ClaimConflictError) {
			throw error(409, 'Issue is already claimed');
		}
		throw err;
	}
};

export const DELETE: RequestHandler = async ({ params, url, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	const forceParam = url.searchParams.get('force') === 'true';

	// force-release requires owner/admin; a plain release requires only the ordinary write role
	// (the caller still has to actually hold the claim — releaseClaim's own conditional UPDATE
	// enforces that, a 409 if not). Dispatches per issue_type (R4/R17) so a system_error issue's
	// claim can be released here too, not just a user_report's.
	await requireIssueAccessAnyType(userId, issueId, forceParam ? 'force-release' : 'write');

	try {
		const { issue: updated, notified } = await releaseClaim(issueId, userId, { force: forceParam });
		await sendIssueNotificationEmails(notified, { issueId, origin: url.origin });
		return json({ success: true, issue: updated });
	} catch (err) {
		if (err instanceof ClaimConflictError) {
			throw error(409, 'Issue is not claimed by you');
		}
		throw err;
	}
};
