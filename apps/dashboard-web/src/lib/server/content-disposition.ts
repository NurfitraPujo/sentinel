/**
 * Manual Issues PR13 remediation R3 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md). Builds a
 * `Content-Disposition` header value that never throws regardless of what a user typed as an
 * attachment filename.
 *
 * `Headers.set` requires an ISO-8859-1-representable string (undici/Node's fetch implementation
 * enforces this) -- a filename containing e.g. Japanese characters made the previous
 * `filename="${safeFilename}"`-only header throw at `headers.set(...)`, a permanent 500 on
 * download for any attachment whose name wasn't ASCII. RFC 6266 + RFC 5987 fix this: emit BOTH
 * an ASCII-sanitized `filename=` fallback (for older/dumber clients) and a `filename*=UTF-8''...`
 * percent-encoded extended parameter (which every modern browser prefers when present).
 *
 * Backslashes are stripped from both forms -- unescaped, a backslash inside the quoted
 * `filename="..."` value is itself a syntax character (an escape introducer per RFC 6266's
 * `quoted-string` grammar) and can malform the header or, depending on the parser, let a crafted
 * filename break out of the quoted value.
 */
export function buildContentDisposition(disposition: 'inline' | 'attachment', filename: string): string {
	const noBackslashes = filename.replace(/\\/g, '');

	// ASCII fallback: strip quotes/control characters (already handled) and anything outside the
	// printable ASCII range, since a raw non-ASCII byte in the unquoted-parameter position is
	// exactly what made `headers.set` throw. Non-ASCII characters are dropped (not substituted)
	// -- the extended `filename*` parameter carries the real name for any client that honors it.
	const asciiFallback = noBackslashes
		.replace(/["\r\n]/g, '_')
		// eslint-disable-next-line no-control-regex
		.replace(/[^\x20-\x7e]/g, '_')
		.trim();
	const safeFallback = asciiFallback.length > 0 ? asciiFallback : 'attachment';

	const encodedExtended = encodeRFC5987(noBackslashes);

	return `${disposition}; filename="${safeFallback}"; filename*=UTF-8''${encodedExtended}`;
}

/** RFC 5987 `ext-value` percent-encoding: like encodeURIComponent, but also escapes the handful of
 *  characters RFC 5987's `attr-char` excludes that encodeURIComponent leaves alone. */
function encodeRFC5987(value: string): string {
	return encodeURIComponent(value).replace(
		/['()*]/g,
		(c) => `%${c.charCodeAt(0).toString(16).toUpperCase()}`
	);
}
