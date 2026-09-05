import { db } from '$lib/server/db';
import {
	issues,
	projects,
	manualIssueReports,
	errorOccurrences,
	externalIssueReports,
	externalReporters,
} from '$lib/db/schema';
import { and, desc, eq, lt } from 'drizzle-orm';
import { getAgentSettingsForProjects, type RepoConnectionRow } from '$lib/db/queries/agent-settings';

/**
 * N1c (agent read endpoints). Own module per FILE OWNERSHIP -- queries backing
 * `GET /api/agent/issues/[issueId]`, `.../occurrences`, and `GET /api/agent/projects`. Reuses
 * `getIssueRelations` (queries/issues.ts) and `resolveAgentIssueScope` (agent-route.ts's
 * `withAgentIssue`) rather than duplicating org-scoping logic -- B7 still applies: every query
 * here that needs org scope takes `organizationId` as an explicit argument sourced from
 * `AgentAuthContext`, never from a request param.
 */

export interface AgentIssueDetail {
	id: string;
	projectId: string;
	fingerprint: string;
	message: string;
	errorClass: string;
	status: string;
	regressionStatus: string;
	issueType: string;
	sourceChannel: string;
	assigneeType: string | null;
	assignedTo: string | null;
	/** N7e (A07): when the CURRENT claim was made -- null if unclaimed. Already present on the raw
	 *  row (`db.select().from(issues)` is a full-row select); this only widens the declared type. */
	claimedAt: Date | null;
	resolvedInVersion: string | null;
	resolvedAt: Date | null;
	resolvedByType: string | null;
	resolvedBy: string | null;
	regressionCount: number;
	lastRegressedAt: Date | null;
	firstSeen: Date | null;
	lastSeen: Date | null;
	count: number;
	waitingOn: string | null;
}

/** Full issue row for the agent detail response. Scope (org membership) is already enforced by
 * `withAgentIssue` resolving `issueId` before this is called -- this just fetches the row. */
export async function getAgentIssueDetail(issueId: string): Promise<AgentIssueDetail | null> {
	const rows = await db.select().from(issues).where(eq(issues.id, issueId));
	return (rows[0] as AgentIssueDetail) ?? null;
}

export interface AgentReportDetail {
	bodyMd: string;
	severity: string;
	/** The reporting `"user".id`, and `null` for an external report -- an external reporter is DATA,
	 *  never a Sentinel principal (§2), and has no row in the user id space. `reporterKind` is what
	 *  a caller branches on; this field stays a user id or nothing. */
	reporterId: string | null;
	/** sentinel-user-report-issue §4.2 / SR-6. */
	reporterKind: 'org_member' | 'external';
	/** `external_reporters.id` for an external report, `null` for an org-member one. Never
	 *  interchangeable with `reporterId`. */
	externalReporterId: string | null;
	reporterDisplayName: string | null;
	/** Stored, and never a reply channel (MANUAL_ISSUES_DESIGN §0 Q1). */
	reporterEmail: string | null;
	/** The service's own opaque user id, for an external report. Never resolved, never trusted as
	 *  identity (§2). */
	reporterExternalId: string | null;
	/** `external_issue_reports.context` verbatim, or `null` for an org-member report. */
	context: Record<string, unknown> | null;
}

/**
 * `user_report` issues only -- returns null for `system_error` issues (no companion row).
 *
 * SR-6 / §4.2's correction: this used to read `manual_issue_reports` alone, so an externally
 * submitted report -- which has no row there -- came back `null` and an agent saw a title and
 * nothing else. Both companions are read; exactly one of them exists for any `user_report` issue.
 */
export async function getAgentReportDetail(issueId: string): Promise<AgentReportDetail | null> {
	const manualRows = await db
		.select({
			bodyMd: manualIssueReports.bodyMd,
			severity: manualIssueReports.severity,
			reporterId: manualIssueReports.reporterId,
		})
		.from(manualIssueReports)
		.where(eq(manualIssueReports.issueId, issueId));

	const manual = manualRows[0];
	if (manual) {
		return {
			bodyMd: manual.bodyMd,
			severity: manual.severity,
			reporterId: manual.reporterId,
			reporterKind: 'org_member',
			externalReporterId: null,
			reporterDisplayName: null,
			reporterEmail: null,
			reporterExternalId: null,
			context: null,
		};
	}

	const externalRows = await db
		.select({
			bodyMd: externalIssueReports.bodyMd,
			severity: externalIssueReports.severity,
			context: externalIssueReports.context,
			externalReporterId: externalReporters.id,
			displayName: externalReporters.displayName,
			email: externalReporters.email,
			externalId: externalReporters.externalId,
		})
		.from(externalIssueReports)
		.innerJoin(externalReporters, eq(externalReporters.id, externalIssueReports.externalReporterId))
		.where(eq(externalIssueReports.issueId, issueId));

	const external = externalRows[0];
	if (!external) {
		return null;
	}

	return {
		bodyMd: external.bodyMd,
		severity: external.severity,
		reporterId: null,
		reporterKind: 'external',
		externalReporterId: external.externalReporterId,
		reporterDisplayName: external.displayName,
		reporterEmail: external.email,
		reporterExternalId: external.externalId,
		context: (external.context ?? null) as Record<string, unknown> | null,
	};
}

