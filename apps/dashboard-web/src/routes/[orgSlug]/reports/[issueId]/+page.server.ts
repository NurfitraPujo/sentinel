import { error } from '@sveltejs/kit';
import { eq } from 'drizzle-orm';
import type { PageServerLoad } from './$types';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';
import { getReportDetail, getIssueActivity, listIssueAttachments } from '$lib/db/queries/reports';
import { getIssueRelations } from '$lib/db/queries/issues';
import { requireReportAccessForIssue } from '$lib/server/report-access';
import { ISSUE_WRITE_ROLES } from '$lib/server/issue-access';
import type { Escalation, Triage } from '$lib/types/report-detail';

// Manual Issues M1 (design §10): imitates the project-issue detail loader pattern at
// [orgSlug]/projects/[projectId]/issues/[issueId]/+page.server.ts -- verify the issue belongs to
// this org (report-access.ts's requireReportAccessForIssue does both the 404-for-wrong-type and
// the org-membership check in one call, see B7/§9), then load everything the page renders.
export const load: PageServerLoad = async ({ params, fetch, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { issueId, orgSlug } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	const userId = session.user.id;
	const { organizationId, role } = await requireReportAccessForIssue(userId, issueId, 'read');

	// requireReportAccessForIssue resolves organizationId from the issue itself, but the route is
	// also parameterized by :orgSlug -- reject a mismatch rather than silently serving the report
	// under whichever org slug happens to be in the URL (mirrors the projectId cross-check on the
	// existing issue detail loader).
	const currentOrg = locals.currentOrg;
	if (!currentOrg || currentOrg.slug !== orgSlug || currentOrg.id !== organizationId) {
		throw error(403, 'Forbidden: Unauthorized access to organization');
	}

	const detail = await getReportDetail(issueId);
	if (!detail) {
		throw error(404, 'Report not found');
	}

	const [relations, activity, orgProjects, attachments] = await Promise.all([
		getIssueRelations(issueId),
		getIssueActivity(issueId),
		db
			.select({ id: projects.id, name: projects.name, isInbox: projects.isInbox })
			.from(projects)
			.where(eq(projects.organizationId, organizationId)),
		listIssueAttachments(issueId),
	]);

	const canWrite = (ISSUE_WRITE_ROLES as readonly string[]).includes(role);

	// sentinel-user-report-issue §4.2's `triage` and `escalation`, fetched from the API route that
	// already computes them rather than recomputed here. That route owns the window derivation, the
	// `linked` read-back and the degrade-to-null rules; a second implementation in this loader is
	// exactly the drift §4.2 warns about, and the two would disagree the first time either changed.
	//
	// A failure here is `null` for both, not a 500: `null` is a state the page already renders, and
	// a report is worth showing even when the matcher or the escalation lookup could not answer.
	const { triage, escalation } = await loadTriageAndEscalation(fetch, organizationId, issueId);

	// R11 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md, §9): the author may edit/delete their own
	// report until it is resolved; an org owner/admin may delete (not edit -- PATCH is strictly
	// author-only) any report at any time. Computed here, not just re-derived client-side, since
	// the route itself re-checks on every PATCH/DELETE regardless -- this only controls whether the
	// UI offers the buttons.
	// sentinel-user-report-issue §4.2: `detail.report` is the ORG-MEMBER companion and is now null
	// for an externally-submitted report (which has an `external_issue_reports` row instead), so
	// this must not dereference it. An external report has no org-member author, which is the
	// correct authorization answer as well as the safe one: nobody is its author, so nobody gets
	// the author's edit/delete affordance. Rendering the external reporter is fe.md's.
	const isAuthor = detail.report?.reporterId === userId;
	const isResolved = detail.issue.status === 'resolved';
	const isModerator = role === 'owner' || role === 'admin';
	const canEditOwnReport = isAuthor && !isResolved;
	const canDeleteReport = (isAuthor && !isResolved) || isModerator;

	return {
		orgId: organizationId,
		orgSlug,
		userId,
		userRole: role,
		canWrite,
		canEditOwnReport,
		canDeleteReport,
		detail,
		relations,
		activity,
		attachments,
		projects: orgProjects,
		triage,
		escalation,
	};
};

/**
 * §4.2's two computed objects, read back through `GET /api/organizations/{orgId}/reports/{issueId}`.
 *
 * SvelteKit's server-side `fetch` forwards this request's cookies, so the endpoint re-runs its own
 * `requireReportAccess` gate against the same session -- this loader is not widening anything it
 * has not already checked above.
 */
async function loadTriageAndEscalation(
	fetch: typeof globalThis.fetch,
	organizationId: string,
	issueId: string
): Promise<{ triage: Triage | null; escalation: Escalation | null }> {
	try {
		const res = await fetch(`/api/organizations/${organizationId}/reports/${issueId}`);
		if (!res.ok) {
			return { triage: null, escalation: null };
		}
		const body = await res.json();
		return { triage: body.triage ?? null, escalation: body.escalation ?? null };
	} catch {
		return { triage: null, escalation: null };
	}
}
