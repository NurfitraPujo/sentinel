import crypto from 'crypto';
import { db, type Tx } from '$lib/server/db';
import {
	issues,
	issueActivity,
	manualIssueReports,
	projects,
	users,
	attachments,
	issueComments,
} from '$lib/db/schema';
import { eq, and, isNull, desc, sql } from 'drizzle-orm';
import { subscribe } from '$lib/db/queries/subscriptions';
import { notifyIssueEvent, type NotifiedUser } from '$lib/server/notify';

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
async function findOrCreateTriageProject(tx: Tx, organizationId: string): Promise<string> {
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
	// R2 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): this SELECT-then-INSERT has a window
	// between the SELECT above and this INSERT where a concurrent call for the same org can win
	// the race -- without a uniqueness constraint, two concurrent first-uses both insert an inbox
	// project. `idx_projects_org_inbox_unique` (partial unique on
	// projects(organization_id) WHERE is_inbox, 1723000000_pr13_remediation.sql) makes the loser's
	// INSERT a no-op (`onConflictDoNothing`) instead of a duplicate row; the loser then re-selects
	// to return the WINNER's project id rather than its own discarded insert.
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
		// NOTE: `onConflictDoNothing`'s partial-index predicate option is named `where` in this
		// drizzle-orm version (0.30.10), NOT `targetWhere` -- `targetWhere`/`setWhere` only exist on
		// `onConflictDoUpdate` here. Passing `targetWhere` here compiles (same config type is
		// accepted) but is silently DROPPED from the generated SQL, producing a bare
		// `on conflict ("organization_id") do nothing` with no `WHERE is_inbox` -- which Postgres
		// then rejects at runtime with 42P10 "no unique or exclusion constraint matching the ON
		// CONFLICT specification", since the arbiter can't be inferred without the partial
		// predicate. Caught by reports.e2e-flow.integration.test.ts against a real migrated
		// Postgres, not by the mock-based unit test above (which never executes real SQL).
		.onConflictDoNothing({ target: [projects.organizationId], where: eq(projects.isInbox, true) })
		.returning({ id: projects.id });

	if (created) {
		return created.id;
	}

	// Lost the race: some other concurrent call already created (and committed, or is about to
	// commit) the inbox project. Re-select to return the winner's id.
	const [winner] = await tx
		.select({ id: projects.id })
		.from(projects)
		.where(and(eq(projects.organizationId, organizationId), eq(projects.isInbox, true)));

	if (!winner) {
		// Should be unreachable (the conflict target guarantees a winning row exists), but throw
		// rather than return undefined -- D18: never signal failure by returning early with a
		// falsy value from inside a transaction callback.
		throw new Error('Triage project conflict resolution found no winner');
	}

	return winner.id;
}

export interface CreateManualIssueInput {
	organizationId: string;
	projectId: string | null;
	reporterId: string;
	title: string;
	bodyMd: string;
	severity: ReportSeverity;
	sourceChannel?: 'manual_support' | 'api';
	/**
	 * Manual Issues M2 (design §4): ids of DRAFT attachments (uploaded via POST /api/uploads,
	 * issue_id/comment_id still NULL) to claim onto the new issue in the SAME transaction as its
	 * creation. Ownership/org checks happen in `claimDraftAttachments` below, not here.
	 */
	attachmentIds?: string[];
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

		// R12 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): creation writes 'report_created', not
		// 'report_edited' -- the latter previously mislabeled creation as an edit; it is now
		// reserved for R11's actual body/title/severity edits.
		await tx.insert(issueActivity).values({
			issueId: issue.id,
			eventType: 'report_created',
			actorType: 'user',
			actorId: input.reporterId,
			newValue: { action: 'created', title, severity: input.severity, projectId },
		});

		// §8 auto-subscribe: the reporter is subscribed to their own report. No fan-out here --
		// nobody else is subscribed to a brand-new issue yet, so notifyIssueEvent would be a no-op.
		await subscribe(
			{ issueId: issue.id, subscriberType: 'user', subscriberId: input.reporterId, reason: 'reporter' },
			tx
		);

		// §4/§4 linking step: claim any DRAFT attachments onto the new issue, in the SAME
		// transaction as creation. Folds into the creation activity above rather than writing a
		// second `attachment_added` row per design §6 ("... or fold into creation activity").
		if (input.attachmentIds && input.attachmentIds.length > 0) {
			const claimed = await claimDraftAttachments(
				tx,
				input.attachmentIds,
				issue.id,
				input.reporterId,
				input.organizationId
			);

			if (claimed.length > 0) {
				await tx.insert(issueActivity).values({
					issueId: issue.id,
					eventType: 'attachment_added',
					actorType: 'user',
					actorId: input.reporterId,
					newValue: { attachmentIds: claimed.map((a) => a.id) },
				});
			}
		}

