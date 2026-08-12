import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * M6 Feature A (docs/plans/M6_PRESIGNED_UPLOADS_AND_TOOLBAR_PLAN.md §Feature A): server-side
 * proof for the presigned large-upload path -- `createPresignedAttachment` (declared-type/size
 * validation before the object exists) and `finalizePresignedAttachment` (the real,
 * post-upload validation: headObject re-check + ranged-GET magic-byte sniff, with the object AND
 * row deleted on any rejection). Proved RED-FIRST: written before the finalize gates existed to
 * confirm each fails for the right reason before it is made to pass.
 */

const isStorageConfigured = vi.fn(() => true);
const createPresignedPutUrl = vi.fn(() => Promise.resolve('https://minio.local/signed-put'));
const headObject = vi.fn();
const getObjectRangeBytes = vi.fn();
const deleteObject = vi.fn(() => Promise.resolve());
const logError = vi.fn();

vi.mock('$lib/server/storage', () => ({
	isStorageConfigured,
	createPresignedPutUrl,
	headObject,
	getObjectRangeBytes,
	deleteObject,
	putObject: vi.fn(),
}));

vi.mock('$lib/server/observability/log', () => ({
	log: { error: logError, info: vi.fn(), warn: vi.fn() },
}));

vi.mock('$lib/server/attachment-reaper', () => ({
	reapOrgOrphanAttachments: vi.fn(() => Promise.resolve(0)),
}));

vi.mock('$lib/db/schema', () => ({
	attachments: {
		id: 'id',
		orgId: 'orgId',
		issueId: 'issueId',
		commentId: 'commentId',
		uploaderType: 'uploaderType',
		uploaderId: 'uploaderId',
		filename: 'filename',
		contentType: 'contentType',
		sizeBytes: 'sizeBytes',
		storageKey: 'storageKey',
		status: 'status',
	},
}));

function makeQueueableDb() {
	const insertReturning: unknown[][] = [];
	const selectResults: unknown[][] = [];
	const updateReturning: unknown[][] = [];
	const deleteCalls: unknown[] = [];

	const db: any = {
		insert: vi.fn(() => ({
			values: vi.fn(() => ({
				returning: vi.fn(() => Promise.resolve(insertReturning.shift() ?? [])),
			})),
		})),
		select: vi.fn(() => ({
			from: vi.fn(() => ({
				where: vi.fn(() => Promise.resolve(selectResults.shift() ?? [])),
			})),
		})),
		update: vi.fn(() => ({
			set: vi.fn(() => ({
				where: vi.fn(() => ({
					returning: vi.fn(() => Promise.resolve(updateReturning.shift() ?? [])),
				})),
			})),
		})),
		delete: vi.fn(() => ({
			where: vi.fn((...args: unknown[]) => {
				deleteCalls.push(args);
				return Promise.resolve([]);
			}),
		})),
	};

	return { db, insertReturning, selectResults, updateReturning, deleteCalls };
}

const { db: dbMock, insertReturning, selectResults, updateReturning, deleteCalls } = makeQueueableDb();
vi.mock('$lib/server/db', () => ({ db: dbMock }));

const { createPresignedAttachment, finalizePresignedAttachment, MAX_PRESIGNED_UPLOAD_BYTES } =
	await import('./upload-core');

function bodyMessage(err: unknown): string {
	const e = err as { body?: { message?: string }; message?: string };
	return e?.body?.message ?? e?.message ?? '';
}

beforeEach(() => {
	vi.clearAllMocks();
	isStorageConfigured.mockReturnValue(true);
	insertReturning.length = 0;
	selectResults.length = 0;
	updateReturning.length = 0;
	deleteCalls.length = 0;
});

