import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { claimIssue, releaseClaim, ClaimConflictError } from '$lib/db/queries/reports';
import { requireReportAccessForIssue } from '$lib/server/report-access';
import { sendIssueNotificationEmails } from '$lib/server/notify';

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
	// enforces that, a 409 if not).
	await requireReportAccessForIssue(userId, issueId, forceParam ? 'force-release' : 'write');

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
