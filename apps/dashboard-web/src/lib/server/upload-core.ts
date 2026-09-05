import { error } from '@sveltejs/kit';
import { randomUUID } from 'node:crypto';
import { and, eq, gte, sql } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { attachments } from '$lib/db/schema';
import {
	isStorageConfigured,
	putObject,
	createPresignedPutUrl,
	headObject,
	getObjectRangeBytes,
	deleteObject,
} from '$lib/server/storage';
import {
	sniffContentType,
	resolveContentType,
	isAllowedContentType,
	ALLOWED_CONTENT_TYPES,
	type AllowedContentType,
} from '$lib/server/attachment-sniff';
import { reapOrgOrphanAttachments } from '$lib/server/attachment-reaper';
import { log } from '$lib/server/observability/log';

/**
 * Manual Issues M2 (design §4) core + M5 stage 2 (design §7 step 3: "POST /api/uploads (agent
 * attachment: reuse the upload path with uploader_type 'agent')"). Extracted from the M2
 * `/api/uploads` route so the session-authenticated and agent-authenticated routes share the
 * exact same validation (25 MB cap, magic-byte sniffing, storage write, opportunistic reap)
 * instead of two copies drifting apart. Callers own their own auth/authorization check and
 * multipart parsing; this function only handles the parts common to both.
 */

export const MAX_UPLOAD_BYTES = 25 * 1024 * 1024; // 25 MB cap (§4/Q4)

/**
 * `attachments.uploader_type`. `'external_reporter'` is added by the §4.6 migration's
 * catalog-guarded swap of `attachments_uploader_type_check`; the union here is the TypeScript
 * mirror of that CHECK, so a caller cannot invent a fourth value that the database would reject at
 * INSERT time.
 */
export type UploaderType = 'user' | 'agent' | 'external_reporter';

export interface UploadCoreInput {
	organizationId: string;
	formData: FormData;
	uploaderType: UploaderType;
	uploaderId: string;
	/**
	 * The content types this CALLER admits, defaulting to the full `ALLOWED_CONTENT_TYPES`.
	 *
	 * It is a per-caller parameter for the same reason the ingestor's scope allowlist became a
	 * per-chain one (contract §2): the session and agent upload routes and the external-reporter
	 * one do not have the same threat model, and one shared list means narrowing it for the
	 * stranger narrows it for the org member too. `POST /api/report/attachments` passes
	 * `REPORT_ALLOWED_CONTENT_TYPES` (images + PDF); everything else passes nothing and keeps the
	 * wider list. A value here can only ever REMOVE types — the sniffer is still the only thing
	 * that decides what the bytes are.
	 */
	allowedContentTypes?: readonly AllowedContentType[];
}

export interface UploadCoreResult {
	id: string;
	url: string;
	filename: string;
	contentType: string;
	sizeBytes: number;
}

