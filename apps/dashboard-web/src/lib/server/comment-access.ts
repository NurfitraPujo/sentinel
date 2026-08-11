import { error } from '@sveltejs/kit';
import { eq } from 'drizzle-orm';
import { db } from './db';
import { issues, projects } from '$lib/db/schema';
import { requireIssueAccess } from './issue-access';
import { requireReportAccessForIssue, getOrgRole } from './report-access';

/**
 * Manual Issues M3 (docs/plans/MANUAL_ISSUES_DESIGN.md §5, §9). Threads work on BOTH issue
 * types, unlike report-access.ts (deliberately `user_report`-only, per its own header comment) --
 * this module is the per-issue-type dispatcher the M2 download route
 * (attachments/[id]/+server.ts) already established: `user_report` issues go through
 * `requireReportAccessForIssue` (§9's viewer carve-out), `system_error` issues go through the
 * existing `requireIssueAccess` (D10/D17). Comment routes reuse that SAME dispatch rather than
 * inventing a third access model for threads.
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

async function loadIssueTypeContext(
	issueId: string
): Promise<{ issueType: string; organizationId: string; issueStatus: string }> {
	const rows = await db
		.select({
			issueType: issues.issueType,
			status: issues.status,
			organizationId: projects.organizationId,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(eq(issues.id, issueId));

	if (rows.length === 0) {
		throw error(404, 'Issue not found');
	}
	if (!rows[0].organizationId) {
		throw error(400, 'Issue project does not belong to an organization');
	}

	return { issueType: rows[0].issueType, organizationId: rows[0].organizationId, issueStatus: rows[0].status };
}

/**
 * GET (list) access: any member who can read the issue (§9), whichever issue type it is.
 * Throws (401/403/404) on failure, same as the helpers it dispatches to.
 */
export async function requireCommentReadAccess(
	userId: string,
	issueId: string
): Promise<CommentAccessContext> {
	const { issueType, organizationId, issueStatus } = await loadIssueTypeContext(issueId);

	if (issueType === 'user_report') {
		const { role } = await requireReportAccessForIssue(userId, issueId, 'read');
		return { issueId, issueType, organizationId, issueStatus, role };
	}

	await requireIssueAccess(userId, issueId, 'read');
	const role = await getOrgRole(userId, organizationId);
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
	const { issueType, organizationId, issueStatus } = await loadIssueTypeContext(issueId);

	if (issueType === 'user_report') {
		const { role } = await requireReportAccessForIssue(userId, issueId, 'comment');
		return { issueId, issueType, organizationId, issueStatus, role };
	}

	await requireIssueAccess(userId, issueId, 'read');
	const role = await getOrgRole(userId, organizationId);
	return { issueId, issueType, organizationId, issueStatus, role };
}

/** §9: "delete others' comments: owner, admin" -- same roles as report-access.ts's force-release. */
export function isCommentModeratorRole(role: string | null): boolean {
	return role === 'owner' || role === 'admin';
}
