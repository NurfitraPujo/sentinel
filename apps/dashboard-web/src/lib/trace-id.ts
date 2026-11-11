/**
 * sentinel-cross-repo-trace-view D1 — a W3C / Sentry trace id is 32 hexadecimal characters. Not under
 * `$lib/server/` because the issue page's template uses it to decide whether an occurrence's trace
 * id is linkable (a stored id of another shape would link to a page that can only answer 404).
 */
const TRACE_ID_RE = /^[0-9a-f]{32}$/;

/** Lower-cases and validates; `null` means "not a trace id" (the trace route answers 404 for it). */
export function parseTraceId(raw: string | null | undefined): string | null {
	if (typeof raw !== 'string') return null;
	const id = raw.toLowerCase();
	return TRACE_ID_RE.test(id) ? id : null;
}

/**
 * Whether a STORED trace id is already in the form the trace page queries by. Ingest stores the
 * value raw (up to 64 chars, any case), while the trace page lower-cases its input and matches by
 * equality — so a stored `ABC…` or ` abc…` would link to a page that cannot show it, and a related
 * count that skips it. Links and counts are offered only for a canonical stored value; anything else
 * stays plain text, as before.
 */
export function isCanonicalTraceId(stored: string | null | undefined): stored is string {
	return typeof stored === 'string' && parseTraceId(stored) === stored;
}
