import { describe, it, expect } from 'vitest';
import {
	isForbiddenCrossSiteFormPost,
	isCsrfExemptPath,
	isFormContentType,
	crossSiteForbiddenResponse,
	CSRF_EXEMPT_PATHS,
} from './csrf';

/**
 * `svelte.config.js` sets `kit.csrf.checkOrigin: false`, so this file IS the app's cross-site
 * form-post protection now. These tests exist to prove two things at once, and the second is the
 * one that would otherwise rot silently:
 *
 *  1. sentinel-user-report-issue §4.5's intended caller — a service sending `multipart/form-data`
 *     with NO `Origin` header — reaches `/api/report/attachments`.
 *  2. Nothing else got quieter. Every case SvelteKit's own check rejected is still rejected here:
 *     a missing origin, a foreign origin, each guarded method, each form enctype.
 *
 * The predicate is deliberately origin-string-in, boolean-out so it is testable without a request
 * event, a build, or an environment. `hooks.server.ts`'s `csrfHandle` is the only thing that adds
 * the `dev` gate, matching upstream's `if (!DEV)`.
 */

const ORIGIN = 'https://sentinel.example.com';

function req(init: {
	method?: string;
	contentType?: string | null;
	origin?: string | null;
	accept?: string;
	url?: string;
}): Request {
	const headers = new Headers();
	if (init.contentType !== null) headers.set('content-type', init.contentType ?? 'multipart/form-data');
	if (init.origin) headers.set('origin', init.origin);
	if (init.accept) headers.set('accept', init.accept);
	return new Request(init.url ?? `${ORIGIN}/api/uploads`, {
		method: init.method ?? 'POST',
		headers,
	});
}

describe('isForbiddenCrossSiteFormPost — what SvelteKit used to reject, still rejected', () => {
	it('rejects a form POST with NO Origin header', () => {
		expect(isForbiddenCrossSiteFormPost(req({}), ORIGIN)).toBe(true);
	});

	it('rejects a form POST from a foreign origin', () => {
		expect(isForbiddenCrossSiteFormPost(req({ origin: 'https://evil.example' }), ORIGIN)).toBe(true);
	});

	it.each(['POST', 'PUT', 'PATCH', 'DELETE'])('rejects an Origin-less %s', (method) => {
		expect(isForbiddenCrossSiteFormPost(req({ method }), ORIGIN)).toBe(true);
	});

	it.each([
		'application/x-www-form-urlencoded',
		'multipart/form-data',
		'text/plain',
		'application/x-sveltekit-formdata',
	])('rejects an Origin-less POST with enctype %s', (contentType) => {
		expect(isForbiddenCrossSiteFormPost(req({ contentType }), ORIGIN)).toBe(true);
	});

	it('matches the content type case-insensitively and ignores parameters', () => {
		expect(
			isForbiddenCrossSiteFormPost(req({ contentType: 'Multipart/Form-Data; boundary=x' }), ORIGIN)
		).toBe(true);
	});
});

describe('isForbiddenCrossSiteFormPost — what it must NOT reject', () => {
	it('allows a same-origin form POST', () => {
		expect(isForbiddenCrossSiteFormPost(req({ origin: ORIGIN }), ORIGIN)).toBe(false);
	});

	it('allows an Origin-less application/json POST — not a form enctype, never was guarded', () => {
		expect(isForbiddenCrossSiteFormPost(req({ contentType: 'application/json' }), ORIGIN)).toBe(false);
	});

	it.each(['GET', 'HEAD', 'OPTIONS'])('allows %s', (method) => {
		expect(isForbiddenCrossSiteFormPost(req({ method }), ORIGIN)).toBe(false);
	});

	it('allows a request with no content-type at all', () => {
		expect(isForbiddenCrossSiteFormPost(req({ contentType: null }), ORIGIN)).toBe(false);
	});
});

describe('isCsrfExemptPath — the §4.1/§4.5 intake endpoints, and nothing near them', () => {
	it.each(['/api/report', '/api/report/attachments', '/api/report/', '/api/report/attachments/'])(
		'exempts %s',
		(path) => {
			expect(isCsrfExemptPath(path)).toBe(true);
		}
	);

	it.each([
		'/api/reports',
		'/api/report/attachments/abc',
		'/api/reportx',
		'/api/uploads',
		'/api/agent/uploads',
		'/',
		'/acme/issues',
	])('does NOT exempt %s', (path) => {
		expect(isCsrfExemptPath(path)).toBe(false);
	});

	it('exempts exactly the two intake paths and no others', () => {
		expect([...CSRF_EXEMPT_PATHS].sort()).toEqual(['/api/report', '/api/report/attachments']);
	});
});

describe('the service caller §4.5 exists for', () => {
	it('is rejected by the predicate but exempted by path — which is the whole point', () => {
		// A Go/Node service POSTing a screenshot: multipart, X-API-Key, no Origin, no cookie.
		const upload = req({ url: `${ORIGIN}/api/report/attachments` });
		expect(isForbiddenCrossSiteFormPost(upload, ORIGIN)).toBe(true);
		expect(isCsrfExemptPath(new URL(upload.url).pathname)).toBe(true);
	});
});

describe('crossSiteForbiddenResponse — byte-for-byte what upstream produced', () => {
	it('is 403 text/plain by default', async () => {
		const res = crossSiteForbiddenResponse(req({}));
		expect(res.status).toBe(403);
		expect(await res.text()).toBe('Cross-site POST form submissions are forbidden');
	});

	it('is 403 JSON when the client sent accept: application/json', async () => {
		const res = crossSiteForbiddenResponse(req({ method: 'DELETE', accept: 'application/json' }));
		expect(res.status).toBe(403);
		expect(await res.json()).toEqual({ message: 'Cross-site DELETE form submissions are forbidden' });
	});
});

describe('isFormContentType', () => {
	it('is true only for the four CSRF-capable enctypes', () => {
		for (const type of [
			'application/x-www-form-urlencoded',
			'multipart/form-data',
			'text/plain',
			'application/x-sveltekit-formdata',
		]) {
			expect(isFormContentType(req({ contentType: type }))).toBe(true);
		}
		for (const type of ['application/json', 'application/octet-stream', 'image/png']) {
			expect(isFormContentType(req({ contentType: type }))).toBe(false);
		}
	});
});
