import { describe, it, expect, vi, beforeEach } from 'vitest';
import { Blob as NodeBlob, File as NodeFile } from 'node:buffer';

// Same reason as reports.attachments.flow.integration.test.ts: this suite runs under jsdom, whose
// Blob/File do not implement `.arrayBuffer()` -- which is the first thing `handleAttachmentUpload`
// calls on the part. Node's own implementations do. No production effect: only `mode === 'test'`
// resolves the browser condition (vite.config.js).
// eslint-disable-next-line @typescript-eslint/no-explicit-any
(globalThis as any).Blob = NodeBlob;
// eslint-disable-next-line @typescript-eslint/no-explicit-any
(globalThis as any).File = NodeFile;

/**
 * The operator's §10 narrowing of sentinel-user-report-issue §4.5, taken 2026-09-04: the
 * external-reporter upload path admits IMAGES AND PDF ONLY, not the shared
 * `ALLOWED_CONTENT_TYPES`, which also admits `application/zip`, `application/msword`, DOCX,
 * `video/*` and `text/plain`.
 *
 * Two properties are asserted, and both matter:
 *
 *  1. `handleAttachmentUpload` rejects a type off the CALLER's list with `415` even when the
 *     sniffer recognised it — and rejects it AFTER the sniff, so `resolveContentType`'s one
 *     declared-type concession (a DOCX declared over real ZIP bytes) cannot slip past either.
 *     A `.png` whose magic bytes are a ZIP local-file header is the acceptance criterion's case
 *     and is asserted by name.
 *  2. The default is unchanged. `/api/uploads` and `/api/agent/uploads` pass no list and keep
 *     every type they had — narrowing for the stranger must not narrow for the org member.
 *
 * `putObject` is asserted NOT to have been called on every rejection: a 415 that has already
 * written the object to MinIO is a rejection in the response only.
 */

const putObject = vi.fn(async () => {});
const insertedRows: any[] = [];

const dbMock: any = {
	insert: vi.fn(() => dbMock),
	values: vi.fn((row: unknown) => {
		insertedRows.push(row);
		return dbMock;
	}),
	returning: vi.fn(async () => [
		{
			id: 'att-1',
			filename: 'screen.png',
			contentType: (insertedRows.at(-1) as any)?.contentType,
			sizeBytes: (insertedRows.at(-1) as any)?.sizeBytes,
		},
	]),
};

vi.mock('$lib/server/db', () => ({ db: dbMock }));
vi.mock('$lib/server/storage', () => ({
	isStorageConfigured: () => true,
	putObject,
	createPresignedPutUrl: vi.fn(),
	headObject: vi.fn(),
	getObjectRangeBytes: vi.fn(),
	deleteObject: vi.fn(),
}));
vi.mock('$lib/server/attachment-reaper', () => ({ reapOrgOrphanAttachments: vi.fn(async () => {}) }));

const { handleAttachmentUpload } = await import('./upload-core');
const { REPORT_ALLOWED_CONTENT_TYPES, ALLOWED_CONTENT_TYPES } = await import('./attachment-sniff');

/** A file whose leading bytes are `signature`, padded so nothing trips a short-buffer guard. */
function fileWith(signature: number[], name: string, declaredType: string): File {
	const buf = new Uint8Array(64);
	buf.set(signature);
	return new File([buf], name, { type: declaredType });
}

const PNG = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];
const JPEG = [0xff, 0xd8, 0xff];
const GIF = [0x47, 0x49, 0x46, 0x38, 0x39, 0x61];
const PDF = [0x25, 0x50, 0x44, 0x46, 0x2d];
const ZIP = [0x50, 0x4b, 0x03, 0x04];
const OLE_DOC = [0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1];
const MATROSKA = [0x1a, 0x45, 0xdf, 0xa3];

function webp(): File {
	const buf = new Uint8Array(64);
	buf.set([0x52, 0x49, 0x46, 0x46]);
	buf.set([0x57, 0x45, 0x42, 0x50], 8);
	return new File([buf], 'shot.webp', { type: 'image/webp' });
}

function text(): File {
	return new File([new TextEncoder().encode('plain log line\n')], 'log.txt', { type: 'text/plain' });
}

/**
 * A `FormData` stand-in backed by a Map. jsdom's own `FormData` re-wraps an appended Blob into a
 * jsdom `File`, undoing the Node swap above; the core only ever calls `.get()`, so a Map is both
 * enough and honest about what is being exercised.
 */
function formDataWith(file: File): FormData {
	const parts = new Map<string, unknown>([['file', file]]);
	return { get: (name: string) => parts.get(name) ?? null } as unknown as FormData;
}

