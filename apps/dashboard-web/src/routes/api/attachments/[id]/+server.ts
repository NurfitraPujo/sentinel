import { error, json } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { eq } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { issues, attachments, issueComments } from '$lib/db/schema';
import { getAttachmentById } from '$lib/db/queries/reports';
import { requireReportAccessForIssue } from '$lib/server/report-access';
import { requireIssueAccess } from '$lib/server/issue-access';
import { getObjectStream, deleteObject, isStorageConfigured } from '$lib/server/storage';
import { log } from '$lib/server/observability/log';

// Manual Issues M2/M3 (docs/plans/MANUAL_ISSUES_DESIGN.md §4). Streams the object from MinIO
// after resolving access:
//   - Linked to an issue: read access to THAT issue, via whichever access helper matches its
//     issue_type -- `user_report` issues go through `requireReportAccessForIssue` (§9's viewer
//     carve-out), `system_error` issues go through the existing `requireIssueAccess` (D10/D17).
//   - Linked to a comment (M3): resolves access via the comment's PARENT issue, same per-type
//     dispatch as above -- issue_comments has no separate access model of its own (§5's threads
//     are exactly as visible as the issue they're attached to).
//   - Unlinked (still a draft): only the uploader may fetch it -- nobody else even knows the id
//     exists yet, and it is not attached to anything a permission check could be based on.

export const GET: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	const { id } = params;
	if (!id) {
		throw error(400, 'Missing attachment id');
	}

	const attachment = await getAttachmentById(id);
	if (!attachment) {
		throw error(404, 'Attachment not found');
	}

	if (attachment.issueId) {
		const [issueRow] = await db
			.select({ issueType: issues.issueType })
			.from(issues)
			.where(eq(issues.id, attachment.issueId));

		if (!issueRow) {
			throw error(404, 'Attachment not found');
		}

		if (issueRow.issueType === 'user_report') {
			await requireReportAccessForIssue(userId, attachment.issueId, 'read');
		} else {
			await requireIssueAccess(userId, attachment.issueId, 'read');
		}
	} else if (attachment.commentId) {
		const [commentRow] = await db
			.select({ issueId: issueComments.issueId })
			.from(issueComments)
			.where(eq(issueComments.id, attachment.commentId));

		if (!commentRow) {
			throw error(404, 'Attachment not found');
		}

		const [issueRow] = await db
			.select({ issueType: issues.issueType })
			.from(issues)
			.where(eq(issues.id, commentRow.issueId));

		if (!issueRow) {
			throw error(404, 'Attachment not found');
		}

		if (issueRow.issueType === 'user_report') {
			await requireReportAccessForIssue(userId, commentRow.issueId, 'read');
		} else {
			await requireIssueAccess(userId, commentRow.issueId, 'read');
		}
	} else {
		// Draft: not linked to anything. Only the uploader may fetch it.
		if (attachment.uploaderType !== 'user' || attachment.uploaderId !== userId) {
			throw error(403, 'Forbidden');
		}
	}

	const object = await getObjectStream(attachment.storageKey);
	if (!object.Body) {
		throw error(404, 'Attachment object not found in storage');
	}

	const headers = new Headers();
	headers.set('Content-Type', attachment.contentType);
	headers.set('Content-Length', String(attachment.sizeBytes));

	const isImage = attachment.contentType.startsWith('image/');
	const safeFilename = attachment.filename.replace(/["\r\n]/g, '_');
	headers.set(
		'Content-Disposition',
		`${isImage ? 'inline' : 'attachment'}; filename="${safeFilename}"`
	);

	// @aws-sdk/client-s3's `Body` is a web ReadableStream in this (fetch-based Node) runtime --
	// hand it straight to the platform Response rather than buffering it into memory again.
	return new Response(object.Body as unknown as ReadableStream, { headers });
};

// Manual Issues M2 §4/§10: an explicit delete endpoint for the uploader's own DRAFT attachments,
// used by the /reports/new upload zone's "remove" action. Deliberately narrower than the GET
// access check above -- only a still-unlinked (issueId/commentId both NULL) attachment may be
// deleted here, and only by the user who uploaded it. Once an attachment is claimed onto an
// issue (createManualIssue's claimDraftAttachments), it becomes part of that issue's permanent
// record and this endpoint 409s rather than silently no-op'ing -- deleting a linked attachment is
// not this phase's concern (no UI offers it, and the reaper never touches linked rows either).
export const DELETE: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	const { id } = params;
	if (!id) {
		throw error(400, 'Missing attachment id');
	}

	const attachment = await getAttachmentById(id);
	if (!attachment) {
		throw error(404, 'Attachment not found');
	}

	if (attachment.issueId !== null || attachment.commentId !== null) {
		throw error(409, 'Attachment is already linked to an issue and can no longer be removed here');
	}

	if (attachment.uploaderType !== 'user' || attachment.uploaderId !== userId) {
		throw error(403, 'Forbidden');
	}

	// Storage delete first, DB row second -- same ordering rationale as attachment-reaper.ts: a
	// crash between the two leaves an orphaned object with no DB row, which is harmless (wasted
	// bytes, no dangling reference), rather than a DB row pointing at nothing.
	try {
		if (isStorageConfigured()) {
			await deleteObject(attachment.storageKey);
		}
	} catch (err) {
		log.error('attachments.delete_storage_failed', { attachmentId: id, error: err });
		throw error(500, 'Failed to delete attachment from storage');
	}

	await db.delete(attachments).where(eq(attachments.id, id));

	return json({ success: true });
};
