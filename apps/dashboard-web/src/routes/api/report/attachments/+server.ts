import type { RequestHandler } from './$types';
import { env } from '$env/dynamic/private';
import {
	authenticateReportRequest,
	reportErrorResponse,
	toReportError,
	type ReportAuthContext,
} from '$lib/server/report-auth';
import {
	handleAttachmentUpload,
	checkDeclaredLength,
	checkReportUploadBudget,
	resolveReportUploadBytesPerDay,
} from '$lib/server/upload-core';
import { REPORT_ALLOWED_CONTENT_TYPES } from '$lib/server/attachment-sniff';
import { upsertExternalReporter } from '$lib/db/queries/external-reporters';
import { checkReportUploadRateLimit, resolveReportUploadRpm } from '$lib/rate-limit';
import { log } from '$lib/server/observability/log';

/**
 * sentinel-user-report-issue contract §4.5 -- `POST /api/report/attachments`: the screenshot,
 * uploaded BEFORE the report itself, so binary never travels inside §4.1's JSON body.
 *
 * Under OQ-1 variant (b) this is hosted by dashboard-web; the path is `/api/report/attachments`
 * rather than `/report/attachments` because `api` is in `hooks.server.ts`'s `reservedRoutes` and a
 * bare `/report` would be parsed as an organization slug by the `[orgSlug]` catch-all.
 *
 * The row it writes is a DRAFT -- `issue_id` and `comment_id` NULL -- claimed onto the issue by
 * `claimDraftAttachments` inside §4.1's transaction. An unclaimed draft is swept at 24 h by the
 * existing orphan reaper. §8: this endpoint is deliberately NOT idempotent; a retried upload
 * creates a second draft and the unreferenced one is reaped.
 *
 * It reuses `handleAttachmentUpload` + `checkDeclaredLength` (25 MB `MAX_UPLOAD_BYTES`, the
 * magic-byte allowlist in `attachment-sniff.ts`, the MinIO write, the opportunistic reap) and
 * explicitly NOT `createPresignedAttachment` -- §1 keeps the 500 MB direct-to-storage path
 * session-only.
 *
 * The content-type allowlist it passes is NARROWER than §4.5 as drafted: images and PDF only
 * (`REPORT_ALLOWED_CONTENT_TYPES`), per the operator's §10 narrowing of 2026-09-04. The shared
 * `ALLOWED_CONTENT_TYPES` admits `application/zip`, `application/msword`, DOCX, `video/*` and
 * `text/plain`, all of which this caller -- a stranger behind a service's key -- has no reason to
 * send and every reason to be refused.
 *
 * Every non-2xx here carries §7's envelope, which is why nothing in this file throws SvelteKit's
 * `error()`: that helper produces `{"message": ...}`.
 */

// §4.1's reporter bounds, which §4.5 carries as form fields because the reporter row is created
// or found at UPLOAD time -- upload and report must agree on which reporter this is.
const MAX_EXTERNAL_ID = 255;
const MAX_DISPLAY_NAME = 255;
const MAX_EMAIL = 320;

