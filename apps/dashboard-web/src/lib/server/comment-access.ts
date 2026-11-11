import { error } from '@sveltejs/kit';
import { eq } from 'drizzle-orm';
import { db } from './db';
import { issues } from '$lib/db/schema';
import { requireIssueAccessAnyType } from './issue-access-dispatch';

/**
 * Manual Issues M3 (docs/plans/MANUAL_ISSUES_DESIGN.md §5, §9). Threads work on BOTH issue
 * types, unlike report-access.ts (deliberately `user_report`-only, per its own header comment).
 *
 * R17 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): the actual per-issue-type dispatch now lives
 * in `issue-access-dispatch.ts`, shared with the attachments download route and R4's claim
 * release path -- this module is a thin wrapper that adds the `issueStatus` this file's own
 * callers need (the "author, until resolved" edit/delete gate) on top of that shared result.
 */

export interface CommentAccessContext {
	issueId: string;
	issueType: string;
	organizationId: string;
	/** §9: "author, until the issue is resolved" -- the route layer's own-comment edit/delete gate. */
	issueStatus: string;
	/** Org role, when resolvable. Used by the route layer for the owner/admin delete-others rule. */
	role: string | null;
}

async function loadIssueStatus(issueId: string): Promise<string> {
	const rows = await db.select({ status: issues.status }).from(issues).where(eq(issues.id, issueId));

	if (rows.length === 0) {
		throw error(404, 'Issue not found');
	}

	return rows[0].status;
}

/**
 * GET (list) access: any member who can read the issue (§9), whichever issue type it is.
 * Throws (401/403/404) on failure, same as the helpers it dispatches to.
 */
export async function requireCommentReadAccess(
	userId: string,
	issueId: string
): Promise<CommentAccessContext> {
	const { issueType, organizationId, role } = await requireIssueAccessAnyType(userId, issueId, 'read');
	const issueStatus = await loadIssueStatus(issueId);
	return { issueId, issueType, organizationId, issueStatus, role };
}

/**
 * POST (create a comment) access: §5/§9 -- "any member who can read the issue (viewers
 * included)", for BOTH issue types. For a `user_report` issue that is the dedicated `comment`
 * action (explicitly open to viewers); for a `system_error` issue `read` already means exactly
 * that via `requireIssueAccess` (a project-member grant conveys read, per its own doc comment).
 */
export async function requireCommentWriteAccess(
	userId: string,
	issueId: string
): Promise<CommentAccessContext> {
	const { issueType, organizationId, role } = await requireIssueAccessAnyType(userId, issueId, 'comment');
	const issueStatus = await loadIssueStatus(issueId);
	return { issueId, issueType, organizationId, issueStatus, role };
}

/** §9: "delete others' comments: owner, admin" -- same roles as report-access.ts's force-release. */
export function isCommentModeratorRole(role: string | null): boolean {
	return role === 'owner' || role === 'admin';
}