async function upload(file: File, allowedContentTypes?: readonly any[]) {
	const formData = formDataWith(file);
	return handleAttachmentUpload({
		organizationId: 'org-1',
		formData,
		uploaderType: 'external_reporter',
		uploaderId: 'reporter-1',
		...(allowedContentTypes ? { allowedContentTypes } : {}),
	} as any);
}

async function statusOf(promise: Promise<unknown>): Promise<number | undefined> {
	try {
		await promise;
		return undefined;
	} catch (err: any) {
		return err?.status;
	}
}

beforeEach(() => {
	putObject.mockClear();
	insertedRows.length = 0;
});

describe('REPORT_ALLOWED_CONTENT_TYPES (§10 narrowing of §4.5)', () => {
	it('is a strict subset of the shared allowlist — it can only remove types, never add one', () => {
		for (const type of REPORT_ALLOWED_CONTENT_TYPES) {
			expect(ALLOWED_CONTENT_TYPES).toContain(type);
		}
		expect(REPORT_ALLOWED_CONTENT_TYPES.length).toBeLessThan(ALLOWED_CONTENT_TYPES.length);
	});

	it('is exactly images + PDF', () => {
		expect([...REPORT_ALLOWED_CONTENT_TYPES].sort()).toEqual(
			['application/pdf', 'image/gif', 'image/jpeg', 'image/png', 'image/webp'].sort()
		);
	});
});

describe('handleAttachmentUpload with the report allowlist', () => {
	it('accepts a PNG and stores the SNIFFED type', async () => {
		const result = await upload(fileWith(PNG, 'screen.png', 'image/png'), REPORT_ALLOWED_CONTENT_TYPES);
		expect(result.contentType).toBe('image/png');
		expect(putObject).toHaveBeenCalledTimes(1);
	});

	it.each([
		['JPEG', () => fileWith(JPEG, 'screen.jpg', 'image/jpeg'), 'image/jpeg'],
		['GIF', () => fileWith(GIF, 'screen.gif', 'image/gif'), 'image/gif'],
		['WEBP', () => webp(), 'image/webp'],
		['PDF', () => fileWith(PDF, 'report.pdf', 'application/pdf'), 'application/pdf'],
	])('accepts %s', async (_label, make, expected) => {
		const result = await upload(make() as File, REPORT_ALLOWED_CONTENT_TYPES);
		expect(result.contentType).toBe(expected);
	});

	it('rejects a .png whose magic bytes are a ZIP local-file header with 415, writing nothing', async () => {
		// Acceptance criterion 2's case. The sniffer resolves this to `application/zip`, which is on
		// the SHARED allowlist and off the report one.
		const status = await statusOf(
			upload(fileWith(ZIP, 'screen.png', 'image/png'), REPORT_ALLOWED_CONTENT_TYPES)
		);
		expect(status).toBe(415);
		expect(putObject).not.toHaveBeenCalled();
		expect(insertedRows).toHaveLength(0);
	});

	it('rejects a real ZIP declared as DOCX with 415 — the declared-type concession does not bypass it', async () => {
		// `resolveContentType` honours a declared DOCX over real ZIP bytes; the narrowing is applied
		// after it, so that concession cannot reintroduce an archive on this route.
		const status = await statusOf(
			upload(
				fileWith(
					ZIP,
					'evidence.docx',
					'application/vnd.openxmlformats-officedocument.wordprocessingml.document'
				),
				REPORT_ALLOWED_CONTENT_TYPES
			)
		);
		expect(status).toBe(415);
		expect(putObject).not.toHaveBeenCalled();
	});

	it.each([
		['a legacy OLE .doc', () => fileWith(OLE_DOC, 'notes.doc', 'application/msword')],
		['a Matroska/WebM video', () => fileWith(MATROSKA, 'clip.webm', 'video/webm')],
		['a plain text file', () => text()],
	])('rejects %s with 415', async (_label, make) => {
		const status = await statusOf(upload(make() as File, REPORT_ALLOWED_CONTENT_TYPES));
		expect(status).toBe(415);
		expect(putObject).not.toHaveBeenCalled();
	});
});

describe('handleAttachmentUpload without an allowlist (the session and agent routes)', () => {
	it.each([
		['a ZIP', () => fileWith(ZIP, 'bundle.zip', 'application/zip'), 'application/zip'],
		['a legacy .doc', () => fileWith(OLE_DOC, 'notes.doc', 'application/msword'), 'application/msword'],
		['a WebM video', () => fileWith(MATROSKA, 'clip.webm', 'video/webm'), 'video/webm'],
		['a text file', () => text(), 'text/plain'],
	])('still accepts %s — the narrowing is per caller, not global', async (_label, make, expected) => {
		const result = await upload(make() as File);
		expect(result.contentType).toBe(expected);
	});
});
