import { error } from '@sveltejs/kit';
import { and, eq } from 'drizzle-orm';
import { db } from './db';
import {
	issues,
	projects,
	organizationMembers,
	manualIssueReports,
	externalIssueReports,
	externalReporters,
} from '$lib/db/schema';
import { ISSUE_WRITE_ROLES } from './issue-access';

/**
 * Manual Issues M1 (docs/plans/MANUAL_ISSUES_DESIGN.md §9). A deliberate sibling of
 * `issue-access.ts`, not a wrapper around it -- the permission matrix here is genuinely
 * different (viewers may create and comment, which `requireIssueAccess`'s org-role gate would
 * refuse), so folding the two together would either loosen `requireIssueAccess`'s guarantees for
 * error issues or force manual reports through a gate that wrongly blocks viewers.
 *
 * | Action                                              | Who                                    |
 * |------------------------------------------------------|-----------------------------------------|
 * | create / read / comment                               | any org member, including `viewer`      |
 * | claim / release (own) / resolve / link / move / edit others' reports | `ISSUE_WRITE_ROLES` (owner,admin,engineer,support) + agents |
 * | force-release a claim                                 | `owner`, `admin`                        |
 */
export type ReportAction = 'create' | 'read' | 'comment' | 'write' | 'force-release';

export interface ReportAccessContext {
	organizationId: string;
	role: string;
}

/**
 * Resolves the caller's org role for `organizationId` and throws a SvelteKit `error()`
 * (401/403) unless it satisfies `action`. Callers pass `organizationId` directly when creating a
 * report (no issue exists yet); `requireReportAccessForIssue` below resolves it from an issue id
 * first, for every other action.
 */
export async function requireReportAccess(
	userId: string,
	organizationId: string,
	action: ReportAction
): Promise<ReportAccessContext> {
	const memberRows = await db
		.select({ role: organizationMembers.role })
		.from(organizationMembers)
		.where(
			and(
				eq(organizationMembers.userId, userId),
				eq(organizationMembers.organizationId, organizationId)
			)
		);

	if (memberRows.length === 0) {
		throw error(403, 'Forbidden: You do not have access to this organization');
	}

	const role = memberRows[0].role;
	assertActionAllowed(role, action);

	return { organizationId, role };
}

/**
 * Same check as `requireReportAccess`, but resolves `organizationId` from an existing manual
 * issue id first -- the shape every route past creation needs (claim/move/detail/activity all
 * start from an issueId, not an orgId). Throws 404 if the id does not name a `user_report` issue
 * at all -- this deliberately also 404s a `system_error` issue id, so this helper can never be
 * used to backdoor access onto the error-dashboard side (§9 strict separation).
 */
