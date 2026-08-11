/**
 * Manual Issues M2 (docs/plans/MANUAL_ISSUES_DESIGN.md §4): a small magic-byte sniffer for the
 * upload content-type allowlist. Deliberately does NOT trust the client-supplied `Content-Type`
 * header/form field -- a client can label an executable "image.png" trivially, so acceptance is
 * decided by inspecting the first bytes of the buffer against known file signatures.
 *
 * Text-ish formats (txt/log) have no magic bytes at all -- they're accepted by exclusion: if the
 * sniffed signature doesn't match any known binary format AND the content is valid UTF-8 with no
 * NUL bytes, it's treated as text. Everything else must match a signature exactly.
 */

export type AllowedContentType =
	| 'image/png'
	| 'image/jpeg'
	| 'image/gif'
	| 'image/webp'
	| 'video/webm'
	| 'video/mp4'
	| 'application/pdf'
	| 'text/plain'
	| 'application/zip'
	| 'application/msword'
	| 'application/vnd.openxmlformats-officedocument.wordprocessingml.document';

export const ALLOWED_CONTENT_TYPES: readonly AllowedContentType[] = [
	'image/png',
	'image/jpeg',
	'image/gif',
	'image/webp',
	'video/webm',
	'video/mp4',
	'application/pdf',
	'text/plain',
	'application/zip',
	'application/msword',
	'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
];

function startsWith(buf: Uint8Array, bytes: number[], offset = 0): boolean {
	if (buf.length < offset + bytes.length) return false;
	for (let i = 0; i < bytes.length; i++) {
		if (buf[offset + i] !== bytes[i]) return false;
	}
	return true;
}

function isLikelyText(buf: Uint8Array): boolean {
	// No NUL bytes, and decodes as valid UTF-8 -- good enough to distinguish "log/text file" from
	// "binary blob mislabeled as text" without a full charset detector.
	if (buf.length === 0) return true;
	for (const byte of buf) {
		if (byte === 0) return false;
	}
	try {
		new TextDecoder('utf-8', { fatal: true }).decode(buf);
		return true;
	} catch {
		return false;
	}
}

/**
 * Sniffs `buf` (at minimum the first ~16 bytes; more is fine) and returns the detected content
 * type from the allowlist, or `null` if it matches nothing recognized/allowed. DOC (legacy
 * `.doc`, OLE Compound File) and DOCX/ZIP share the same outer signature family — DOCX is a ZIP
 * containing `[Content_Types].xml`, so it is reported as `application/zip` here; callers that
 * need to distinguish DOCX from a plain ZIP can fall back to the extension, but the security
 * property (real archive, not an arbitrary binary) already holds either way.
 */
export function sniffContentType(buf: Uint8Array): AllowedContentType | null {
	// PNG: 89 50 4E 47 0D 0A 1A 0A
	if (startsWith(buf, [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])) return 'image/png';

	// JPEG: FF D8 FF
	if (startsWith(buf, [0xff, 0xd8, 0xff])) return 'image/jpeg';

	// GIF: "GIF87a" or "GIF89a"
	if (startsWith(buf, [0x47, 0x49, 0x46, 0x38, 0x37, 0x61])) return 'image/gif';
	if (startsWith(buf, [0x47, 0x49, 0x46, 0x38, 0x39, 0x61])) return 'image/gif';

	// WEBP: "RIFF" .... "WEBP"
	if (startsWith(buf, [0x52, 0x49, 0x46, 0x46]) && startsWith(buf, [0x57, 0x45, 0x42, 0x50], 8)) {
		return 'image/webp';
	}

	// WEBM/MKV (Matroska/EBML): 1A 45 DF A3
	if (startsWith(buf, [0x1a, 0x45, 0xdf, 0xa3])) return 'video/webm';

	// MP4/ISO-BMFF: bytes 4-7 == "ftyp"
	if (startsWith(buf, [0x66, 0x74, 0x79, 0x70], 4)) return 'video/mp4';

	// PDF: "%PDF-"
	if (startsWith(buf, [0x25, 0x50, 0x44, 0x46, 0x2d])) return 'application/pdf';

	// Legacy DOC (OLE Compound File Binary): D0 CF 11 E0 A1 B1 1A E1
	if (startsWith(buf, [0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1])) return 'application/msword';

	// ZIP / DOCX / XLSX all share the local-file-header signature "PK\x03\x04" (or empty-archive
	// "PK\x05\x06", or spanned "PK\x07\x08"). DOCX detection down to the exact office subtype is
	// deliberately not attempted -- see doc comment above.
	if (
		startsWith(buf, [0x50, 0x4b, 0x03, 0x04]) ||
		startsWith(buf, [0x50, 0x4b, 0x05, 0x06]) ||
		startsWith(buf, [0x50, 0x4b, 0x07, 0x08])
	) {
		return 'application/zip';
	}

	if (isLikelyText(buf)) return 'text/plain';

	return null;
}

export function isAllowedContentType(value: unknown): value is AllowedContentType {
	return typeof value === 'string' && (ALLOWED_CONTENT_TYPES as readonly string[]).includes(value);
}

/**
 * Resolves the content type to actually store, given the sniffed (source-of-truth) type and the
 * client's declared one. `null` means reject the upload. For every family except the ZIP family
 * the two must match exactly the declared type is otherwise ignored entirely. The ZIP family is
 * the one case where magic bytes are ambiguous by design (DOCX is a ZIP): a declared DOCX type is
 * honored, for a more useful stored/download content type, but ONLY once the bytes have already
 * proven to be a real ZIP-family archive -- the declared value never bypasses the sniff.
 */
export function resolveContentType(
	detected: AllowedContentType | null,
	declared: string | undefined
): AllowedContentType | null {
	if (!detected) return null;

	if (detected === 'application/zip') {
		if (declared === 'application/vnd.openxmlformats-officedocument.wordprocessingml.document') {
			return declared;
		}
		return 'application/zip';
	}

	return detected;
}
