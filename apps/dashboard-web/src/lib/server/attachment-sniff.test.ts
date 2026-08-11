import { describe, it, expect } from 'vitest';
import { sniffContentType, resolveContentType, isAllowedContentType } from './attachment-sniff';

function bytes(...vals: number[]): Uint8Array {
	return new Uint8Array(vals);
}

describe('sniffContentType', () => {
	it('detects PNG by its 8-byte signature', () => {
		expect(sniffContentType(bytes(0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0))).toBe(
			'image/png'
		);
	});

	it('detects JPEG', () => {
		expect(sniffContentType(bytes(0xff, 0xd8, 0xff, 0xe0, 0, 0))).toBe('image/jpeg');
	});

	it('detects GIF87a and GIF89a', () => {
		expect(sniffContentType(bytes(0x47, 0x49, 0x46, 0x38, 0x37, 0x61))).toBe('image/gif');
		expect(sniffContentType(bytes(0x47, 0x49, 0x46, 0x38, 0x39, 0x61))).toBe('image/gif');
	});

	it('detects WEBP (RIFF....WEBP)', () => {
		const buf = bytes(0x52, 0x49, 0x46, 0x46, 0, 0, 0, 0, 0x57, 0x45, 0x42, 0x50);
		expect(sniffContentType(buf)).toBe('image/webp');
	});

	it('does not misdetect a bare RIFF (e.g. WAV) as WEBP', () => {
		const buf = bytes(0x52, 0x49, 0x46, 0x46, 0, 0, 0, 0, 0x57, 0x41, 0x56, 0x45);
		expect(sniffContentType(buf)).not.toBe('image/webp');
	});

	it('detects WEBM (EBML)', () => {
		expect(sniffContentType(bytes(0x1a, 0x45, 0xdf, 0xa3, 0, 0))).toBe('video/webm');
	});

	it('detects MP4 (ftyp box)', () => {
		const buf = bytes(0, 0, 0, 0x18, 0x66, 0x74, 0x79, 0x70, 0x69, 0x73, 0x6f, 0x6d);
		expect(sniffContentType(buf)).toBe('video/mp4');
	});

	it('detects PDF', () => {
		expect(sniffContentType(new TextEncoder().encode('%PDF-1.4\n'))).toBe('application/pdf');
	});

	it('detects legacy DOC (OLE compound file)', () => {
		expect(
			sniffContentType(bytes(0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1, 0, 0))
		).toBe('application/msword');
	});

	it('detects ZIP-family archives (DOCX/XLSX/plain ZIP all share the signature)', () => {
		expect(sniffContentType(bytes(0x50, 0x4b, 0x03, 0x04, 0, 0))).toBe('application/zip');
	});

	it('falls back to text/plain for valid UTF-8 with no NUL bytes', () => {
		expect(sniffContentType(new TextEncoder().encode('hello world\nlog line'))).toBe('text/plain');
	});

	it('rejects binary junk that matches no known signature and is not valid text', () => {
		// Invalid UTF-8 continuation byte with no leading byte, plus a NUL, matches nothing.
		expect(sniffContentType(bytes(0xff, 0xfe, 0x00, 0x01, 0x02, 0x03))).toBeNull();
	});

	it('rejects an executable mislabeled with an image extension (magic bytes win)', () => {
		// MZ header (Windows PE/DOS executable) -- not in any allowlisted family.
		const buf = bytes(0x4d, 0x5a, 0x90, 0x00, 0x03, 0x00, 0x00, 0x00, 0, 0);
		expect(sniffContentType(buf)).toBeNull();
	});
});

describe('resolveContentType', () => {
	it('rejects when nothing was detected', () => {
		expect(resolveContentType(null, 'image/png')).toBeNull();
	});

	it('ignores the declared type outside the ZIP family -- detected always wins', () => {
		expect(resolveContentType('image/png', 'application/pdf')).toBe('image/png');
	});

	it('upgrades a ZIP-family detection to DOCX when declared as DOCX', () => {
		expect(
			resolveContentType(
				'application/zip',
				'application/vnd.openxmlformats-officedocument.wordprocessingml.document'
			)
		).toBe('application/vnd.openxmlformats-officedocument.wordprocessingml.document');
	});

	it('keeps a ZIP-family detection as plain zip when declared type is not DOCX', () => {
		expect(resolveContentType('application/zip', 'application/octet-stream')).toBe(
			'application/zip'
		);
		expect(resolveContentType('application/zip', undefined)).toBe('application/zip');
	});
});

describe('isAllowedContentType', () => {
	it('accepts every allowlisted value', () => {
		expect(isAllowedContentType('image/png')).toBe(true);
		expect(isAllowedContentType('application/pdf')).toBe(true);
	});

	it('rejects anything not on the allowlist', () => {
		expect(isAllowedContentType('application/octet-stream')).toBe(false);
		expect(isAllowedContentType(123)).toBe(false);
		expect(isAllowedContentType(undefined)).toBe(false);
	});
});
