import { db } from '$lib/server/db';
import {
	errorOccurrences,
	issues,
	attachments,
	issueComments,
	issueActivity,
	issueTombstones,
	projects,
} from '$lib/db/schema';
import { sql, lt, gte, eq, and, inArray, or, isNull } from 'drizzle-orm';
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

	return {
		deletedOccurrences,
		deletedOrphanedIssues,
		retentionDays,
		cutoffDate,
		manualRetentionDays,
		tombstonesWritten,
		deletedTombstones,
		tombstoneRetentionDays,
	};
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
