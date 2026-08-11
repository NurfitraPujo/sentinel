import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { randomUUID } from 'node:crypto';
import { db } from '$lib/server/db';
import { attachments } from '$lib/db/schema';
import { requireReportAccess } from '$lib/server/report-access';
import { isStorageConfigured, putObject } from '$lib/server/storage';
import { sniffContentType, resolveContentType } from '$lib/server/attachment-sniff';
import { reapOrgOrphanAttachments } from '$lib/server/attachment-reaper';
import { log } from '$lib/server/observability/log';

// Manual Issues M2 (docs/plans/MANUAL_ISSUES_DESIGN.md §4). Manual-validation style
// (allowlists + throw error(status)), matching the invitations/reports endpoints -- no schema
// library.

const MAX_UPLOAD_BYTES = 25 * 1024 * 1024; // 25 MB cap (§4/Q4)

export const POST: RequestHandler = async ({ request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	if (!isStorageConfigured()) {
		throw error(503, 'Object storage is not configured');
	}

	// Enforced BEFORE buffering the body where the platform gives us the chance to: a client that
	// declares its size up front via Content-Length is rejected before request.formData() reads
	// anything. This is a best-effort check, not a guarantee -- a client that omits/lies about
	// Content-Length is still caught by the post-parse size check below, just after buffering.
	const declaredLength = request.headers.get('content-length');
	if (declaredLength && Number(declaredLength) > MAX_UPLOAD_BYTES) {
		throw error(413, `File exceeds the ${MAX_UPLOAD_BYTES} byte cap`);
	}

	const formData = await request.formData().catch(() => null);
	if (!formData) {
		throw error(400, 'Expected multipart/form-data');
	}

	const organizationId = formData.get('organizationId');
	if (typeof organizationId !== 'string' || organizationId.length === 0) {
		throw error(400, 'organizationId is required');
	}

	// §9 Q8: any recognized org member, including viewer, may upload -- attachments accompany
	// report creation and thread comments, both of which are open to viewers.
	await requireReportAccess(userId, organizationId, 'create');

	const file = formData.get('file');
	if (!(file instanceof File)) {
		throw error(400, 'file is required');
	}

	if (file.size > MAX_UPLOAD_BYTES) {
		throw error(413, `File exceeds the ${MAX_UPLOAD_BYTES} byte cap`);
	}
	if (file.size === 0) {
		throw error(400, 'file is empty');
	}

	const buffer = Buffer.from(await file.arrayBuffer());

	// Magic bytes are the source of truth, never the client-supplied header/File.type (§4).
	const detected = sniffContentType(buffer);
	const resolved = resolveContentType(detected, file.type || undefined);
	if (!resolved) {
		throw error(415, 'File content is not an allowed type (or does not match its declared type)');
	}

	const storageKey = `org/${organizationId}/${randomUUID()}`;

	await putObject(storageKey, buffer, resolved);

	const filename = (formData.get('filename') as string | null)?.trim() || file.name || 'upload';

	const [row] = await db
		.insert(attachments)
		.values({
			orgId: organizationId,
			issueId: null,
			commentId: null,
			uploaderType: 'user',
			uploaderId: userId,
			filename: filename.slice(0, 512),
			contentType: resolved,
			sizeBytes: buffer.length,
			storageKey,
		})
		.returning();

	if (!row) {
		throw error(500, 'Failed to record attachment');
	}

	// Opportunistic per-org sweep (mirrors D42's reapExpiredInvitations being called from the
	// invitation-write path) -- best-effort, must never fail the upload itself.
	reapOrgOrphanAttachments(organizationId).catch((err) => {
		log.error('uploads.opportunistic_reap_failed', { organizationId, error: err });
	});

	return json(
		{
			id: row.id,
			url: `/api/attachments/${row.id}`,
			filename: row.filename,
			contentType: row.contentType,
			sizeBytes: row.sizeBytes,
		},
		{ status: 201 }
	);
};
