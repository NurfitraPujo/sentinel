import { db } from '$lib/server/db';
import {
	errorOccurrences,
	issues,
	attachments,
	issueComments,
	issueActivity,
	issueTombstones,
	projects,
	agentIdempotencyKeys,
	reportIdempotencyKeys,
	externalIssueReports,
	externalReporters,
	issueAffectedUsers,
	issueAffectedTenants,
} from '$lib/db/schema';
import { sql, lt, gte, eq, and, inArray, or, isNull } from 'drizzle-orm';
import type { PgColumn, PgTable } from 'drizzle-orm/pg-core';
import { AGENT_IDEMPOTENCY_RETENTION_DAYS } from '$lib/server/agent-idempotency';
import { REPORT_IDEMPOTENCY_RETENTION_DAYS } from '$lib/server/report-idempotency';
import { deleteObject, isStorageConfigured } from '$lib/server/storage';
import { log } from '$lib/server/observability/log';

// There is deliberately no "stale issue" count here any more. The stale-marking step this module used to
// run never did anything, and could not have: it set `status: 'stale'`, which issues.check_status does not
// permit (only 'unresolved' | 'resolved' | 'ignored'), while filtering on `status = 'open'`, which is also
// not a permitted value — so the WHERE never matched a row and the invalid SET was never reached. Two
// mutually-masking bugs: the unreachable filter is precisely what stopped the illegal write from ever
// throwing, and the endpoint reported markedStaleIssues: 0 forever while appearing to work.
//
// Both values came from scripts/db/init.sql, a third and stale schema that declared
// CHECK (status IN ('open','resolved','ignored')). That file has since been DELETED (2026-07-30) — it was
// unreferenced by any container or test and existed only as a source of wrong values to copy from. The concept is dropped rather than repaired: nothing
// consumed the count, no spec asks for a 'stale' state, and adding one would mean a migration widening a
// CHECK constraint to support a feature that has never run.
export interface RetentionResult {
	deletedOccurrences: number;
	deletedOrphanedIssues: number;
	retentionDays: number;
	cutoffDate: Date;
	// A04 (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md): the second, longer cutoff that gates
	// deletion of occurrence-less manual issues (issueType='user_report') — see orphanCondition
	// below. Manual issues never HAVE occurrence rows by construction, so gating them on the same
	// short `retentionDays` used for system-error issues would delete a legitimate, still-open
	// manually-reported issue for no reason other than its type.
	manualRetentionDays: number;
	// N8 (docs/audits/AGENT_AUTOMATION_AUDIT_2026-08-14.md A04, DECISIONS.md D20): one
	// `issue_tombstones` row is written per issue this run actually deleted, so a claim-holding /
	// question-awaiting agent gets a terminal 'issue_deleted' event on the feed instead of a silent
	// 404. `deletedTombstones` counts tombstones aged past `tombstoneRetentionDays` and pruned this
	// run -- the tombstone table is itself bounded, it does not grow without limit.
	tombstonesWritten: number;
	deletedTombstones: number;
	tombstoneRetentionDays: number;
	// sentinel-user-report-issue §6: the manual-issue retention sweep has to know about the SECOND
	// companion. `orphanCondition` below already ages an externally-sourced report out, because it
	// keys on `issues.issue_type = 'user_report'` -- which an external report is -- and the
	// `external_issue_reports` row goes with it through the FK cascade. What did NOT go with it is
	// the `external_reporters` row, which has no cascade from `issues` at all and would otherwise
	// outlive every report it ever made, forever. This counts the reporters reaped for that reason.
	deletedExternalReporters: number;
	// sentinel-affected-users §4.5 (decision 23): the number of `issue_affected_users` /
	// `issue_affected_tenants` rows the sweep removed this run -- the length of each side-table
	// DELETE's RETURNING, exactly as `deletedOccurrences` is the occurrence delete's -- and NOT the
	// number of issues whose count was recomputed. Written by `sweepAffectedSideTable` below.
	deletedAffectedUsers: number;
	deletedAffectedTenants: number;
}

// sentinel-affected-users §4.5: one side table of the affected users/tenants rollup, as the sweep
// sees it -- the table whose aged rows go, and the `issues` column that is its cardinality
// (decision 12). Two of them; the SQL is identical, only the names differ.
interface AffectedSideTable {
	table: PgTable;
	// `sql.identifier(countColumn.name)` is what the UPDATE's SET uses: `${issues.affectedUserCount}`
	// would render the qualified `"issues"."affected_user_count"`, which Postgres rejects on the
	// left of a SET.
	countColumn: PgColumn;
}