export interface AgentOccurrence {
	id: string;
	environment: string;
	platform: string;
	releaseVersion: string | null;
	stacktrace: unknown;
	metadata: unknown;
	traceId: string | null;
	createdAt: Date | null;
}

/** `system_error` issues only -- most recent occurrence, or null if none exist. */
export async function getLatestAgentOccurrence(issueId: string): Promise<AgentOccurrence | null> {
	const rows = await db
		.select({
			id: errorOccurrences.id,
			environment: errorOccurrences.environment,
			platform: errorOccurrences.platform,
			releaseVersion: errorOccurrences.releaseVersion,
			stacktrace: errorOccurrences.stacktrace,
			metadata: errorOccurrences.metadata,
			traceId: errorOccurrences.traceId,
			createdAt: errorOccurrences.createdAt,
		})
		.from(errorOccurrences)
		.where(eq(errorOccurrences.issueId, issueId))
		.orderBy(desc(errorOccurrences.createdAt))
		.limit(1);
	return rows[0] ?? null;
}

export interface ListAgentOccurrencesOptions {
	issueId: string;
	limit?: number;
	before?: Date;
}

const OCCURRENCES_DEFAULT_LIMIT = 20;
const OCCURRENCES_MAX_LIMIT = 50;

/** Newest-first page of occurrences for an issue. `limit` is clamped to [1, 50]; `before` is an
 * exclusive `createdAt` cursor for paging further back. */
export async function listAgentOccurrences(options: ListAgentOccurrencesOptions): Promise<AgentOccurrence[]> {
	const limit = Math.min(Math.max(options.limit ?? OCCURRENCES_DEFAULT_LIMIT, 1), OCCURRENCES_MAX_LIMIT);

	const conditions = [eq(errorOccurrences.issueId, options.issueId)];
	if (options.before) {
		conditions.push(lt(errorOccurrences.createdAt, options.before));
	}

	const rows = await db
		.select({
			id: errorOccurrences.id,
			environment: errorOccurrences.environment,
			platform: errorOccurrences.platform,
			releaseVersion: errorOccurrences.releaseVersion,
			stacktrace: errorOccurrences.stacktrace,
			metadata: errorOccurrences.metadata,
			traceId: errorOccurrences.traceId,
			createdAt: errorOccurrences.createdAt,
		})
		.from(errorOccurrences)
		.where(and(...conditions))
		.orderBy(desc(errorOccurrences.createdAt))
		.limit(limit);

	return rows;
}

export interface AgentProjectRepo {
	provider: string;
	owner: string;
	repo: string;
	defaultBranch: string;
	testCmd: string;
	agentCmd: string | null;
	cloneDepth: number | null;
}

export interface AgentProjectAgentSettings {
	fixEnabled: boolean;
	maxPrsPerDay: number | null;
	repo: AgentProjectRepo | null;
}

export interface AgentProject {
	id: string;
	name: string;
	isInbox: boolean;
	agentSettings: AgentProjectAgentSettings;
}

function toAgentProjectRepo(row: RepoConnectionRow): AgentProjectRepo {
	return {
		provider: row.provider,
		owner: row.owner,
		repo: row.repo,
		defaultBranch: row.defaultBranch,
		testCmd: row.testCmd,
		agentCmd: row.agentCmd,
		cloneDepth: row.cloneDepth,
	};
}

/**
 * Org's projects (B7: `organizationId` MUST come from `AgentAuthContext`, never a request param).
 * Each project carries its full `agentSettings` (N10 part 1, DECISIONS.md D23), including the
 * repo connection's `testCmd`/`agentCmd` -- agents get the full connection deliberately (see
 * D23). Uses the batch read `getAgentSettingsForProjects` to avoid N+1 queries.
 */
export async function listAgentProjects(organizationId: string): Promise<AgentProject[]> {
	const rows = await db
		.select({
			id: projects.id,
			name: projects.name,
			isInbox: projects.isInbox,
		})
		.from(projects)
		.where(eq(projects.organizationId, organizationId))
		.orderBy(projects.name);

	const settingsMap = await getAgentSettingsForProjects(rows.map((row) => row.id));

	return rows.map((row) => {
		const settings = settingsMap.get(row.id) ?? { fixEnabled: false, maxPrsPerDay: null, repo: null };
		return {
			...row,
			agentSettings: {
				fixEnabled: settings.fixEnabled,
				maxPrsPerDay: settings.maxPrsPerDay,
				repo: settings.repo ? toAgentProjectRepo(settings.repo) : null,
			},
		};
	});
}
