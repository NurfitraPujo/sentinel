import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { handleAttachmentUpload, checkDeclaredLength } from '$lib/server/upload-core';
import { writeAgentAuditLog } from '$lib/server/agent-audit';

// Manual Issues M5 stage 2 (design §7 step 3): agent attachment upload, reusing the M2 upload
// core with uploader_type 'agent'. organizationId is NOT read from the body/form -- it comes
// straight from the credential (B7), same as every other /api/agent/* route.

export const POST: RequestHandler = async ({ request }) => {
	const ctx = await authenticateAgentRequest(request);

	checkDeclaredLength(request);

	const formData = await request.formData().catch(() => null);
	if (!formData) {
		throw error(400, 'Expected multipart/form-data');
	}

	const result = await handleAttachmentUpload({
		organizationId: ctx.organizationId,
		formData,
		uploaderType: 'agent',
		uploaderId: ctx.agentId,
	});

	await writeAgentAuditLog(ctx, 'agent.attachment.uploaded', 'attachment', result.id, {
		filename: result.filename,
		sizeBytes: result.sizeBytes,
	});

	return json(result, { status: 201 });
};
