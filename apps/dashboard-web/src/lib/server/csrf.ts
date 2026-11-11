/**
 * SvelteKit's cross-site form-post check, re-implemented here so ONE route family can be exempt
 * from it.
 *
 * Why this file exists. `@sveltejs/kit`'s `respond()` runs a CSRF origin check
 * (`node_modules/@sveltejs/kit/src/runtime/server/respond.js`, the `options.csrf_check_origin`
 * branch) BEFORE `handle` and before any route: a `POST` whose `content-type` is a form type and
 * whose `Origin` header is absent or does not equal the server's own origin is answered
 * `403 text/plain "Cross-site POST form submissions are forbidden"`. That check has no
 * per-route escape hatch, and `csrf.trustedOrigins` does not help, because the rejection an
 * absent `Origin` earns is `!request_origin` — there is no origin to trust.
 *
 * sentinel-user-report-issue §4.5's `POST /api/report/attachments` is `multipart/form-data` sent
 * by a SERVICE holding an `X-API-Key`, not by a browser. A service HTTP client sends no `Origin`
 * header, so the endpoint's headline capability answered 403 to its only intended caller. §4.1's
 * `POST /api/report` is the same credential and the same caller.
 *
 * So `svelte.config.js` sets `kit.csrf.checkOrigin: false` and the check is re-implemented below,
 * verbatim, with the two intake paths exempt. The exemption is safe for exactly the reason the
 * global check is not sufficient here: those two routes are authenticated by a bearer-style
 * header credential that a cross-site form post cannot cause a browser to attach. They read no
 * cookie and no session; `report-auth.ts` reads `X-API-Key` and nothing else. Every other route
 * in this app keeps the identical protection it had.
 *
 * Kept deliberately close to the upstream source it replaces, so a future SvelteKit change to
 * `csrf_check_origin` is a readable diff against this file rather than a silent divergence.
 */

/**
 * Mirrors `is_form_content_type` (`@sveltejs/kit/src/utils/http.js`) — the enctypes a plain HTML
 * `<form>` can produce without a preflight, which is what makes them CSRF-capable — plus
 * `BINARY_FORM_CONTENT_TYPE` (`@sveltejs/kit/src/runtime/form-utils.js`), the enctype SvelteKit's
 * own remote-form submissions use.
 */
const FORM_CONTENT_TYPES: ReadonlySet<string> = new Set([
	'application/x-www-form-urlencoded',
	'multipart/form-data',
	'text/plain',
	'application/x-sveltekit-formdata',
]);

/** The methods upstream guards. `GET`/`HEAD`/`OPTIONS` are not form-submittable mutations. */
const GUARDED_METHODS: ReadonlySet<string> = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);

/**
 * Origins accepted in addition to the server's own. Empty, matching `svelte.config.js`, which
 * declares no `csrf.trustedOrigins`; it is a named constant so adding one has an obvious home
 * rather than being set in a config this check no longer reads.
 */
export const CSRF_TRUSTED_ORIGINS: readonly string[] = [];

/**
 * The paths that do NOT get the origin check: sentinel-user-report-issue's two `X-API-Key` intake
 * endpoints (§4.1, §4.5), and sentinel-sourcemaps §2.1's `POST /api/sourcemaps` — the same shape
 * of caller (a CI `curl` holding the project's ingest key sends no `Origin`) behind the same kind
 * of credential: a header a cross-site form post cannot make a browser attach, no cookie, no
 * session. Exact paths, never a prefix — `/api/reports`, `/api/report/anything-else` and
 * `/api/sourcemaps/anything` are not on this list and are protected exactly as before.
 */
export const CSRF_EXEMPT_PATHS: readonly string[] = ['/api/report', '/api/report/attachments', '/api/sourcemaps'];

/** Strips one trailing slash so `/api/report/` matches `/api/report`. */
function normalizePath(pathname: string): string {
	if (pathname.length > 1 && pathname.endsWith('/')) return pathname.slice(0, -1);
	return pathname;
}

export function isCsrfExemptPath(pathname: string): boolean {
	return CSRF_EXEMPT_PATHS.includes(normalizePath(pathname));
}

/** Mirrors upstream's `is_content_type` parameter handling: type only, case-insensitive. */
export function isFormContentType(request: Request): boolean {
	const type = request.headers.get('content-type')?.split(';', 1)[0].trim() ?? '';
	return FORM_CONTENT_TYPES.has(type.toLowerCase());
}

/**
 * The predicate itself, deliberately free of any `dev`/environment reading so it is testable as a
 * pure function. `origin` is the server's own origin (`event.url.origin`).
 */
export function isForbiddenCrossSiteFormPost(request: Request, origin: string): boolean {
	const requestOrigin = request.headers.get('origin');
	return (
		isFormContentType(request) &&
		GUARDED_METHODS.has(request.method) &&
		requestOrigin !== origin &&
		(!requestOrigin || !CSRF_TRUSTED_ORIGINS.includes(requestOrigin))
	);
}

/**
 * The response upstream produces, byte for byte: `application/json` when the client asked for it,
 * `text/plain` otherwise, both `403`.
 */
export function crossSiteForbiddenResponse(request: Request): Response {
	const message = `Cross-site ${request.method} form submissions are forbidden`;
	if (request.headers.get('accept') === 'application/json') {
		return new Response(JSON.stringify({ message }), {
			status: 403,
			headers: { 'Content-Type': 'application/json' },
		});
	}
	return new Response(message, { status: 403, headers: { 'Content-Type': 'text/plain' } });
}