const AFFECTED_USERS_SIDE_TABLE: AffectedSideTable = {
	table: issueAffectedUsers,
	countColumn: issues.affectedUserCount,
};
const AFFECTED_TENANTS_SIDE_TABLE: AffectedSideTable = {
	table: issueAffectedTenants,
	countColumn: issues.affectedTenantCount,
};

/**
 * sentinel-affected-users §4.5 (decisions 12, 23, 28): the retention sweep's step for ONE side
 * table -- three statements in ONE transaction, in this order and no other:
 *
 *   1. `SELECT id FROM issues WHERE id IN (aged side rows' issue_ids) ORDER BY id FOR UPDATE` --
 *      the touched `issues` rows are locked FIRST, in id order. This is the same lock order
 *      `StoreEvent` takes (apps/processor-go/store/store.go: the issues upsert, then the side
 *      table), so an ingest for a touched issue queues on the row lock and proceeds when this
 *      commits; neither transaction can hold a side-table lock the other is waiting for, so `40P01`
 *      is impossible by construction. The first draft locked the side rows first and would have
 *      deadlocked against ingest once an hour under load.
 *   2. `DELETE … WHERE last_seen < cutoff AND issue_id = ANY(locked ids) RETURNING issue_id` --
 *      decision 28: scoped to the set statement 1 locked. An aged row on an issue statement 1 did
 *      not see (an ingest that committed between the two statements with a client `created_at`
 *      older than the cutoff) survives one more hour and is swept by the next run, which locks its
 *      issue first. Its RETURNING length is the result field.
 *   3. `UPDATE issues SET <count> = (SELECT count(*) FROM <side table> …) WHERE id = ANY(locked
 *      ids)` -- the recompute, over the locked set only (an untouched issue's count is not
 *      rewritten). Windowed counts (decision 12): this is where a count goes DOWN.
 *
 * Three statements and NOT one data-modifying CTE, on purpose: a CTE and the query that references
 * it run on the same snapshot ("cannot see one another's effects on the target tables"), so a
 * `count(*)` in the UPDATE half of one statement would count the side rows BEFORE the DELETE half
 * removed them and the recompute would be a no-op after every sweep. Separate statements in one
 * transaction see each other.
 *
 * A transaction -- unlike every other step of `cleanupRetainedData`, which are autocommit
 * statements -- because a row lock lasts exactly as long as its transaction, and because failure
 * must be atomic (flow R4): if any statement throws, Drizzle rolls the transaction back and
 * neither rows nor counts for this side table have moved; the route logs and returns non-2xx and
 * the next hourly run does the same work. There is no window in which rows are gone and the count
 * is stale. D18: nothing in here `return`s early to signal a failure -- an error propagates as a
 * throw so the rollback actually happens. With no aged rows at all the three statements still run
 * (an empty locked set locks, deletes and recomputes nothing) rather than short-circuiting, so the
 * callback has exactly one exit and it is the commit.
 *
 * Returns the number of side rows deleted.
 */
async function sweepAffectedSideTable(
	{ table, countColumn }: AffectedSideTable,
	cutoffDate: Date
): Promise<number> {
	// `::timestamptz`, not `::timestamp`: `last_seen` is TIMESTAMP WITH TIME ZONE (§4.6), and a
	// `::timestamp` cast would drop the ISO literal's `Z` and re-read it in the session's TimeZone,
	// sliding the window by the server's UTC offset -- the same trap triage.ts's window query names.
	const cutoffIso = cutoffDate.toISOString();

	return await db.transaction(async (tx) => {
		// 1. Lock the touched issues rows first, in id order (decision 23).
		const locked = await tx.execute<{ id: string }>(sql`
			SELECT ${issues.id} AS id
			FROM ${issues}
			WHERE ${issues.id} IN (
				SELECT DISTINCT issue_id FROM ${table} WHERE last_seen < ${cutoffIso}::timestamptz
			)
			ORDER BY ${issues.id}
			FOR UPDATE
		`);
		// `sql.param`, not a bare `${lockedIds}`: Drizzle expands a bare JS array into a `(a, b, c)`
		// tuple, which Postgres cannot cast to `uuid[]`; a Param goes through as ONE `$n` bound to
		// the whole array (postgres-js encodes it as a PG array), which is what `= ANY($2)` needs.
		const lockedIds = sql.param(locked.map((row) => row.id));

		// 2. Delete the aged side rows, scoped to the locked set (decision 28); the returned row
		//    count IS the result field.
		const deleted = await tx.execute(sql`
			DELETE FROM ${table}
			WHERE last_seen < ${cutoffIso}::timestamptz
			  AND issue_id = ANY(${lockedIds}::uuid[])
			RETURNING issue_id
		`);

		// 3. Recompute every touched issue from what remains -- a separate statement, so it sees
		//    statement 2's effect.
		await tx.execute(sql`
			UPDATE ${issues}
			   SET ${sql.identifier(countColumn.name)} = (
			       SELECT count(*) FROM ${table} a WHERE a.issue_id = ${issues.id}
			   )
			 WHERE ${issues.id} = ANY(${lockedIds}::uuid[])
		`);

		return deleted.length;
	});
}