export async function requireReportAccessForIssue(
	userId: string,
	issueId: string,
	action: ReportAction
): Promise<ReportAccessContext & { issueId: string; projectId: string }> {
	const rows = await db
		.select({
			issueId: issues.id,
			projectId: issues.projectId,
			organizationId: projects.organizationId,
			issueType: issues.issueType,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(eq(issues.id, issueId));

	if (rows.length === 0 || rows[0].issueType !== 'user_report') {
		throw error(404, 'Report not found');
	}

	const { projectId, organizationId } = rows[0];
	if (!organizationId) {
		throw error(400, 'Issue project does not belong to an organization');
	}

	const { role } = await requireReportAccess(userId, organizationId, action);

	return { issueId: rows[0].issueId, projectId, organizationId, role };
}

function isWriteRole(role: string): boolean {
	return (ISSUE_WRITE_ROLES as readonly string[]).includes(role);
}

function isForceReleaseRole(role: string): boolean {
	return role === 'owner' || role === 'admin';
}

function assertActionAllowed(role: string, action: ReportAction): void {
	switch (action) {
		case 'create':
		case 'read':
		case 'comment':
			// Any recognized org member, including viewer (§9 Q8).
			return;
		case 'write':
			if (!isWriteRole(role)) {
				throw error(403, 'Forbidden: Insufficient permissions for this report action');
			}
			return;
		case 'force-release':
			if (!isForceReleaseRole(role)) {
				throw error(403, 'Forbidden: Only an org owner or admin may force-release a claim');
			}
			return;
	}
}

/**
 * §9: the author of their own report, or edit others (a write-role/agent). Callers pass the
 * report's `reporterId` (from `manual_issue_reports.reporter_id`) and the acting user's id.
 * Resolution is separated from `assertActionAllowed` because "own vs. others" is a fact about
 * the specific report, not something derivable from role alone.
 */
export function canEditReport(role: string, reporterId: string, actorId: string): boolean {
	return reporterId === actorId || isWriteRole(role);
}

/**
 * M3 (comment-access.ts): the caller's org role, or null if they aren't a member at all. A
 * standalone read (no throw) for callers that need to branch on the role itself (e.g. "is this
 * an owner/admin, for the delete-others-comments carve-out") rather than get an allow/deny
 * verdict the way `requireReportAccess` does.
 */
export async function getOrgRole(userId: string, organizationId: string): Promise<string | null> {
	const rows = await db
		.select({ role: organizationMembers.role })
		.from(organizationMembers)
		.where(
			and(
				eq(organizationMembers.userId, userId),
				eq(organizationMembers.organizationId, organizationId)
			)
		);

	return rows[0]?.role ?? null;
}

/**
 * sentinel-user-report-issue §4.2 / SR-6: who reported an issue, without pulling in the full
 * report-detail query.
 *
 * **The return is a tagged union, not a bare id, and that is the point.** This used to return
 * `manual_issue_reports.reporter_id` -- a `"user".id` -- or `null`, and every caller compared it
 * to a session user id. An externally-submitted report has no `manual_issue_reports` row, so it
 * came back `null`; returning `external_reporters.id` in its place would have been worse, because
 * that id lives in a DIFFERENT id space and a caller comparing it to a session user id would be
 * asking a question with no meaning. The tag makes "is this caller the author?" answerable only
 * after the caller has established WHICH KIND of reporter it is looking at.
 *
 * An external report has no org-member author, so no session user is ever its author -- which is
 * exactly the authorization answer §2 wants: an external reporter is data, never a principal.
 */
export type IssueReporter =
	| { kind: 'org_member'; userId: string }
	| {
			kind: 'external';
			externalReporterId: string;
			externalId: string;
			displayName: string | null;
			email: string | null;
			firstSeenAt: Date;
	  };

export async function getReporterId(issueId: string): Promise<IssueReporter | null> {
	const manualRows = await db
		.select({ reporterId: manualIssueReports.reporterId })
		.from(manualIssueReports)
		.where(eq(manualIssueReports.issueId, issueId));

	const manual = manualRows[0];
	if (manual) {
		return { kind: 'org_member', userId: manual.reporterId };
	}

	const externalRows = await db
		.select({
			externalReporterId: externalReporters.id,
			externalId: externalReporters.externalId,
			displayName: externalReporters.displayName,
			email: externalReporters.email,
			firstSeenAt: externalReporters.firstSeenAt,
		})
		.from(externalIssueReports)
		.innerJoin(externalReporters, eq(externalReporters.id, externalIssueReports.externalReporterId))
		.where(eq(externalIssueReports.issueId, issueId));

	const external = externalRows[0];
	if (!external) {
		return null;
	}

	return { kind: 'external', ...external };
}

/** The org-member author's `"user".id`, or `null` when the report has none (external, or no
 *  companion at all). The narrow question the PATCH/DELETE author gate actually asks. */
export async function getAuthorUserId(issueId: string): Promise<string | null> {
	const reporter = await getReporterId(issueId);
	return reporter?.kind === 'org_member' ? reporter.userId : null;
}
