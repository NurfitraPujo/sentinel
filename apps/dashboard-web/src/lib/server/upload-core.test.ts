import { describe, it, expect } from 'vitest';
import { checkDeclaredLength, MAX_UPLOAD_BYTES } from './upload-core';

// R8 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): `Number(declaredLength) > MAX_UPLOAD_BYTES`
// let a missing or NaN Content-Length sail through unchecked (`Number(null) > cap` and
// `NaN > cap` are both false), so the full request body still got buffered before any size check
// ran. This proves the fix rejects both, alongside the existing oversized-header case.
describe('checkDeclaredLength (R8)', () => {
	function requestWithLength(value: string | null): Request {
		const headers = new Headers();
		if (value !== null) headers.set('content-length', value);
		return new Request('http://localhost/api/uploads', { headers });
	}

	function bodyMessage(fn: () => void): string {
		try {
			fn();
			return '';
		} catch (err: any) {
			return err?.body?.message ?? err?.message ?? '';
		}
	}

	it('rejects a missing Content-Length header', () => {
		expect(() => checkDeclaredLength(requestWithLength(null))).toThrow();
		expect(bodyMessage(() => checkDeclaredLength(requestWithLength(null)))).toMatch(/Content-Length/);
	});

	it('rejects a non-numeric ("NaN") Content-Length header', () => {
		expect(() => checkDeclaredLength(requestWithLength('not-a-number'))).toThrow();
	});

	it('rejects a negative Content-Length header', () => {
		expect(() => checkDeclaredLength(requestWithLength('-5'))).toThrow();
	});

	it('rejects a Content-Length over the cap', () => {
		expect(() => checkDeclaredLength(requestWithLength(String(MAX_UPLOAD_BYTES + 1)))).toThrow();
		expect(
			bodyMessage(() => checkDeclaredLength(requestWithLength(String(MAX_UPLOAD_BYTES + 1))))
		).toMatch(/byte cap/);
	});

	it('allows a valid, in-range Content-Length', () => {
		expect(() => checkDeclaredLength(requestWithLength('1024'))).not.toThrow();
	});
});