/**
 * R6 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): best-effort deletes a batch of MinIO objects,
 * logging (not throwing) on individual failures -- mirrors `deleteComment`'s ordering rationale
 * (queries/comments.ts): the DB rows are already gone by the time this runs, so a storage failure
 * here just leaves an invisible orphaned object, not a dangling DB reference.
 */
// R11 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): exported so the report delete route
// (routes/api/organizations/[orgId]/reports/[issueId]/+server.ts) reuses this exact helper
// instead of growing a second copy of the same best-effort-delete-and-log pattern.
export async function bestEffortDeleteObjects(storageKeys: string[], context: string): Promise<void> {
	if (storageKeys.length === 0 || !isStorageConfigured()) {
		return;
	}
	for (const key of storageKeys) {
		try {
			await deleteObject(key);
		} catch (err) {
			log.error('retention.attachment_storage_delete_failed', { context, key, error: err });
		}
	}
}

export async function cleanupRetainedData(
	retentionDays: number = 30,
	manualRetentionDays: number = 365,
	tombstoneRetentionDays: number = 30
): Promise<RetentionResult> {
	const cutoffDate = new Date();
	cutoffDate.setDate(cutoffDate.getDate() - retentionDays);

	// A04: manual issues (issueType='user_report') never accumulate errorOccurrences rows by
	// construction (they are reported directly, not ingested), so they are ALWAYS "occurrence-less"
	// -- gating their deletion on `retentionDays` (default 30) would delete a legitimate,
	// still-relevant manual issue purely because of its type. `manualCutoff` is a second, longer
	// cutoff (default 365d) that applies only to them.
	const manualCutoff = new Date();
	manualCutoff.setDate(manualCutoff.getDate() - manualRetentionDays);

	const deletedOccurrencesResult = await db
		.delete(errorOccurrences)
		.where(lt(errorOccurrences.createdAt, cutoffDate))
		.returning({ id: errorOccurrences.id });

	const deletedOccurrences = deletedOccurrencesResult.length;

	// sentinel-affected-users §4.5: the side-table step sits HERE -- after the occurrence delete
	// (so it uses the same `cutoffDate`) and BEFORE the orphan-issue delete below, so an issue whose
	// occurrences all aged out is still present for the count recompute and is then removed by the
	// existing orphan rule with its side rows cascading (flow R2; `ON DELETE CASCADE`, §4.6 -- no
	// statement here). One transaction per side table; see `sweepAffectedSideTable`.
	const deletedAffectedUsers = await sweepAffectedSideTable(AFFECTED_USERS_SIDE_TABLE, cutoffDate);
	const deletedAffectedTenants = await sweepAffectedSideTable(
		AFFECTED_TENANTS_SIDE_TABLE,
		cutoffDate
	);

	// DEFECT FIX: this delete previously had no age check at all — it removed ANY zero-occurrence
	// issue immediately, including one created moments ago whose first (and only) occurrence had
	// just been deleted above. An issue with no occurrences yet still has to survive until it is
	// itself old enough to be outside the retention window.
	//
	// `first_seen` (not `last_seen`) is the right column: per packages/db-migrations, it defaults to
	// now() at INSERT and is never updated again (processor-go's upsert in store.go only bumps
	// last_seen via GREATEST on conflict) — so it is exactly "when this issue was created" and keeps
	// that meaning even after its occurrences are gone. `last_seen` would also default to the
	// creation time for a brand-new issue, but conceptually it tracks the most recent occurrence,
	// which is the wrong signal to gate deletion of a row precisely because it has none.
	const noOccurrencesCondition = sql`${issues.id} NOT IN (
		SELECT DISTINCT ${errorOccurrences.issueId}
		FROM ${errorOccurrences}
	)`;

	// System-error (ingested) issues: unchanged behavior, gated on the short `retentionDays` cutoff.
	const systemErrorOrphanCondition = and(
		noOccurrencesCondition,
		sql`${issues.issueType} <> 'user_report'`,
		lt(issues.firstSeen, cutoffDate)
	);

	// A04: manual issues additionally require status IN ('resolved','ignored') -- never delete an
	// 'unresolved' one, however old -- AND assigned_to IS NULL -- never delete a claimed one, agent
	// or human, mid-triage -- AND the longer `manualCutoff`.
	//
	// sentinel-user-report-issue §6: this condition is COMPANION-AGNOSTIC and must stay that way.
	// It keys on `issues.issue_type`, not on the presence of a `manual_issue_reports` row, so an
	// externally-sourced report -- which is an `issue_type = 'user_report'` row with an
	// `external_issue_reports` companion instead -- is aged out by exactly this branch, and its
	// companion goes with it through that table's `ON DELETE CASCADE`. Narrowing this to the
	// manual companion (a join, an EXISTS) would make every external report immortal.
	const manualIssueOrphanCondition = and(
		noOccurrencesCondition,
		eq(issues.issueType, 'user_report'),
		or(eq(issues.status, 'resolved'), eq(issues.status, 'ignored')),
		isNull(issues.assignedTo),
		lt(issues.firstSeen, manualCutoff)
	);

	const orphanCondition = or(systemErrorOrphanCondition, manualIssueOrphanCondition);

	// R6: this delete cascades away any attachments rows tied to a doomed issue (directly, via
	// attachments.issue_id) or to one of its comments (via attachments.comment_id ->
	// issue_comments.issue_id) -- MinIO has no transactional participation in that cascade, so the
	// storage_keys are collected BEFORE the delete, mirroring `deleteComment`
	// (queries/comments.ts). This candidate set is a snapshot: a concurrent occurrence insert
	// between this SELECT and the DELETE below could in principle change who is "orphaned", but
	// the DELETE's own WHERE re-evaluates the same condition, so at worst this collects a
	// storage_key for a row the DELETE then doesn't actually remove -- a harmless no-op object
	// delete, never a false negative that leaves an object behind.
	// N8: the same candidate SELECT also captures the detail a tombstone needs -- organization_id
	// (via the projects join, since the issues row is gone the moment we delete it and cannot be
	// joined afterward), the display message/type, and the claim snapshot so a claim-holding agent
	// can still find the deletion via `?claimed=me`. Keyed by id so we tombstone ONLY the issues the
	// DELETE actually removed (its WHERE re-evaluates orphanCondition; a concurrent occurrence insert
	// could spare a candidate).
	const candidateRows = await db
		.select({
			id: issues.id,
			projectId: issues.projectId,
			organizationId: projects.organizationId,
			message: issues.message,
			issueType: issues.issueType,
			assigneeType: issues.assigneeType,
			assignedTo: issues.assignedTo,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(orphanCondition);
	const candidateIds = candidateRows.map((row) => row.id);
	const candidateById = new Map(candidateRows.map((row) => [row.id, row]));

	let doomedStorageKeys: string[] = [];
	if (candidateIds.length > 0) {
		const commentRows = await db
			.select({ id: issueComments.id })
			.from(issueComments)
			.where(inArray(issueComments.issueId, candidateIds));
		const commentIds = commentRows.map((row) => row.id);

		const attachmentConditions = [inArray(attachments.issueId, candidateIds)];
		if (commentIds.length > 0) {
			attachmentConditions.push(inArray(attachments.commentId, commentIds));
		}

		const attachmentRows = await db
			.select({ storageKey: attachments.storageKey })
			.from(attachments)
			.where(
				attachmentConditions.length > 1 ? or(...attachmentConditions) : attachmentConditions[0]
			);
		doomedStorageKeys = attachmentRows.map((row) => row.storageKey);
	}

	const orphanedIssuesResult = await db.delete(issues).where(orphanCondition).returning({ id: issues.id });

	await bestEffortDeleteObjects(doomedStorageKeys, 'retention_orphaned_issues');

	const deletedOrphanedIssues = orphanedIssuesResult.length;

	// N8: one tombstone per issue this run actually deleted, built from the pre-delete snapshot. The
	// insert follows the DELETE (the FK-free tombstone has no ordering dependency on the issue row)
	// and is scoped to `orphanedIssuesResult` so a candidate the DELETE spared is not tombstoned.
	let tombstonesWritten = 0;
	const tombstoneValues = orphanedIssuesResult
		.map((row) => candidateById.get(row.id))
		// `projects.organization_id` is nullable in the schema; an org-scoped feed row is
		// meaningless without one, so a (in practice non-existent) orgless project's issue is simply
		// not tombstoned rather than written with a null org.
		.filter(
			(detail): detail is NonNullable<typeof detail> & { organizationId: string } =>
				detail !== undefined && detail.organizationId !== null
		)
		.map((detail) => ({
			issueId: detail.id,
			organizationId: detail.organizationId,
			projectId: detail.projectId,
			issueMessage: detail.message,
			issueType: detail.issueType,
			assigneeType: detail.assigneeType,
			assignedTo: detail.assignedTo,
			reason: 'retention',
		}));
	if (tombstoneValues.length > 0) {
		await db.insert(issueTombstones).values(tombstoneValues);
		tombstonesWritten = tombstoneValues.length;
	}

	// N8: bound the tombstone table itself -- prune tombstones older than `tombstoneRetentionDays`
	// (default 30). A consumer that has been offline longer than that loses the deletion signal, the
	// same forward-looking-window trade the events feed already makes (agents bootstrap current
	// state via GET /api/agent/issues, not by replaying unbounded history).
	const tombstoneCutoff = new Date();
	tombstoneCutoff.setDate(tombstoneCutoff.getDate() - tombstoneRetentionDays);
	const deletedTombstonesResult = await db
		.delete(issueTombstones)
		.where(lt(issueTombstones.deletedAt, tombstoneCutoff))
		.returning({ id: issueTombstones.id });
	const deletedTombstones = deletedTombstonesResult.length;

	// sentinel-user-report-issue §6 / §4.6: reap external reporters that no longer have a report.
	// `external_reporters` is referenced by `external_issue_reports.external_reporter_id` with no
	// ON DELETE behaviour back from `issues`, so deleting a report above leaves the reporter row
	// standing. Both conditions matter: no remaining report (a reporter with one live report must
	// survive even if it has not been seen for a year) AND `last_seen_at` older than the same long
	// `manualCutoff` the reports themselves use (a reporter that filed and had its report deleted
	// yesterday is still a repeat reporter for §4.2's "first report from this person" signal).
	const deletedExternalReportersResult = await db
		.delete(externalReporters)
		.where(
			and(
				lt(externalReporters.lastSeenAt, manualCutoff),
				sql`NOT EXISTS (
					SELECT 1 FROM ${externalIssueReports}
					WHERE ${externalIssueReports.externalReporterId} = ${externalReporters.id}
				)`
			)
		)
		.returning({ id: externalReporters.id });
	const deletedExternalReporters = deletedExternalReportersResult.length;

	return {
		deletedOccurrences,
		deletedOrphanedIssues,
		retentionDays,
		cutoffDate,
		manualRetentionDays,
		tombstonesWritten,
		deletedTombstones,
		tombstoneRetentionDays,
		deletedExternalReporters,
		deletedAffectedUsers,
		deletedAffectedTenants,
	};
}

/**
 * N9 (D21): ages out client-supplied idempotency keys older than
 * `AGENT_IDEMPOTENCY_RETENTION_DAYS` (7). Past that window a repeat key is no longer deduplicated --
 * a retry that late is a fresh logical request, not a resend of a dropped response -- so the row's
 * only remaining purpose is table bloat. Same piggyback pattern as reapStaleClaims: driven by the
 * existing retention cron rather than a second scheduled route. Returns the count deleted.
 */
export async function reapExpiredIdempotencyKeys(
	retentionDays: number = AGENT_IDEMPOTENCY_RETENTION_DAYS
): Promise<number> {
	const cutoff = new Date();
	cutoff.setDate(cutoff.getDate() - retentionDays);

	const deleted = await db
		.delete(agentIdempotencyKeys)
		.where(lt(agentIdempotencyKeys.createdAt, cutoff))
		.returning({ id: agentIdempotencyKeys.id });

	return deleted.length;
}

/**
 * sentinel-user-report-issue §8: the same sweep for `report_idempotency_keys`, whose window is
 * also 7 days ("matching the existing reaper's cutoff"). A separate function rather than a
 * parameter on the one above because the two tables are two different key spaces -- the agent's
 * and the calling API key row's -- and collapsing them would make one retention knob mean two
 * things. `idx_report_idempotency_keys_created_at` keeps this off a full scan.
 *
 * Past the window a stored key no longer deduplicates: `POST /api/report` deletes an expired row
 * itself when a repeat arrives (see `deleteExpiredReportIdempotency`), so this sweep is about
 * table bloat, not about correctness. Returns the count deleted.
 */
export async function reapExpiredReportIdempotencyKeys(
	retentionDays: number = REPORT_IDEMPOTENCY_RETENTION_DAYS
): Promise<number> {
	const cutoff = new Date();
	cutoff.setDate(cutoff.getDate() - retentionDays);

	const deleted = await db
		.delete(reportIdempotencyKeys)
		.where(lt(reportIdempotencyKeys.createdAt, cutoff))
		.returning({ id: reportIdempotencyKeys.id });

	return deleted.length;
}

// N7c (A03): result of a single stale-claim reap pass.
export interface ReapStaleClaimsResult {
	releasedClaims: number;
	staleHours: number;
}

/**
 * A03: force-releases agent claims an unattended loop abandoned. Only claims with
 * assigneeType='agent' are eligible -- human claims are never auto-released. A claim is
 * stale-eligible when `claimedAt` is either NULL (pre-migration claim, or somehow never set) or
 * older than `staleHours` (default 24, env CLAIM_STALE_HOURS). Even a stale-eligible claim is
 * still protected if the claimant has written an issue_activity row on that issue within the
 * same window -- an agent making visible progress should not be reaped just because it claimed a
 * while ago.
 *
 * Per-candidate: read (select stale-eligible issues) -> check recent activity -> conditional
 * UPDATE re-scoped to the same claimant, so a claim that changed hands between the select and the
 * update (release + reclaim by someone else) is never touched (0 rows updated -> skipped, not an
 * error -- this is a best-effort sweep, not a caller-facing conditional mutation like claimIssue).
 * D18: every skip is a `continue` over a candidate, never an early `return` out of the
 * transaction -- the loop's own writes so far always land as part of one commit.
 */
export async function reapStaleClaims(staleHours: number = 24): Promise<ReapStaleClaimsResult> {
	const cutoff = new Date();
	cutoff.setHours(cutoff.getHours() - staleHours);

	return await db.transaction(async (tx) => {
		const candidates = await tx
			.select({ id: issues.id, assignedTo: issues.assignedTo })
			.from(issues)
			.where(
				and(
					eq(issues.assigneeType, 'agent'),
					sql`${issues.assignedTo} IS NOT NULL`,
					or(isNull(issues.claimedAt), lt(issues.claimedAt, cutoff))
				)
			);

		let releasedClaims = 0;

		for (const candidate of candidates) {
			if (!candidate.assignedTo) continue;

			const recentActivity = await tx
				.select({ id: issueActivity.id })
				.from(issueActivity)
				.where(
					and(
						eq(issueActivity.issueId, candidate.id),
						eq(issueActivity.actorId, candidate.assignedTo),
						gte(issueActivity.createdAt, cutoff)
					)
				)
				.limit(1);

			if (recentActivity.length > 0) continue;

			const updated = await tx
				.update(issues)
				.set({ assigneeType: null, assignedTo: null, claimedAt: null })
				.where(
					and(
						eq(issues.id, candidate.id),
						eq(issues.assignedTo, candidate.assignedTo),
						eq(issues.assigneeType, 'agent')
					)
				)
				.returning({ id: issues.id });

			if (updated.length === 0) continue;

			await tx.insert(issueActivity).values({
				issueId: candidate.id,
				eventType: 'claim_released',
				actorType: 'system',
				actorId: 'sentinel-claim-reaper',
				newValue: { previousAssignee: candidate.assignedTo, reason: 'stale' },
			});

			releasedClaims++;
		}

		return { releasedClaims, staleHours };
	});
}
