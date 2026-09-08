import { error } from '@sveltejs/kit';
import { eq } from 'drizzle-orm';
import { db } from './db';
import { issues, projects } from '$lib/db/schema';
import { requireIssueAccess } from './issue-access';
import { requireReportAccessForIssue, getOrgRole, type ReportAction } from './report-access';
import { resolveEnvironmentScope } from './environment-access';

/**
 * R17 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): the single per-issue-type access dispatch,
 * extracted from three call sites that had each grown their own copy --
 * routes/api/attachments/[id]/+server.ts's GET (read-only), comment-access.ts's read/write
 * pair, and R4's local `requireClaimAccess` in routes/api/issues/[issueId]/claim/+server.ts.
 * `user_report` issues go through `requireReportAccessForIssue` (§9's viewer carve-out);
 * `system_error` issues go through the existing `requireIssueAccess` (D10/D17), with the org role
 * additionally resolved so callers that need it (delete-others-comments, force-release) don't
 * have to look it up a second time.
 *
 * sentinel-project-environments §2.2 names THIS function -- not `report-access.ts:
 * requireReportAccessForIssue` -- as the environment guard point for reports, comments and
 * attachments, and the reason is the dispatch below: only the function that switches on
 * `issue_type` covers both branches by construction. `requireIssueAccess` carries the same
 * predicate for its own §2.2 row (`/api/issues/**` writes), but the dispatch reaches it on the
 * `system_error` branch ONLY, so a guard placed there alone leaves every `user_report` -- which is
 * every row §4.8a writes the literal `production` on -- unguarded. The predicate is therefore
 * applied here, before the dispatch, over the issue row this function already loads.
 */

export type IssueTypeAccessAction = 'read' | 'comment' | 'write' | 'force-release';

export interface IssueTypeAccessResult {
	issueId: string;
	issueType: string;
	organizationId: string;
	/** Org role, when resolvable (always resolvable once access is granted). */
	role: string | null;
}

async function loadIssueTypeContext(issueId: string): Promise<{
	issueType: string;
	organizationId: string;
	projectId: string;
	environment: string;
}> {
	const rows = await db
		.select({
			issueType: issues.issueType,
			organizationId: projects.organizationId,
			// §2.2's predicate is a single-column one on `issues` (decision 7), so the environment
			// dimension costs this query two more columns and no extra round trip.
			projectId: issues.projectId,
			environment: issues.environment,
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

	return {
		issueType: rows[0].issueType,
		organizationId: rows[0].organizationId,
		projectId: rows[0].projectId,
		environment: rows[0].environment,
	};
}

/**
 * Resolves access to `issueId` at `action`, dispatching per issue_type, and throws (401/403/404)
 * on failure exactly like the helpers it wraps. `force-release` has no `system_error`-specific
 * equivalent in report-access.ts's matrix, so it borrows the report side's rule (owner/admin
 * only) rather than inventing a third one -- same rationale R4 already established locally.
 */
export async function requireIssueAccessAnyType(
	userId: string,
	issueId: string,
	action: IssueTypeAccessAction
): Promise<IssueTypeAccessResult> {
	const { issueType, organizationId, projectId, environment } =
		await loadIssueTypeContext(issueId);

	// sentinel-project-environments §2.2 -- the ENVIRONMENT dimension, applied BEFORE the dispatch
	// so both branches and every caller carry it.
	//
	// Denial is `404`, never `403` (decision 20): an issue in an environment the caller may not see
	// is indistinguishable from one that does not exist, and a `403` on a report id tells a
	// contractor that a production report with that id exists. This does not decide whether the
	// caller may reach the project or the organization at all -- the helpers dispatched to below
	// still do that, and keep their own `403`s.
	const scope = await resolveEnvironmentScope(userId, projectId);
	if (scope !== null && !scope.includes(environment)) {
		throw error(404, 'Issue not found');
	}

	if (issueType === 'user_report') {
		// 'read'|'comment'|'write'|'force-release' are all valid ReportAction values already.
		const { role } = await requireReportAccessForIssue(userId, issueId, action as ReportAction);
		return { issueId, issueType, organizationId, role };
	}

	// system_error: requireIssueAccess only knows 'read'|'write'. 'comment' maps to 'read' (§9:
	// commenting requires only read access to the issue); 'force-release' maps to 'write' plus an
	// additional owner/admin gate below.
	const issueAccessPermission = action === 'write' || action === 'force-release' ? 'write' : 'read';
	const { organizationId: orgId } = await requireIssueAccess(userId, issueId, issueAccessPermission);
	const role = await getOrgRole(userId, orgId);

	if (action === 'force-release' && role !== 'owner' && role !== 'admin') {
		throw error(403, 'Forbidden: Only an org owner or admin may force-release a claim');
	}

	return { issueId, issueType, organizationId: orgId, role };
}
