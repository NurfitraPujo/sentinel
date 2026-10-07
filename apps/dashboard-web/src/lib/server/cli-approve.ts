/**
 * sentinel-cli-browser-login: helpers for the /cli/approve page that must be importable by tests and by the sign-in page
 * (a route module may export only its handlers, B12).
 */

const APPROVE_PATH = '/cli/approve';
const CODE_PARAM_RE = /^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$/;

/**
 * The ONLY post-sign-in destinations `/signin?next=` accepts: `/cli/approve`, optionally with a well-formed `code`.
 * Anything else -- an absolute URL, `//host`, another path, extra parameters -- yields null, so `next` can never become an
 * open redirect.
 */
export function safeCliApproveNext(next: unknown): string | null {
	if (typeof next !== 'string') return null;
	if (next === APPROVE_PATH) return next;
	const prefix = `${APPROVE_PATH}?code=`;
	if (!next.startsWith(prefix)) return null;
	let code: string;
	try {
		code = decodeURIComponent(next.slice(prefix.length));
	} catch {
		// A malformed escape (`?code=%`) is an invalid `next`, not a 500.
		return null;
	}
	return CODE_PARAM_RE.test(code) ? `${prefix}${code}` : null;
}

/** `/signin?next=/cli/approve?code=...` for a signed-out visitor; `code` must already be normalized (8 letters) or null. */
export function cliApproveSignInUrl(code: string | null): string {
	const next = code ? `${APPROVE_PATH}?code=${code.slice(0, 4)}-${code.slice(4)}` : APPROVE_PATH;
	return `/signin?next=${encodeURIComponent(next)}`;
}

/**
 * The approve/deny actions' own CSRF check, independent of the hook (which upstream-style skips in dev): the request must
 * carry an Origin header equal to this server's origin. A browser always sends Origin on a POST form submission.
 */
export function approveRequestIsSameOrigin(request: Request, origin: string): boolean {
	return request.headers.get('origin') === origin;
}

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** The approve action's `organizationId`, or null unless it is a UUID -- checked BEFORE it reaches a uuid column (else 500). */
export function parseApproveOrganizationId(value: unknown): string | null {
	return typeof value === 'string' && UUID_RE.test(value) ? value : null;
}

/**
 * The approval page and its sign-in hop must never render inside a frame: a framed approve button is a clickjacking
 * target ("click here to continue" over an invisible Approve). Both headers, for old and new browsers.
 */
export const FRAME_GUARD_HEADERS = { 'Content-Security-Policy': "frame-ancestors 'none'", 'X-Frame-Options': 'DENY' } as const;

/** True for `/cli/approve` and `/signin`, their trailing-slash forms and their `__data.json` / action sub-requests. */
export function needsFrameGuard(pathname: string): boolean {
	return /^\/(cli\/approve|signin)(\/.*)?$/.test(pathname);
}

/** Adds FRAME_GUARD_HEADERS to a response (merging into an existing CSP); never throws on an immutable response. */
export function applyFrameGuard(headers: Headers): void {
	try {
		const csp = headers.get('content-security-policy');
		if (!csp) headers.set('Content-Security-Policy', FRAME_GUARD_HEADERS['Content-Security-Policy']);
		else if (!/frame-ancestors/i.test(csp)) headers.set('Content-Security-Policy', `${csp}; frame-ancestors 'none'`);
		headers.set('X-Frame-Options', FRAME_GUARD_HEADERS['X-Frame-Options']);
	} catch {
		// immutable headers (a proxied fetch Response): nothing to do here, and never worth a 500
	}
}
