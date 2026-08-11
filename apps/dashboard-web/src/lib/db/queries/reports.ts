import crypto from 'crypto';
import { db } from '$lib/server/db';
import {
	issues,
	issueActivity,
	manualIssueReports,
	projects,
	users,
} from '$lib/db/schema';
import { eq, and, isNull, desc, sql } from 'drizzle-orm';

/**
 * Manual Issues M1 (docs/plans/MANUAL_ISSUES_DESIGN.md §2, §6, §9, §10). Every write here
 * follows D18 (throw inside db.transaction, never return early to signal failure) and writes
 * exactly one issue_activity row in the same transaction as the mutation it describes.
 */

export type ReportSeverity = 'low' | 'medium' | 'high' | 'critical';

/**
 * §2, Q12: find-or-create the per-org Triage inbox project, lazily, on first use. No backfill
 * migration -- an org with no manual reports yet simply has no Triage project until one is
 * needed. Called from inside createManualIssue's transaction when the caller omits a projectId,
 * so the provisioning and the report creation are atomic together.
 *
 * Looks up by (organizationId, isInbox=true) rather than by name -- §2 is explicit that the
 * inbox is marked by the durable `is_inbox` column, never a name convention, so a user
 * renaming "Triage" must not break this lookup or cause a second Triage project to appear.
 */
async function findOrCreateTriageProject(tx: any, organizationId: string): Promise<string> {
	const existing = await tx
		.select({ id: projects.id })
		.from(projects)
		.where(and(eq(projects.organizationId, organizationId), eq(projects.isInbox, true)));

	if (existing.length > 0) {
		return existing[0].id;
	}

	// Inbox projects get no API key (§2) -- apiKey/apiKeyHash are NOT NULL columns, so this
	// stores inert placeholder values rather than a usable credential. Nothing in the ingestion
	// path (auth/apikey.go) can ever look these up, since it only ever queries by hash of a real
	// SDK-issued key, and this project is never handed a real one.
	// `projects.api_key` is `varchar(64)` (schema.ts). `inbox_` (6 chars) + hex(randomBytes(n)) must
	// fit inside that, so n <= 29 -- 24 bytes (48 hex chars, 54 total) leaves headroom while keeping
	// plenty of entropy for a value nothing ever looks up by. `randomBytes(32)` here silently
	// exceeded the column (70 chars) and every first-use Triage provisioning threw
	// `value too long for type character varying(64)` -- caught by the M1 end-to-end flow test
	// (reports.e2e-flow.integration.test.ts) against a real migrated Postgres, not by any mock-based
	// unit test, because mocked `db.transaction` calls never round-trip through Postgres's column
	// typing.
	const placeholder = crypto.randomBytes(24).toString('hex');
	const [created] = await tx
		.insert(projects)
		.values({
			organizationId,
			name: 'Triage',
			apiKey: `inbox_${placeholder}`,
			apiKeyHash: crypto.createHash('sha256').update(placeholder).digest('hex'),
			isInbox: true,
		})
		.returning({ id: projects.id });

	return created.id;
}

export interface CreateManualIssueInput {
	organizationId: string;
	projectId: string | null;
	reporterId: string;
	title: string;
	bodyMd: string;
	severity: ReportSeverity;
	sourceChannel?: 'manual_support' | 'api';
}

/**
 * §2: a manual issue IS an `issues` row (issue_type='user_report') plus a 1:1
 * `manual_issue_reports` companion, created together with a `report_created`-shaped activity
 * entry. `fingerprint` is a random UUID hex -- manual issues are never deduped by fingerprint,
 * only `UNIQUE(project_id, fingerprint)` needs satisfying. `error_class` is the fixed literal
 * `'user_report'`; `message` mirrors the report title so the existing issues list/search
 * columns render something sensible if ever viewed unfiltered.
 *
 * D18: throws instead of returning early from inside the transaction callback -- a partial
 * failure (e.g. the report insert violating a CHECK) must roll back the issues insert and the
 * Triage provisioning together, not commit a half-created report.
 */
