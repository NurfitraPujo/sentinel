import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { requireReportAccess } from '$lib/server/report-access';
import { createPresignedAttachment } from '$lib/server/upload-core';

// M6 Feature A (docs/plans/M6_PRESIGNED_UPLOADS_AND_TOOLBAR_PLAN.md §Feature A). Session-only --
// the presigned large-upload path is for users uploading large media; agents keep the proxy
// /api/agent/uploads route (documented, not a gap). Manual-validation style, matching
// /api/uploads -- no schema library.

export const POST: RequestHandler = async ({ request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	const body = await request.json().catch(() => null);
	if (!body || typeof body !== 'object') {
		throw error(400, 'Expected a JSON body');
	}

	const { organizationId, filename, contentType, sizeBytes } = body as Record<string, unknown>;

	if (typeof organizationId !== 'string' || organizationId.length === 0) {
		throw error(400, 'organizationId is required');
	}
	if (typeof filename !== 'string' || filename.length === 0) {
		throw error(400, 'filename is required');
	}
	if (typeof contentType !== 'string' || contentType.length === 0) {
		throw error(400, 'contentType is required');
	}
	if (typeof sizeBytes !== 'number') {
		throw error(400, 'sizeBytes is required');
	}

	// §9 Q8: any recognized org member, including viewer, may upload.
	await requireReportAccess(userId, organizationId, 'create');

	const result = await createPresignedAttachment({
		organizationId,
		uploaderId: userId,
		filename,
		declaredContentType: contentType,
		sizeBytes,
	});

	return json(result, { status: 201 });
};