export async function handleAttachmentUpload(input: UploadCoreInput): Promise<UploadCoreResult> {
	if (!isStorageConfigured()) {
		throw error(503, 'Object storage is not configured');
	}

	const file = input.formData.get('file');
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

	// The caller's own narrowing, applied AFTER the sniff so a declared type can never reach it.
	// A `.png` whose bytes are a ZIP header resolves to `application/zip` above and is rejected
	// here for the report route, which is exactly the case §4.5's abuse table is about.
	const allowed = input.allowedContentTypes ?? ALLOWED_CONTENT_TYPES;
	if (!allowed.includes(resolved)) {
		throw error(415, 'File content is not an allowed type (or does not match its declared type)');
	}

	const storageKey = `org/${input.organizationId}/${randomUUID()}`;

	await putObject(storageKey, buffer, resolved);

	const filename = (input.formData.get('filename') as string | null)?.trim() || file.name || 'upload';

	const [row] = await db
		.insert(attachments)
		.values({
			orgId: input.organizationId,
			issueId: null,
			commentId: null,
			uploaderType: input.uploaderType,
			uploaderId: input.uploaderId,
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
	reapOrgOrphanAttachments(input.organizationId).catch((err) => {
		log.error('uploads.opportunistic_reap_failed', { organizationId: input.organizationId, error: err });
	});

	return {
		id: row.id,
		url: `/api/attachments/${row.id}`,
		filename: row.filename,
		contentType: row.contentType,
		sizeBytes: row.sizeBytes,
	};
}

/**
 * Content-Length pre-check shared by both upload routes -- rejects before buffering when
 * possible.
 *
 * R8 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): `Number(declaredLength) > MAX_UPLOAD_BYTES`
 * silently PASSED a missing or non-numeric header -- `Number(null) > cap` and
 * `Number('not-a-number') > cap` (== `NaN > cap`) are both `false`, so a request with no
 * Content-Length, or a garbage one, sailed through this guard and was fully buffered
 * (`Buffer.from(await file.arrayBuffer())` in `handleAttachmentUpload`) before the `file.size`
 * check ever ran -- exactly the memory-abuse vector this function exists to stop.
 *
 * SvelteKit/undici give no hook to enforce a byte-counting limit on the incoming stream before
 * `request.formData()` fully buffers it (there is no public streaming multipart parser in this
 * stack) -- Content-Length is the only signal available before that buffering happens. The
 * strongest guard achievable here without a body-parsing rewrite is therefore to reject anything
 * that doesn't declare a numeric length, not just anything that declares an oversized one.
 */
export function checkDeclaredLength(request: Request): void {
	const declaredLength = request.headers.get('content-length');
	if (declaredLength === null || declaredLength.trim() === '') {
		throw error(411, 'Content-Length header is required');
	}
	const parsed = Number(declaredLength);
	if (!Number.isFinite(parsed) || parsed < 0) {
		throw error(400, 'Content-Length header must be a non-negative number');
	}
	if (parsed > MAX_UPLOAD_BYTES) {
		throw error(413, `File exceeds the ${MAX_UPLOAD_BYTES} byte cap`);
	}
}

/**
 * sentinel-user-report-issue contract §4.5 + SR-5: the per-organization daily byte budget for
 * external-reporter uploads.
 *
 * Enforced in POSTGRES, not Redis, and that is the whole point of it. The Redis limiter fails open
 * (VERIFIED_STATE.md S10) and this feature does not fix it (§1), so a budget that evaporates when
 * Redis does is not a budget. SR-5 settles the three readings §4.5's table cell allowed:
 *
 *   - the sum is over `org_id`, because `attachments` has no API-key column and §4.6 adds none,
 *     so a literal "per key" sum is not computable;
 *   - it is filtered to `uploader_type = 'external_reporter'`, so an org member's own screenshot
 *     through `/api/uploads` and an agent's through `/api/agent/uploads` do NOT consume any of the
 *     external reporters' allowance -- otherwise a busy dashboard team could start returning 429
 *     to a service that had uploaded nothing;
 *   - the window is a ROLLING 24 hours, not a calendar day, so no timezone question arises.
 *
 * The query is `(org_id, created_at)` + a filter, which is the shape `idx_attachments_org_created`
 * exists for.
 */
export const DEFAULT_REPORT_UPLOAD_BYTES_PER_DAY = 250 * 1024 * 1024; // 250 MB (§4.5)
export const REPORT_UPLOAD_WINDOW_MS = 24 * 60 * 60 * 1000;
export const EXTERNAL_REPORTER_UPLOADER_TYPE = 'external_reporter';

/**
 * Resolves `REPORT_UPLOAD_BYTES_PER_DAY`. An unset, unparseable or negative value falls back to
 * the contract default rather than to "unlimited" -- a misconfigured ceiling must not become an
 * absent one. `0` is honoured as a real value (upload disabled), since that is a deliberate
 * setting and not a parse failure.
 */
export function resolveReportUploadBytesPerDay(raw: string | undefined): number {
	if (raw === undefined || raw.trim() === '') return DEFAULT_REPORT_UPLOAD_BYTES_PER_DAY;
	const parsed = Number(raw);
	if (!Number.isFinite(parsed) || parsed < 0) return DEFAULT_REPORT_UPLOAD_BYTES_PER_DAY;
	return Math.floor(parsed);
}

export interface ReportUploadBudgetUsage {
	/** Bytes already stored for this org by external reporters inside the window. */
	usedBytes: number;
	/** `created_at` of the oldest counted row, or null when nothing is counted. */
	oldestCreatedAt: Date | null;
}

/** The rolling-window sum SR-5 specifies. Exported so a test can assert the filter, not just the total. */
export async function sumExternalReporterUploadBytes(
	organizationId: string,
	windowStart: Date
): Promise<ReportUploadBudgetUsage> {
	const [row] = await db
		.select({
			total: sql<string>`coalesce(sum(${attachments.sizeBytes}), 0)`,
			oldest: sql<Date | null>`min(${attachments.createdAt})`,
		})
		.from(attachments)
		.where(
			and(
				eq(attachments.orgId, organizationId),
				eq(attachments.uploaderType, EXTERNAL_REPORTER_UPLOADER_TYPE),
				gte(attachments.createdAt, windowStart)
			)
		);

	return {
		usedBytes: Number(row?.total ?? 0),
		oldestCreatedAt: row?.oldest ? new Date(row.oldest) : null,
	};
}

export interface ReportUploadBudgetVerdict {
	allowed: boolean;
	usedBytes: number;
	budgetBytes: number;
	/** Seconds until enough of the window ages out for a retry to have a chance. */
	retryAfter: number;
}

/**
 * Decides whether `incomingBytes` fits under the budget. The incoming file is counted: "over the
 * ceiling" has to include the request being decided, or a single 25 MB upload would always pass a
 * 1 MB budget exactly once.
 *
 * `retryAfter` is derived from when the OLDEST counted row leaves the rolling window, which is the
 * first moment the sum can actually drop -- not a made-up constant.
 */
export async function checkReportUploadBudget(
	organizationId: string,
	incomingBytes: number,
	budgetBytes: number,
	now: Date = new Date()
): Promise<ReportUploadBudgetVerdict> {
	const windowStart = new Date(now.getTime() - REPORT_UPLOAD_WINDOW_MS);
	const { usedBytes, oldestCreatedAt } = await sumExternalReporterUploadBytes(organizationId, windowStart);

	const allowed = usedBytes + incomingBytes <= budgetBytes;
	const retryAfterMs = oldestCreatedAt
		? oldestCreatedAt.getTime() + REPORT_UPLOAD_WINDOW_MS - now.getTime()
		: REPORT_UPLOAD_WINDOW_MS;

	return {
		allowed,
		usedBytes,
		budgetBytes,
		retryAfter: Math.max(1, Math.ceil(retryAfterMs / 1000)),
	};
}

/**
 * M6 Feature A (docs/plans/M6_PRESIGNED_UPLOADS_AND_TOOLBAR_PLAN.md §Feature A): presigned
 * direct-to-bucket path for files above the proxy cap. Declared type/size at presign time are
 * untrusted -- the real validation happens in `finalizePresignedAttachment` after the bytes
 * land, via a ranged GET + the exact same sniff/resolve allowlist the proxy path runs inline.
 */
export const MAX_PRESIGNED_UPLOAD_BYTES = 500 * 1024 * 1024; // 500 MB cap (§4/Q4)
export const SNIFF_BYTES = 4096; // enough for every signature in attachment-sniff.ts
const PRESIGN_EXPIRES_SECONDS = 15 * 60; // 15 min

export interface CreatePresignedAttachmentInput {
	organizationId: string;
	uploaderId: string;
	filename: string;
	declaredContentType: string;
	sizeBytes: number;
}

export interface CreatePresignedAttachmentResult {
	attachmentId: string;
	uploadUrl: string;
	expiresAt: string;
}

export async function createPresignedAttachment(
	input: CreatePresignedAttachmentInput
): Promise<CreatePresignedAttachmentResult> {
	if (!isStorageConfigured()) {
		throw error(503, 'Object storage is not configured');
	}

	if (!isAllowedContentType(input.declaredContentType)) {
		throw error(415, 'Content type is not an allowed type');
	}

	if (!Number.isFinite(input.sizeBytes) || input.sizeBytes <= 0) {
		throw error(400, 'sizeBytes must be a positive number');
	}
	if (input.sizeBytes > MAX_PRESIGNED_UPLOAD_BYTES) {
		throw error(413, `File exceeds the ${MAX_PRESIGNED_UPLOAD_BYTES} byte cap`);
	}

	const storageKey = `org/${input.organizationId}/${randomUUID()}`;
	const filename = input.filename.trim().slice(0, 512) || 'upload';

	const [row] = await db
		.insert(attachments)
		.values({
			orgId: input.organizationId,
			issueId: null,
			commentId: null,
			uploaderType: 'user',
			uploaderId: input.uploaderId,
			filename,
			contentType: input.declaredContentType,
			sizeBytes: input.sizeBytes,
			storageKey,
			status: 'pending',
		})
		.returning();

	if (!row) {
		throw error(500, 'Failed to record attachment');
	}

	const uploadUrl = await createPresignedPutUrl(storageKey, PRESIGN_EXPIRES_SECONDS);
	const expiresAt = new Date(Date.now() + PRESIGN_EXPIRES_SECONDS * 1000).toISOString();

	// Opportunistic per-org sweep, same as the proxy path.
	reapOrgOrphanAttachments(input.organizationId).catch((err) => {
		log.error('uploads.opportunistic_reap_failed', { organizationId: input.organizationId, error: err });
	});

	log.info('uploads.presign_created', {
		attachmentId: row.id,
		organizationId: input.organizationId,
		declaredContentType: input.declaredContentType,
		declaredSizeBytes: input.sizeBytes,
	});

	return { attachmentId: row.id, uploadUrl, expiresAt };
}

export interface FinalizePresignedAttachmentInput {
	attachmentId: string;
	organizationId: string;
	uploaderId: string;
}

export async function finalizePresignedAttachment(
	input: FinalizePresignedAttachmentInput
): Promise<UploadCoreResult> {
	if (!isStorageConfigured()) {
		throw error(503, 'Object storage is not configured');
	}

	const [row] = await db.select().from(attachments).where(eq(attachments.id, input.attachmentId));

	if (!row) {
		throw error(404, 'Attachment not found');
	}
	if (row.orgId !== input.organizationId) {
		throw error(404, 'Attachment not found');
	}
	if (row.uploaderType !== 'user' || row.uploaderId !== input.uploaderId) {
		throw error(403, 'Not the uploader of this attachment');
	}
	if (row.status !== 'pending') {
		throw error(409, 'Attachment is not pending');
	}
	if (row.issueId || row.commentId) {
		throw error(409, 'Attachment is already linked');
	}

	let contentLength: number;
	try {
		const head = await headObject(row.storageKey);
		contentLength = head.contentLength;
	} catch {
		throw error(409, 'upload not completed');
	}

	if (contentLength > MAX_PRESIGNED_UPLOAD_BYTES) {
		await deleteObject(row.storageKey);
		await db.delete(attachments).where(eq(attachments.id, row.id));
		log.warn('uploads.finalize_rejected', {
			attachmentId: row.id,
			organizationId: input.organizationId,
			reason: 'oversize',
			contentLength,
		});
		throw error(413, `File exceeds the ${MAX_PRESIGNED_UPLOAD_BYTES} byte cap`);
	}

	const buffer = await getObjectRangeBytes(row.storageKey, SNIFF_BYTES);
	const detected = sniffContentType(buffer);
	const resolved = resolveContentType(detected, row.contentType);

	if (!resolved) {
		await deleteObject(row.storageKey);
		await db.delete(attachments).where(eq(attachments.id, row.id));
		log.warn('uploads.finalize_rejected', {
			attachmentId: row.id,
			organizationId: input.organizationId,
			reason: 'disallowed_content',
			declaredContentType: row.contentType,
		});
		throw error(415, 'File content is not an allowed type (or does not match its declared type)');
	}

	const [updated] = await db
		.update(attachments)
		.set({ status: 'ready', contentType: resolved, sizeBytes: contentLength })
		.where(eq(attachments.id, row.id))
		.returning();

	if (!updated) {
		throw error(500, 'Failed to finalize attachment');
	}

	log.info('uploads.finalize_succeeded', {
		attachmentId: updated.id,
		organizationId: input.organizationId,
		contentType: updated.contentType,
		sizeBytes: updated.sizeBytes,
	});

	return {
		id: updated.id,
		url: `/api/attachments/${updated.id}`,
		filename: updated.filename,
		contentType: updated.contentType,
		sizeBytes: updated.sizeBytes,
	};
}