export async function createManualIssue(input: CreateManualIssueInput) {
	const title = input.title.trim();
	if (title.length === 0) {
		throw new Error('title must not be empty');
	}

	return await db.transaction(async (tx) => {
		const projectId = input.projectId ?? (await findOrCreateTriageProject(tx, input.organizationId));

		const fingerprint = crypto.randomBytes(16).toString('hex');

		const [issue] = await tx
			.insert(issues)
			.values({
				projectId,
				fingerprint,
				message: title,
				errorClass: 'user_report',
				issueType: 'user_report',
				sourceChannel: input.sourceChannel ?? 'manual_support',
			})
			.returning();

		if (!issue) {
			throw new Error('Failed to create issue row for manual report');
		}

		const [report] = await tx
			.insert(manualIssueReports)
			.values({
				issueId: issue.id,
				reporterId: input.reporterId,
				bodyMd: input.bodyMd,
				severity: input.severity,
			})
			.returning();

		await tx.insert(issueActivity).values({
			issueId: issue.id,
			eventType: 'report_edited',
			actorType: 'user',
			actorId: input.reporterId,
			newValue: { action: 'created', title, severity: input.severity, projectId },
		});

		return { issue, report };
	});
}

export type ReportTab = 'all' | 'mine' | 'claimed-by-me' | 'unclaimed' | 'needs-input' | 'triage';

export interface ListReportsOptions {
	organizationId: string;
	tab: ReportTab;
	userId: string;
	/** Restrict to a set of project ids the caller may see (viewer scoping mirrors issues.ts). */
	accessibleProjectIds?: Set<string> | null;
}

/**
 * §10: tabs All / My reports / Claimed by me / Unclaimed / Needs input / Triage. All tabs are
 * scoped to `issue_type='user_report'` (§9 strict separation is the whole point of this table
 * existing alongside issues.ts's error-dashboard queries, not a shared listing).
 */
export async function listReports({ organizationId, tab, userId, accessibleProjectIds }: ListReportsOptions) {
	const conditions = [eq(projects.organizationId, organizationId), eq(issues.issueType, 'user_report')];

	switch (tab) {
		case 'mine':
			conditions.push(eq(manualIssueReports.reporterId, userId));
			break;
		case 'claimed-by-me':
			conditions.push(eq(issues.assignedTo, userId));
			conditions.push(eq(issues.assigneeType, 'user'));
			break;
		case 'unclaimed':
			conditions.push(isNull(issues.assignedTo));
			break;
		case 'needs-input':
			conditions.push(sql`${issues.waitingOn} IS NOT NULL`);
			break;
		case 'triage':
			conditions.push(eq(projects.isInbox, true));
			break;
		case 'all':
		default:
			break;
	}

	if (accessibleProjectIds) {
		if (accessibleProjectIds.size === 0) {
			return [];
		}
		conditions.push(sql`${issues.projectId} IN (${sql.join(
			[...accessibleProjectIds].map((id) => sql`${id}`),
			sql`, `
		)})`);
	}

	return await db
		.select({
			issue: issues,
			report: manualIssueReports,
			projectName: projects.name,
			projectIsInbox: projects.isInbox,
			reporterName: users.name,
			reporterEmail: users.email,
		})
		.from(issues)
		.innerJoin(manualIssueReports, eq(manualIssueReports.issueId, issues.id))
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.leftJoin(users, eq(users.id, manualIssueReports.reporterId))
		.where(and(...conditions))
		.orderBy(desc(issues.firstSeen));
}

/**
 * §10, single-report detail view: the issue, its manual_issue_reports companion, project, and
 * reporter, in one row. Returns null if the issue does not exist or is not a manual report
 * (callers should treat both the same -- 404, never leak a system_error issue through this path).
 */
export async function getReportDetail(issueId: string) {
	const rows = await db
		.select({
			issue: issues,
			report: manualIssueReports,
			projectId: projects.id,
			projectName: projects.name,
			projectIsInbox: projects.isInbox,
			organizationId: projects.organizationId,
			reporterName: users.name,
			reporterEmail: users.email,
		})
		.from(issues)
		.innerJoin(manualIssueReports, eq(manualIssueReports.issueId, issues.id))
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.leftJoin(users, eq(users.id, manualIssueReports.reporterId))
		.where(and(eq(issues.id, issueId), eq(issues.issueType, 'user_report')));

	return rows[0] ?? null;
}

