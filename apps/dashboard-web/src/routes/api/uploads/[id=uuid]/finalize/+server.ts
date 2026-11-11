import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { requireReportAccess } from '$lib/server/report-access';
import { getAttachmentById } from '$lib/db/queries/reports';
import { finalizePresignedAttachment } from '$lib/server/upload-core';

// M6 Feature A (docs/plans/M6_PRESIGNED_UPLOADS_AND_TOOLBAR_PLAN.md §Feature A). Session-only,
// mirrors /api/uploads/presign. Org is resolved from the attachment row itself (never from the
// request body -- B7) before the access check runs; uploader identity is re-checked inside the
// core function as well.

export const POST: RequestHandler = async ({ params, locals }) => {
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

	await requireReportAccess(userId, attachment.orgId, 'create');

	const result = await finalizePresignedAttachment({
		attachmentId: id,
		organizationId: attachment.orgId,
		uploaderId: userId,
	});

	return json(result, { status: 200 });
};
