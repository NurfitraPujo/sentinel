import { and, eq, isNull, lt } from 'drizzle-orm';
import { db } from './db';
import { attachments } from '$lib/db/schema';
import { deleteObject, isStorageConfigured } from './storage';
import { log } from './observability/log';

/**
 * Manual Issues M2 (docs/plans/MANUAL_ISSUES_DESIGN.md §4): reaps DRAFT attachments (never
 * linked to an issue or comment) older than 24h, following the SAME two-trigger-point shape as
 * D42's invitation reaper (`queries/organizations.ts`):
 *
 *   - `reapOrgOrphanAttachments(organizationId)` -- scoped to one org, called opportunistically
 *     from the upload path (an org that keeps uploading gets swept regularly), mirroring
 *     `reapExpiredInvitations` being called from `createOrganizationInvitation`.
 *   - `reapAllOrphanAttachments()` -- an unconditional global sweep with no org scope, driven by
 *     the existing `POST /api/cron/retention` job, mirroring `reapAllExpiredInvitations` --
 *     because an org that stops uploading (or never uploads again) would otherwise never reap on
 *     its own, the same gap D42's comment calls out for invitations.
 *
 * One structural difference from the invitation reaper: an attachment also owns a MinIO object,
 * and object storage has no transactional participation with Postgres. The DB row is therefore
 * deleted only AFTER its object is confirmed deleted (or S3 says "already gone" -- both a prior
 * partial run and a genuinely-never-uploaded row look the same to us and are both safe to treat
 * as "object side is done"). A crash between the two leaves an orphaned object with no DB row --
 * invisible to the app, and acceptable: it is not a security or correctness issue, just wasted
 * bytes, and every future run's scan is keyed off the DB row's age, not the object's, so it is
 * bounded by how often this runs rather than growing unbounded.
 */

const ORPHAN_AGE_MS = 24 * 60 * 60 * 1000;

async function deleteOrphanRows(
	rows: { id: string; storageKey: string }[]
): Promise<number> {
	if (rows.length === 0) {
		return 0;
	}

	let deleted = 0;
	for (const row of rows) {
		try {
			if (isStorageConfigured()) {
				await deleteObject(row.storageKey);
			}
		} catch (err) {
			// Storage delete failing must not stop the sweep for the OTHER rows, and must not delete
			// the DB row for THIS one -- it stays a candidate next run (see module doc above).
			log.error('attachment_reaper.storage_delete_failed', { attachmentId: row.id, error: err });
			continue;
		}

		const result = await db
			.delete(attachments)
			.where(eq(attachments.id, row.id))
			.returning({ id: attachments.id });

		if (result.length > 0) {
			deleted += 1;
		}
	}

	return deleted;
}

/**
 * Org-scoped sweep, called opportunistically after a successful upload (best-effort, errors are
 * logged and swallowed by the caller -- an upload must never fail because reaping failed).
 */
export async function reapOrgOrphanAttachments(organizationId: string): Promise<number> {
	const cutoff = new Date(Date.now() - ORPHAN_AGE_MS);

	const rows = await db
		.select({ id: attachments.id, storageKey: attachments.storageKey })
		.from(attachments)
		.where(
			and(
				eq(attachments.orgId, organizationId),
				isNull(attachments.issueId),
				isNull(attachments.commentId),
				lt(attachments.createdAt, cutoff)
			)
		);

	return await deleteOrphanRows(rows);
}

/** Unconditional global sweep, driven by `POST /api/cron/retention` (mirrors D42's global reap). */
export async function reapAllOrphanAttachments(): Promise<number> {
	const cutoff = new Date(Date.now() - ORPHAN_AGE_MS);

	const rows = await db
		.select({ id: attachments.id, storageKey: attachments.storageKey })
		.from(attachments)
		.where(
			and(isNull(attachments.issueId), isNull(attachments.commentId), lt(attachments.createdAt, cutoff))
		);

	return await deleteOrphanRows(rows);
}
