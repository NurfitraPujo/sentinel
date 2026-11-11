import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { requireReportAccess } from '$lib/server/report-access';
import { handleAttachmentUpload, checkDeclaredLength } from '$lib/server/upload-core';

// Manual Issues M2 (docs/plans/MANUAL_ISSUES_DESIGN.md §4). Manual-validation style
// (allowlists + throw error(status)), matching the invitations/reports endpoints -- no schema
// library. Core upload logic (25 MB cap, magic-byte sniffing, storage write, opportunistic reap)
// lives in $lib/server/upload-core.ts as of M5, shared with the agent-authenticated
// /api/agent/uploads route -- this file keeps only session auth + the multipart/orgId parsing.

export const POST: RequestHandler = async ({ request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	// Enforced BEFORE buffering the body where the platform gives us the chance to.
	checkDeclaredLength(request);

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

	const result = await handleAttachmentUpload({
		organizationId,
		formData,
		uploaderType: 'user',
		uploaderId: userId,
	});

	return json(result, { status: 201 });
};
