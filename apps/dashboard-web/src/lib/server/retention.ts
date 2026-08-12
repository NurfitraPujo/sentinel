import { db } from '$lib/server/db';
import { errorOccurrences, issues, attachments, issueComments } from '$lib/db/schema';
import { sql, lt, and, inArray, or } from 'drizzle-orm';
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

export async function cleanupRetainedData(retentionDays: number = 30): Promise<RetentionResult> {
	const cutoffDate = new Date();
	cutoffDate.setDate(cutoffDate.getDate() - retentionDays);

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
	const orphanCondition = and(
		sql`${issues.id} NOT IN (
			SELECT DISTINCT ${errorOccurrences.issueId}
			FROM ${errorOccurrences}
		)`,
		lt(issues.firstSeen, cutoffDate)
	);

	// R6: this delete cascades away any attachments rows tied to a doomed issue (directly, via
	// attachments.issue_id) or to one of its comments (via attachments.comment_id ->
	// issue_comments.issue_id) -- MinIO has no transactional participation in that cascade, so the
	// storage_keys are collected BEFORE the delete, mirroring `deleteComment`
	// (queries/comments.ts). This candidate set is a snapshot: a concurrent occurrence insert
	// between this SELECT and the DELETE below could in principle change who is "orphaned", but
	// the DELETE's own WHERE re-evaluates the same condition, so at worst this collects a
	// storage_key for a row the DELETE then doesn't actually remove -- a harmless no-op object
	// delete, never a false negative that leaves an object behind.
	const candidateIssueIds = await db
		.select({ id: issues.id })
		.from(issues)
		.where(orphanCondition);
	const candidateIds = candidateIssueIds.map((row) => row.id);

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

	return {
		deletedOccurrences,
		deletedOrphanedIssues,
		retentionDays,
		cutoffDate,
	};
}