		return { issue, report };
	});
}

/**
 * §4 linking: claims DRAFT attachments (issue_id/comment_id both NULL) onto `issueId`. Each
 * candidate id is verified to (a) exist, (b) still be a draft, (c) belong to `organizationId`
 * (B7: tenant scope from the caller's already-established org, never re-derived from the
 * attachment row), and (d) have been uploaded by `uploaderId` -- a caller cannot claim someone
 * else's draft upload onto their own report. Ids that fail any check are silently skipped rather
 * than throwing (a stale/foreign id in the array must not abort creating the whole report) --
 * callers that need to know what was actually claimed use the returned array.
 */
export async function claimDraftAttachments(
	tx: Tx,
	attachmentIds: string[],
	issueId: string,
	uploaderId: string,
	organizationId: string,
	uploaderType: 'user' | 'agent' = 'user'
): Promise<{ id: string }[]> {
	return await claimDraftAttachmentsOnto(tx, attachmentIds, { issueId }, uploaderId, organizationId, uploaderType);
}

/**
 * Manual Issues M3 (design §5 groundwork, §4): the comment-attachment sibling of
 * `claimDraftAttachments` -- same verification (still a draft, same org (B7), uploaded by the
 * commenting author), just setting `comment_id` instead of `issue_id`. Both delegate to
 * `claimDraftAttachmentsOnto` below so the verification logic cannot drift between the two call
 * sites.
 */
export async function claimDraftAttachmentsForComment(
	tx: Tx,
	attachmentIds: string[],
	commentId: string,
	authorId: string,
	organizationId: string,
	uploaderType: 'user' | 'agent' = 'user'
): Promise<{ id: string }[]> {
	return await claimDraftAttachmentsOnto(tx, attachmentIds, { commentId }, authorId, organizationId, uploaderType);
}

// R10 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): `uploaderType` is now matched alongside
// `uploaderId`, not just `uploaderId` alone -- `attachments.uploader_id` is a bare varchar shared
// across the 'user' and 'agent' id spaces (users.id vs. agents.id), so an id collision between
// the two (unlikely, but not prevented by any constraint) previously let a caller's draft claim
// silently match a same-valued id from the OTHER actor type.
async function claimDraftAttachmentsOnto(
	tx: Tx,
	attachmentIds: string[],
	target: { issueId: string } | { commentId: string },
	uploaderId: string,
	organizationId: string,
	uploaderType: 'user' | 'agent' = 'user'
): Promise<{ id: string }[]> {
	if (attachmentIds.length === 0) {
		return [];
	}

	const claimed: { id: string }[] = [];

	for (const attachmentId of attachmentIds) {
		const rows = await tx
			.select({
				id: attachments.id,
				orgId: attachments.orgId,
				uploaderId: attachments.uploaderId,
				uploaderType: attachments.uploaderType,
				issueId: attachments.issueId,
				commentId: attachments.commentId,
			})
			.from(attachments)
			.where(eq(attachments.id, attachmentId));

		const row = rows[0];
		if (
			!row ||
			row.orgId !== organizationId ||
			row.uploaderId !== uploaderId ||
			row.uploaderType !== uploaderType ||
			row.issueId !== null ||
			row.commentId !== null
		) {
			continue;
		}

		const updated = await tx
			.update(attachments)
			.set(target)
			.where(
				and(eq(attachments.id, attachmentId), isNull(attachments.issueId), isNull(attachments.commentId))
			)
			.returning({ id: attachments.id });

		if (updated.length > 0) {
			claimed.push(updated[0]);
		}
	}

	return claimed;
}

/**
 * §4: fetches an attachment row by id, or null. Used by both the download route (access-control
 * decisions) and reaping.
 */
export async function getAttachmentById(attachmentId: string) {
	const rows = await db.select().from(attachments).where(eq(attachments.id, attachmentId));
	return rows[0] ?? null;
}

/**
 * Manual Issues M2 (design §4/§10): attachments linked to a given issue, oldest first, for the
 * report detail page's attachments section. Deliberately does not include DRAFT attachments
 * (issueId still NULL) -- those only ever exist transiently on the /reports/new form, before the
 * issue they'll be claimed onto has been created.
 */
