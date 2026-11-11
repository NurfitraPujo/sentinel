import crypto from 'crypto';

/**
 * sentinel-sourcemaps contract §4.4 step 1 / §4.1 `minified_path` (decision 22): the frame-path
 * normaliser, TypeScript half.
 *
 * ONE rule, two implementations -- this one stores `minified_path` on upload, and
 * `apps/processor-go/symbolicate/normalize.go: NormalizePath` looks a frame's `file` up with it.
 * The two are identity-affecting on both sides of the join `(project_id, release_version,
 * minified_path)`: a path this side stores as `/assets/x.js` that the Go side reads as
 * `/assets/x.js?v=1` is a map that never matches. So both pin to the SAME fixture file,
 * `tests/fixtures/sourcemaps/path-normalisation.json`, read at test time
 * (`sourcemap-path.test.ts`, `normalize_test.go`), and an edit to the rule is an edit to that file
 * first. Do not "fix" one side without the other.
 *
 * The rule, in full:
 *  (a) when the string starts with a scheme that is `http` or `https` (case-insensitive), then
 *      `://`, then a NON-EMPTY host (everything up to the next `/`, `?` or `#`, port included),
 *      drop the scheme and the host; a host with no path (`https://app.example.com`,
 *      `https://app.example.com?x=1`) becomes `/` -- Node's `URL.pathname` says `/` and Go's
 *      `url.Path` says empty, so the rule says `/` and both implementations are pinned to it;
 *  (b) then drop a `?…` suffix and a `#…` suffix, first occurrence, whatever the scheme;
 *  (c) everything else is byte-identical -- `webpack://ns/./src/x.ts`, `webpack:///./src/x.ts`,
 *      `file:///Users/a/x.js`, `blob:https://…`, `chrome-extension://abc/x.js`, a bare
 *      `/assets/x.js`, a bare `x.js` all pass (a) untouched.
 *
 * Deliberately NOT `new URL(s).pathname`: WHATWG parsing percent-encodes, lower-cases the host it
 * then discards, collapses `\` to `/` on special schemes and throws on a bare path -- none of
 * which the Go side does, and decision 22 is "byte-identical" outside (a) and (b).
 */
export function normalizeMinifiedPath(s: string): string {
	let out = s;
	const stripped = stripHttpSchemeAndHost(out);
	if (stripped !== null) out = stripped;
	// (b): the query and the fragment go whichever comes first -- `/a.js?x=#y` and `/a.js#y?x` both
	// cut at the first of the two, which is what "first occurrence" of each means once both are
	// applied in either order.
	const cut = firstIndexOfAny(out, '?#');
	if (cut >= 0) out = out.slice(0, cut);
	return out;
}

/**
 * (a). Returns the remainder starting at the first `/`, `?` or `#` after the host (`/` when the
 * host is the whole string), or null when (a) does not apply -- another scheme, `http:///x` with
 * an empty host, a bare path -- so the caller leaves the string byte-identical.
 */
function stripHttpSchemeAndHost(s: string): string | null {
	const sep = '://';
	const i = s.indexOf(sep);
	if (i <= 0) return null;
	const scheme = s.slice(0, i).toLowerCase();
	if (scheme !== 'http' && scheme !== 'https') return null;
	const afterScheme = s.slice(i + sep.length);
	const hostEnd = firstIndexOfAny(afterScheme, '/?#');
	if (hostEnd === 0) {
		// `http:///x` -- an empty host is not a host, so (a) does not apply.
		return null;
	}
	if (hostEnd < 0) {
		// `https://app.example.com` -- a host and nothing after it; `https://` -- nothing at all.
		return afterScheme === '' ? null : '/';
	}
	const rest = afterScheme.slice(hostEnd);
	// `https://app.example.com?x=1` / `…#f`: the host ends at the `?`/`#`; the path is `/`, and (b)
	// then drops the suffix.
	return rest[0] === '/' ? rest : `/${rest}`;
}

function firstIndexOfAny(s: string, chars: string): number {
	for (let i = 0; i < s.length; i++) {
		if (chars.includes(s[i])) return i;
	}
	return -1;
}

/** The first 16 hex characters of SHA-256 over the string -- `h16` in contract §4.1 / decision 17. */
export function h16(value: string): string {
	return crypto.createHash('sha256').update(value).digest('hex').slice(0, 16);
}

/** SHA-256 hex over raw bytes -- `files[].sha256` and the storage key's content half. */
export function sha256Hex(bytes: Uint8Array): string {
	return crypto.createHash('sha256').update(bytes).digest('hex');
}

/**
 * §4.1 phase 2 / decisions 5 and 17: the object key
 * `sourcemaps/{project_id}/{h16(release_version)}/{h16(minified_path)}-{sha256}.map`.
 *
 * The user-supplied path and version are NEVER in the key as strings, so a `/../` in either
 * cannot reach another prefix (D5); folding the path in (decision 17) means each row owns exactly
 * one object -- identical bytes under two paths are two objects, never one the UNIQUE
 * `storage_key` index would refuse -- and a `replaced` cleanup is a plain delete of the old key.
 * `minifiedPath` is the NORMALISED path (the stored one), so the key is a function of the row.
 */
export function sourcemapStorageKey(
	projectId: string,
	releaseVersion: string,
	minifiedPath: string,
	sha256: string
): string {
	return `sourcemaps/${projectId}/${h16(releaseVersion)}/${h16(minifiedPath)}-${sha256}.map`;
}
