import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { batchUpdateIssues, MAX_BATCH_ISSUE_IDS } from '$lib/db/queries/issues';
import { requireProjectAccess, validateResolvedInVersion } from '$lib/server/issue-access';

export const POST: RequestHandler = async ({ request, params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const { projectId } = params;

	// D23: this used to check `organization_members` and a locally-declared role allowlist, with no
	// project-membership check — leaving the BULK path more permissive than the single-issue one
	// once D10 tightened that. A user in the org but not on the project could bulk-resolve the
	// project's issues while being refused a single one. Both paths now share one implementation,
	// so they cannot drift apart again; the role allowlist lives in ISSUE_WRITE_ROLES.
	await requireProjectAccess(userId, projectId, 'write');

	// D40-class: a malformed body must 400, not surface as an unhandled 500.
	const body = await request.json().catch(() => {
		throw error(400, 'Invalid JSON body');
	});
	const { action, issueIds, assigneeType, assignedTo } = body;

	// D23: resolvedInVersion reached the DB unvalidated here, so an oversized value 500'd on the
	// varchar(100) constraint. The single-issue path already validated it; share the check.
	// batchUpdateIssues takes `string | undefined`; the validator normalises omitted/blank to null.
	const resolvedInVersion = validateResolvedInVersion(body.resolvedInVersion) ?? undefined;

	if (!action || !issueIds || !Array.isArray(issueIds) || issueIds.length === 0) {
		throw error(400, 'Invalid request body: action and non-empty issueIds array are required');
	}

	// Mirrors the ingestor's own batch cap (apps/ingestor-go/main.go:47) — reject outright rather
	// than silently truncating; batchUpdateIssues enforces the same limit as a defensive backstop.
	if (issueIds.length > MAX_BATCH_ISSUE_IDS) {
		throw error(413, `Batch too large: ${issueIds.length} ids exceeds max of ${MAX_BATCH_ISSUE_IDS}`);
	}

	if (!['resolve', 'ignore', 'unresolve', 'assign'].includes(action)) {
		throw error(400, 'Invalid action');
	}

	if (action === 'assign' && (!assigneeType || !assignedTo)) {
		throw error(400, 'assigneeType and assignedTo are required for assign action');
	}

	const updatedCount = await batchUpdateIssues(
		projectId,
		action,
		issueIds,
		{
			resolvedInVersion,
			assigneeType,
			assignedTo,
			actorType: 'user',
			actorId: userId
		}
	);

	return json({ success: true, updated: updatedCount });
};