export async function listIssueAttachments(issueId: string) {
	return await db
		.select()
		.from(attachments)
		.where(eq(attachments.issueId, issueId))
		.orderBy(attachments.createdAt);
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
			// R7: waitingOn is now cleared on resolve/ignore, so this condition alone should already
			// exclude them going forward -- kept as an explicit filter too (belt-and-suspenders) for
			// any pre-existing row from before that fix still carries a stale waitingOn value.
			conditions.push(sql`${issues.waitingOn} IS NOT NULL`);
			conditions.push(eq(issues.status, 'unresolved'));
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
			// M3 (design §5/§10): the "Comments" column that rendered "–" in M1 -- a correlated
			// scalar subquery rather than a join+group, since a join would fan the base row out
			// once per comment and force an outer aggregation this query didn't otherwise need.
			commentCount: sql<number>`(select count(*)::int from ${issueComments} where ${issueComments.issueId} = ${issues.id})`,
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
			// M3: same "Comments" count surfacing as listReports above.
			commentCount: sql<number>`(select count(*)::int from ${issueComments} where ${issueComments.issueId} = ${issues.id})`,
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
 *
 * §8 M4 scope note: deliberately NO notifyIssueEvent call here. `notifications.kind`'s CHECK
 * constraint only allows the catalog in the design doc (commented|claimed|status_changed|
 * resolved|linked|progress_update|question_asked) -- 'moved' is not among them, and folding a
 * move under 'status_changed' would be semantically wrong (nothing about the issue's status
 * changed). A move gets no notification in v1; revisit if/when the CHECK grows a 'moved' kind.
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

export interface UpdateManualIssueReportInput {
	issueId: string;
	actorId: string;
	title?: string;
	bodyMd?: string;
	severity?: ReportSeverity;
}

/**
 * R11 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md, §9): author-only edit of a report's
 * title/body/severity, until the issue is resolved (enforced by the route layer, not here --
 * this function trusts the caller already checked `canEditReport` + issue status). Updates
 * `issues.message` (title mirror) and/or `manual_issue_reports.body_md`/`severity` in one
 * transaction with a `report_edited` activity row carrying old/new values (D18: throw, not
 * return, on a missing issue/report so a partial update never commits).
 */
export async function updateManualIssueReport(input: UpdateManualIssueReportInput) {
	return await db.transaction(async (tx) => {
		const [existingIssue] = await tx
			.select({ id: issues.id, message: issues.message })
			.from(issues)
			.where(eq(issues.id, input.issueId));

		if (!existingIssue) {
			throw new Error(`Issue ${input.issueId} not found`);
		}

		const [existingReport] = await tx
			.select({ bodyMd: manualIssueReports.bodyMd, severity: manualIssueReports.severity })
			.from(manualIssueReports)
			.where(eq(manualIssueReports.issueId, input.issueId));

		if (!existingReport) {
			throw new Error(`Report ${input.issueId} not found`);
		}

		const oldValue: Record<string, unknown> = {};
		const newValue: Record<string, unknown> = {};

		if (input.title !== undefined && input.title !== existingIssue.message) {
			oldValue.title = existingIssue.message;
			newValue.title = input.title;
			await tx.update(issues).set({ message: input.title }).where(eq(issues.id, input.issueId));
		}

		const reportUpdate: Partial<typeof manualIssueReports.$inferInsert> = {};
		if (input.bodyMd !== undefined && input.bodyMd !== existingReport.bodyMd) {
			oldValue.bodyMd = existingReport.bodyMd;
			newValue.bodyMd = input.bodyMd;
			reportUpdate.bodyMd = input.bodyMd;
		}
		if (input.severity !== undefined && input.severity !== existingReport.severity) {
			oldValue.severity = existingReport.severity;
			newValue.severity = input.severity;
			reportUpdate.severity = input.severity;
		}
		if (Object.keys(reportUpdate).length > 0) {
			await tx.update(manualIssueReports).set(reportUpdate).where(eq(manualIssueReports.issueId, input.issueId));
		}

		if (Object.keys(newValue).length > 0) {
			await tx.insert(issueActivity).values({
				issueId: input.issueId,
				eventType: 'report_edited',
				actorType: 'user',
				actorId: input.actorId,
				oldValue,
				newValue,
			});
		}

		const [updatedIssue] = await tx.select().from(issues).where(eq(issues.id, input.issueId));
		const [updatedReport] = await tx
			.select()
			.from(manualIssueReports)
			.where(eq(manualIssueReports.issueId, input.issueId));

		return { issue: updatedIssue, report: updatedReport };
	});
}

/**
 * R11 (§9, R6): deletes a manual issue entirely (author until resolved, owner/admin anytime --
 * enforced by the route layer). `issues` row delete cascades away `manual_issue_reports`,
 * `issue_activity`, `issue_comments`, `issue_subscriptions`, and `attachments` rows (all FK
 * `ON DELETE CASCADE`, schema.ts) -- but MinIO has no transactional participation in that
 * cascade, so attachment storage_keys (both linked directly to the issue AND to one of its
 * comments) are collected BEFORE the delete, same pattern as retention.ts's R6 fix and
 * `deleteComment` (queries/comments.ts). Returns the collected keys so the route can best-effort
 * delete the objects AFTER the transaction commits.
 */
export async function deleteManualIssue(issueId: string): Promise<{ storageKeys: string[] }> {
	return await db.transaction(async (tx) => {
		const [existingIssue] = await tx.select({ id: issues.id }).from(issues).where(eq(issues.id, issueId));
		if (!existingIssue) {
			throw new Error(`Issue ${issueId} not found`);
		}

		const commentRows = await tx
			.select({ id: issueComments.id })
			.from(issueComments)
			.where(eq(issueComments.issueId, issueId));
		const commentIds = commentRows.map((row) => row.id);

		const directAttachmentRows = await tx
			.select({ storageKey: attachments.storageKey })
			.from(attachments)
			.where(eq(attachments.issueId, issueId));

		let commentAttachmentRows: { storageKey: string }[] = [];
		if (commentIds.length > 0) {
			commentAttachmentRows = await tx
				.select({ storageKey: attachments.storageKey })
				.from(attachments)
				.where(sql`${attachments.commentId} IN (${sql.join(commentIds.map((id) => sql`${id}`), sql`, `)})`);
		}

		const storageKeys = [...directAttachmentRows, ...commentAttachmentRows].map((row) => row.storageKey);

		await tx.delete(issues).where(eq(issues.id, issueId));

		return { storageKeys };
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
): Promise<{ issue: typeof issues.$inferSelect; notified: NotifiedUser[] }> {
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

		// §7/§8/M5: the claimant is auto-subscribed (reason 'claimant'), for BOTH actor types as of
		// M5. M4 only subscribed user claimants (a correct simplification while nothing ever claimed
		// as an agent); M5's work-loop makes agent claims real, and design §7 step 2 explicitly
		// requires the subscription ROW to exist ("claimant auto-subscribed") even though
		// `notifyIssueEvent` (design §8) still skips agent subscribers when building `notifications`
		// rows -- agents poll comments/activity rather than receive notification rows, so this row's
		// only consumer today is the subscription-list UI/toggle, not the email/notification fan-out.
		await subscribe({ issueId, subscriberType: actorType, subscriberId: actorId, reason: 'claimant' }, tx);

		const notified = await notifyIssueEvent(tx, {
			issueId,
			kind: 'claimed',
			actorType,
			actorId,
			payload: { assigneeType: actorType, assignedTo: actorId },
		});

		return { issue: updated[0], notified };
	});
}

/**
 * §7: release a claim. Non-force release is itself an atomic conditional UPDATE scoped to the
 * current claimant (`WHERE assigned_to = $actorId`) so a caller cannot release someone else's
 * claim by simply calling this with their own id -- that path is `force`, gated by the caller
 * (owner/admin only, enforced in report-access.ts, not here). 0 rows updated (non-force) throws
 * `ClaimConflictError` the same way claimIssue does, for the same reason: the caller must be able
 * to tell "released" from "nothing happened".
 *
 * M5: `actorType` used to be hardcoded 'user' here (the M1 stub), which meant an agent release
 * (via `/api/agent/issues/[id]/claim` DELETE) recorded a false activity/notification actor type
 * -- the issue_activity row said 'user' even though `actorId` was an agent id. It now defaults to
 * 'user' only for source compatibility with the session-authenticated route, which never passes
 * it; callers that release on behalf of an agent MUST pass `actorType: 'agent'`.
 */
export async function releaseClaim(
	issueId: string,
	actorId: string,
	options: { force?: boolean; actorType?: 'user' | 'agent' } = {}
): Promise<{ issue: typeof issues.$inferSelect; notified: NotifiedUser[] }> {
	const actorType = options.actorType ?? 'user';
	return await db.transaction(async (tx) => {
		// R10: `assigneeType` is now matched alongside `assignedTo` in the non-force conditional
		// UPDATE too -- `assigned_to` is a bare varchar shared across the 'user'/'agent' id spaces
		// (same rationale as claimDraftAttachmentsOnto's uploaderType check above), so without this
		// an id collision across the two spaces could let one actor type release a claim actually
		// held by the other.
		const whereClause = options.force
			? eq(issues.id, issueId)
			: and(eq(issues.id, issueId), eq(issues.assignedTo, actorId), eq(issues.assigneeType, actorType));

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
			actorType,
			actorId,
			newValue: { force: Boolean(options.force) },
		});

		// §8: 'claim_released' is not in notifications.kind's CHECK -- reuse 'claimed' with a
		// `released: true` payload flag (per the M4 task's instruction), rather than the
		// activity-timeline eventType.
		const notified = await notifyIssueEvent(tx, {
			issueId,
			kind: 'claimed',
			actorType,
			actorId,
			payload: { released: true, force: Boolean(options.force) },
		});

		return { issue: updated[0], notified };
	});
}

// Not issue-type-specific -- issue_activity is the single shared timeline for both issue
// types (§6). Re-exported here so callers of the reports query module don't need a second
// import from queries/issues.ts.
export { getIssueActivity } from './issues';
