import { error } from '@sveltejs/kit';
import { eq } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { issues, projects } from '$lib/db/schema';
import { getReporterId, type IssueReporter } from '$lib/server/report-access';

/**
 * Manual Issues M5 stage 2 (design §7): resolves an issue for an agent-authenticated request and
 * enforces org scoping FROM THE CREDENTIAL (B7) -- every `/api/agent/issues/[issueId]/*` route
 * calls this with `ctx.organizationId` from `authenticateAgentRequest`, never anything the
 * request body claims. Deliberately NOT `report-access.ts` or `issue-access.ts`: the work-loop
 * spans BOTH issue types (design §7 "spans both issue types; this is the one deliberate bridge
 * across Q9's separation"), so this is its own minimal resolver rather than a wrapper around
 * either issue-type-specific gate.
 */
export interface AgentIssueScope {
	issueId: string;
	projectId: string;
	organizationId: string;
	issueType: string;
	assignedTo: string | null;
	assigneeType: string | null;
	waitingOn: string | null;
	/**
	 * A11 (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7f): surfaced alongside `assignedTo`
	 * so callers building claim-conflict/claimed-issue context (`{claimedBy, claimedAt}`) don't
	 * need a second query -- this is the SAME `issues.claimed_at` column N7c (A03) added for the
	 * stale-claim reaper.
	 */
	claimedAt: Date | null;
}

export async function resolveAgentIssueScope(issueId: string, organizationId: string): Promise<AgentIssueScope> {
	const rows = await db
		.select({
			issueId: issues.id,
			projectId: issues.projectId,
			organizationId: projects.organizationId,
			issueType: issues.issueType,
			assignedTo: issues.assignedTo,
			assigneeType: issues.assigneeType,
			waitingOn: issues.waitingOn,
			claimedAt: issues.claimedAt,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(eq(issues.id, issueId));

	const row = rows[0];
	// Deliberately indistinguishable from "belongs to another organization" (404 both ways) -- an
	// agent key must not be able to enumerate other tenants' issue ids by status-code probing,
	// same rationale as ResolveProjectInOrg in apps/ingestor-go/auth/apikey.go.
	if (!row || row.organizationId !== organizationId) {
		throw error(404, 'Issue not found');
	}

	return row as AgentIssueScope;
}

/**
 * Reporter for a `user_report` issue, or null if it isn't one -- used to resolve
 * `audience: 'reporter'`.
 *
 * sentinel-user-report-issue SR-6: this read `manual_issue_reports` alone and so returned `null`
 * for every externally-submitted report, which is the same answer it gives for "this is not a
 * report at all". It now returns the reporter, tagged with its kind, and delegates to
 * `report-access.ts`'s `getReporterId` rather than growing a second copy of the two-companion
 * lookup -- the two were the same query with the same defect and must not drift back apart.
 *
 * An `audience: 'reporter'` fan-out still reaches nobody for an external report, and deliberately:
 * §1 fixes that there is no reply channel to an external reporter and §2 that they are never a
 * Sentinel principal. The difference this makes is that a caller can now SEE that, instead of
 * reading a `null` that means "no such report".
 */
export async function getIssueReporterId(issueId: string): Promise<IssueReporter | null> {
	return await getReporterId(issueId);
}