describe('createPresignedAttachment', () => {
	it('rejects a non-allowlisted declared content type', async () => {
		await expect(
			createPresignedAttachment({
				organizationId: 'org-1',
				uploaderId: 'user-1',
				filename: 'evil.exe',
				declaredContentType: 'application/x-msdownload',
				sizeBytes: 1024,
			})
		).rejects.toThrow();
		expect(dbMock.insert).not.toHaveBeenCalled();
	});

	it('rejects an oversized declared sizeBytes', async () => {
		await expect(
			createPresignedAttachment({
				organizationId: 'org-1',
				uploaderId: 'user-1',
				filename: 'huge.mp4',
				declaredContentType: 'video/mp4',
				sizeBytes: MAX_PRESIGNED_UPLOAD_BYTES + 1,
			})
		).rejects.toThrow();
		expect(dbMock.insert).not.toHaveBeenCalled();
	});

	it('creates a pending row and returns a presigned URL for a valid request', async () => {
		insertReturning.push([{ id: 'att-1' }]);

		const result = await createPresignedAttachment({
			organizationId: 'org-1',
			uploaderId: 'user-1',
			filename: 'video.mp4',
			declaredContentType: 'video/mp4',
			sizeBytes: 100 * 1024 * 1024,
		});

		expect(result.attachmentId).toBe('att-1');
		expect(result.uploadUrl).toBe('https://minio.local/signed-put');
		// Content-Type is intentionally not signed into the presigned PUT (see storage.ts) -- only
		// the storage key and the expiry are passed.
		expect(createPresignedPutUrl).toHaveBeenCalledWith(
			expect.stringMatching(/^org\/org-1\//),
			expect.any(Number)
		);
	});
});

function pendingRow(overrides: Record<string, unknown> = {}) {
	return {
		id: 'att-1',
		orgId: 'org-1',
		issueId: null,
		commentId: null,
		uploaderType: 'user',
		uploaderId: 'user-1',
		filename: 'video.mp4',
		contentType: 'video/mp4',
		sizeBytes: 100 * 1024 * 1024,
		storageKey: 'org/org-1/att-1-key',
		status: 'pending',
		...overrides,
	};
}

describe('finalizePresignedAttachment', () => {
	it('returns 409 when the object was never uploaded (headObject throws)', async () => {
		selectResults.push([pendingRow()]);
		headObject.mockRejectedValueOnce(new Error('NotFound'));

		await expect(
			finalizePresignedAttachment({ attachmentId: 'att-1', organizationId: 'org-1', uploaderId: 'user-1' })
		).rejects.toThrow();
	});

	it('deletes the object and row and returns 413 when the real size exceeds the cap', async () => {
		selectResults.push([pendingRow()]);
		headObject.mockResolvedValueOnce({ contentLength: MAX_PRESIGNED_UPLOAD_BYTES + 1 });

		const err = await finalizePresignedAttachment({
			attachmentId: 'att-1',
			organizationId: 'org-1',
			uploaderId: 'user-1',
		}).catch((e) => e);

		expect(bodyMessage(err)).toMatch(/byte cap/);
		expect(deleteObject).toHaveBeenCalledWith('org/org-1/att-1-key');
		expect(dbMock.delete).toHaveBeenCalledTimes(1);
		expect(getObjectRangeBytes).not.toHaveBeenCalled();
	});

	it('deletes the object and row and returns 415 on bad magic bytes', async () => {
		selectResults.push([pendingRow()]);
		headObject.mockResolvedValueOnce({ contentLength: 2048 });
		// Not a recognized signature and not valid UTF-8 text -- NUL byte forces rejection.
		getObjectRangeBytes.mockResolvedValueOnce(Buffer.from([0x00, 0x01, 0x02, 0x03]));

		const err = await finalizePresignedAttachment({
			attachmentId: 'att-1',
			organizationId: 'org-1',
			uploaderId: 'user-1',
		}).catch((e) => e);

		expect(bodyMessage(err)).toMatch(/not an allowed type/);
		expect(deleteObject).toHaveBeenCalledWith('org/org-1/att-1-key');
		expect(dbMock.delete).toHaveBeenCalledTimes(1);
		expect(dbMock.update).not.toHaveBeenCalled();
	});

	it('flips the row to ready with the sniffed content type and real size on the happy path', async () => {
		selectResults.push([pendingRow()]);
		headObject.mockResolvedValueOnce({ contentLength: 5000 });
		// MP4 signature: bytes 4-7 == "ftyp"
		const buf = Buffer.alloc(12);
		buf.write('ftyp', 4);
		getObjectRangeBytes.mockResolvedValueOnce(buf);
		updateReturning.push([
			{ id: 'att-1', filename: 'video.mp4', contentType: 'video/mp4', sizeBytes: 5000 },
		]);

		const result = await finalizePresignedAttachment({
			attachmentId: 'att-1',
			organizationId: 'org-1',
			uploaderId: 'user-1',
		});

		expect(result).toEqual({
			id: 'att-1',
			url: '/api/attachments/att-1',
			filename: 'video.mp4',
			contentType: 'video/mp4',
			sizeBytes: 5000,
		});
		expect(deleteObject).not.toHaveBeenCalled();
	});

	it('rejects finalizing an attachment belonging to a different org', async () => {
		selectResults.push([pendingRow({ orgId: 'org-OTHER' })]);

		await expect(
			finalizePresignedAttachment({ attachmentId: 'att-1', organizationId: 'org-1', uploaderId: 'user-1' })
		).rejects.toThrow();
		expect(headObject).not.toHaveBeenCalled();
	});

	it('rejects finalizing an attachment uploaded by a different user', async () => {
		selectResults.push([pendingRow({ uploaderId: 'user-OTHER' })]);

		await expect(
			finalizePresignedAttachment({ attachmentId: 'att-1', organizationId: 'org-1', uploaderId: 'user-1' })
		).rejects.toThrow();
		expect(headObject).not.toHaveBeenCalled();
	});

	it('rejects finalizing an already-ready attachment', async () => {
		selectResults.push([pendingRow({ status: 'ready' })]);

		await expect(
			finalizePresignedAttachment({ attachmentId: 'att-1', organizationId: 'org-1', uploaderId: 'user-1' })
		).rejects.toThrow();
		expect(headObject).not.toHaveBeenCalled();
	});

	it('rejects finalizing an attachment already linked to an issue', async () => {
		selectResults.push([pendingRow({ issueId: 'issue-1' })]);

		await expect(
			finalizePresignedAttachment({ attachmentId: 'att-1', organizationId: 'org-1', uploaderId: 'user-1' })
		).rejects.toThrow();
		expect(headObject).not.toHaveBeenCalled();
	});
});
