package symbolicate

import "strings"

// NormalizePath is sentinel-sourcemaps §4.4 step 1 (decision 22) -- the ONE frame-path rule shared
// by three call sites: the processor's lookup key (§4.4 step 3), the upload route's stored
// `minified_path` (§4.1, the TypeScript twin in apps/dashboard-web), and the fingerprint step's hash
// input (§4.4, fingerprint.Compute under the platform gate). All three must produce byte-identical
// output or the join between an uploaded map and a frame silently never matches, which is why both
// implementations are table-driven against tests/fixtures/sourcemaps/path-normalisation.json and
// nothing else.
//
// The rule, in the contract's words:
//
//	(a) when the string starts with a scheme that is `http` or `https` (case-insensitive), then
//	    `://`, then a NON-EMPTY host (everything up to the next `/`, `?` or `#`, port included),
//	    drop the scheme and the host; a host with no path becomes `/` -- Node's URL.pathname says
//	    `/` and Go's url.Path says "", so the rule says `/` and both sides are pinned to it;
//	(b) then drop a `?…` suffix and a `#…` suffix, first occurrence, whatever the scheme;
//	(c) everything else is byte-identical: `webpack://ns/./src/x.ts` (a host-shaped segment under a
//	    non-http scheme), `webpack:///./src/x.ts`, `file:///Users/a/x.js`,
//	    `blob:https://app.example.com/uuid`, `chrome-extension://abc/x.js`, a bare `/assets/x.js`
//	    and a bare `x.js` all pass (a) untouched.
//
// Deliberately NOT net/url: url.Parse would percent-decode, lower-case the host, collapse nothing
// and refuse strings a browser happily reports (`webpack:///./x` parses, `blob:https://…` does not
// round-trip), and its Path for a host-only URL is "" where the rule says "/". Byte-level string
// operations are the only thing that can be pinned to a fixture on two runtimes.
func NormalizePath(s string) string {
	if rest, ok := stripHTTPSchemeAndHost(s); ok {
		s = rest
	}
	// (b): the query and the fragment go whichever comes first -- `/a.js?x=#y` and `/a.js#y?x` both
	// cut at the first of the two, which is what "first occurrence" of each means once both are
	// applied in either order.
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	return s
}

// stripHTTPSchemeAndHost implements (a). It returns (rest, true) only when the string is
// `http://` or `https://` (any case) followed by a non-empty host; rest is the remainder starting
// at the first `/`, `?` or `#` after the host, or "/" when the host is the whole string. Anything
// else -- another scheme, `http:///x` with an empty host, a bare path -- returns (s, false).
func stripHTTPSchemeAndHost(s string) (string, bool) {
	const sep = "://"
	i := strings.Index(s, sep)
	if i <= 0 {
		return s, false
	}
	if scheme := strings.ToLower(s[:i]); scheme != "http" && scheme != "https" {
		return s, false
	}
	afterScheme := s[i+len(sep):]
	hostEnd := strings.IndexAny(afterScheme, "/?#")
	if hostEnd == 0 {
		// `http:///x` -- an empty host is not a host, so (a) does not apply (byte-identical).
		return s, false
	}
	if hostEnd < 0 {
		// `https://app.example.com` -- a host and nothing after it.
		if afterScheme == "" {
			return s, false
		}
		return "/", true
	}
	rest := afterScheme[hostEnd:]
	if rest[0] != '/' {
		// `https://app.example.com?x=1` / `https://app.example.com#f`: the host ends at the `?`/`#`;
		// the path is `/`, and (b) then drops the suffix.
		rest = "/" + rest
	}
	return rest, true
}