/**
 * §2/§10 write-role "move to project" action: re-homes a manual issue and logs a `moved`
 * activity entry with old/new project ids. D18: throw, don't return, on a bad target.
 */
export async function moveIssueToProject(
	issueId: string,
	targetProjectId: string,
	actorType: 'user' | 'agent' | 'system',
	actorId: string
) {
	return await db.transaction(async (tx) => {
		const [existing] = await tx
			.select({ projectId: issues.projectId })
			.from(issues)
			.where(eq(issues.id, issueId));

		if (!existing) {
			throw new Error(`Issue ${issueId} not found`);
		}

		const [targetProject] = await tx
			.select({ id: projects.id })
			.from(projects)
			.where(eq(projects.id, targetProjectId));

		if (!targetProject) {
			throw new Error(`Project ${targetProjectId} not found`);
		}

		await tx.update(issues).set({ projectId: targetProjectId }).where(eq(issues.id, issueId));

		await tx.insert(issueActivity).values({
			issueId,
			eventType: 'moved',
			actorType,
			actorId,
			oldValue: { projectId: existing.projectId },
			newValue: { projectId: targetProjectId },
		});

		return { issueId, fromProjectId: existing.projectId, toProjectId: targetProjectId };
	});
}

export class ClaimConflictError extends Error {
	constructor(message = 'Issue is already claimed') {
		super(message);
		this.name = 'ClaimConflictError';
	}
}

/**
 * §7/§9: atomic conditional claim -- `WHERE assigned_to IS NULL`, 0 rows updated means someone
 * else won the race, and this throws `ClaimConflictError` rather than silently no-op'ing (D18:
 * the caller must be able to distinguish "I claimed it" from "I did nothing"). Works for both
 * users and agents; `actorType` records which.
 */
export async function claimIssue(
	issueId: string,
	actorType: 'user' | 'agent',
	actorId: string
) {
	return await db.transaction(async (tx) => {
		const updated = await tx
			.update(issues)
			.set({ assigneeType: actorType, assignedTo: actorId })
			.where(and(eq(issues.id, issueId), isNull(issues.assignedTo)))
			.returning();

		if (updated.length === 0) {
			throw new ClaimConflictError();
		}

		await tx.insert(issueActivity).values({
			issueId,
			eventType: 'claimed',
			actorType,
			actorId,
			newValue: { assigneeType: actorType, assignedTo: actorId },
		});

		return updated[0];
	});
}

/**
 * §7: release a claim. Non-force release is itself an atomic conditional UPDATE scoped to the
 * current claimant (`WHERE assigned_to = $actorId`) so a caller cannot release someone else's
 * claim by simply calling this with their own id -- that path is `force`, gated by the caller
 * (owner/admin only, enforced in report-access.ts, not here). 0 rows updated (non-force) throws
 * `ClaimConflictError` the same way claimIssue does, for the same reason: the caller must be able
 * to tell "released" from "nothing happened".
 */
export async function releaseClaim(
	issueId: string,
	actorId: string,
	options: { force?: boolean } = {}
) {
	return await db.transaction(async (tx) => {
		const whereClause = options.force
			? eq(issues.id, issueId)
			: and(eq(issues.id, issueId), eq(issues.assignedTo, actorId));

		const updated = await tx
			.update(issues)
			.set({ assigneeType: null, assignedTo: null })
			.where(whereClause)
			.returning();

		if (updated.length === 0) {
			throw new ClaimConflictError('Issue is not claimed by this actor');
		}

		await tx.insert(issueActivity).values({
			issueId,
			eventType: 'claim_released',
			actorType: 'user',
			actorId,
			newValue: { force: Boolean(options.force) },
		});

		return updated[0];
	});
}

// Not issue-type-specific -- issue_activity is the single shared timeline for both issue
// types (§6). Re-exported here so callers of the reports query module don't need a second
// import from queries/issues.ts.
export { getIssueActivity } from './issues';