export const POST: RequestHandler = async ({ request }) => {
	let ctx: ReportAuthContext;
	try {
		ctx = await authenticateReportRequest(request);
	} catch (err) {
		return toReportError(err);
	}

	try {
		// Before buffering, where the platform gives us the chance to (413/411/400).
		checkDeclaredLength(request);
	} catch (err) {
		return toReportError(err);
	}

	// §4.5: a dedicated REPORT_UPLOAD_RPM on top of the key's own rate_limit_rpm, checked before
	// the body is parsed so a flood costs as little as possible.
	const rpm = resolveReportUploadRpm(env.REPORT_UPLOAD_RPM);
	const perMinute = checkReportUploadRateLimit(ctx.keyId, rpm);
	if (!perMinute.allowed) {
		return reportErrorResponse(429, 'rate_limited', 'Upload rate limit exceeded.', {}, {
			'Retry-After': String(perMinute.retryAfter ?? 60),
		});
	}

	// A failure here is usually "not multipart", but adapter-node also rejects a body over
	// BODY_SIZE_LIMIT with a 413 at this exact point -- swallowing that into a 400 would report a
	// size rejection as a malformed one, so a thrown status is preserved and only a shapeless
	// failure becomes `invalid_request`.
	let formData: FormData;
	try {
		formData = await request.formData();
	} catch (err) {
		const status = (err as { status?: unknown })?.status;
		if (typeof status === 'number' && status !== 400) {
			return toReportError(err);
		}
		return reportErrorResponse(400, 'invalid_request', 'Expected multipart/form-data.', { field: 'body' });
	}

	// B7 / §4.5: there is deliberately no `organizationId` part. The credential is the only source
	// of the tenant, and a form field claiming otherwise is ignored, not honoured.
	const externalId = readField(formData, 'external_id');
	if (!externalId || externalId.length > MAX_EXTERNAL_ID) {
		return reportErrorResponse(400, 'invalid_request', 'external_id is required (1-255 characters).', {
			field: 'external_id',
		});
	}

	const displayName = readField(formData, 'display_name');
	if (displayName && displayName.length > MAX_DISPLAY_NAME) {
		return reportErrorResponse(400, 'invalid_request', 'display_name must be at most 255 characters.', {
			field: 'display_name',
		});
	}

	const email = readField(formData, 'email');
	if (email && email.length > MAX_EMAIL) {
		return reportErrorResponse(400, 'invalid_request', 'email must be at most 320 characters.', {
			field: 'email',
		});
	}

	const file = formData.get('file');
	if (!(file instanceof File)) {
		return reportErrorResponse(400, 'invalid_request', 'file is required.', { field: 'file' });
	}

	// SR-5: the durable ceiling. Postgres, rolling 24 h, this org's external-reporter rows only.
	// Counted BEFORE the object is written, and counting the incoming file, so the request being
	// decided cannot be the one that steps over.
	const budgetBytes = resolveReportUploadBytesPerDay(env.REPORT_UPLOAD_BYTES_PER_DAY);
	let budget;
	try {
		budget = await checkReportUploadBudget(ctx.organizationId, file.size, budgetBytes);
	} catch (err) {
		return toReportError(err);
	}
	if (!budget.allowed) {
		log.warn('report_uploads.budget_exceeded', {
			organizationId: ctx.organizationId,
			keyId: ctx.keyId,
			usedBytes: budget.usedBytes,
			budgetBytes: budget.budgetBytes,
			incomingBytes: file.size,
		});
		return reportErrorResponse(429, 'rate_limited', 'Daily upload byte budget exhausted.', {}, {
			'Retry-After': String(budget.retryAfter),
		});
	}

	// The reporter row is created or found HERE, from the same fields §4.1's `reporter` object
	// will carry, so the upload and the report agree on who this is (§4.5). It is an upsert on
	// (project_id, external_id), so a repeat reporter is one row seen twice.
	let reporterId: string;
	try {
		const reporter = await upsertExternalReporter({
			organizationId: ctx.organizationId,
			projectId: ctx.projectId,
			externalId,
			displayName,
			email,
		});
		reporterId = reporter.id;
	} catch (err) {
		log.error('report_uploads.reporter_upsert_failed', {
			organizationId: ctx.organizationId,
			projectId: ctx.projectId,
			error: err,
		});
		return reportErrorResponse(500, 'internal', 'Failed to record the reporter.');
	}

	let result;
	try {
		result = await handleAttachmentUpload({
			organizationId: ctx.organizationId,
			formData,
			uploaderType: 'external_reporter',
			uploaderId: reporterId,
			allowedContentTypes: REPORT_ALLOWED_CONTENT_TYPES,
		});
	} catch (err) {
		// 413 (over the cap after buffering), 415 (magic bytes off the allowlist), 400, 503 --
		// translated into §7's envelope rather than SvelteKit's `{"message": ...}`.
		return toReportError(err);
	}

	log.info('report_uploads.accepted', {
		attachmentId: result.id,
		organizationId: ctx.organizationId,
		projectId: ctx.projectId,
		externalReporterId: reporterId,
		contentType: result.contentType,
		sizeBytes: result.sizeBytes,
	});

	// §4.5's success body, snake_case per §3 (intake endpoints match /ingest's payload casing).
	return new Response(
		JSON.stringify({
			attachment_id: result.id,
			filename: result.filename,
			content_type: result.contentType,
			size_bytes: result.sizeBytes,
		}),
		{ status: 201, headers: { 'Content-Type': 'application/json' } }
	);
};

/** Reads one text part, trimmed; returns null for absent, non-string or empty-after-trim. */
function readField(formData: FormData, name: string): string | null {
	const value = formData.get(name);
	if (typeof value !== 'string') return null;
	const trimmed = value.trim();
	return trimmed.length === 0 ? null : trimmed;
}
