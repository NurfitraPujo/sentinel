// The URL privacy filter (spec §5.1, decision 5): applied to every URL the SDK records — breadcrumb `data.url`
// / `.from` / `.to`, `request.url`, `request.query_string`, the `referer` header (§6.4), every `stacktrace[].file`
// and every event `message`. No lookbehind, no `replaceAll`, no post-Safari-12 built-in (§5.1 *Regex syntax*,
// *Built-in API floor*) — this file ships in the browser bundle.

import { PII_KEYS, isSensitiveKey as isPiiSensitiveKey } from './pii.js';
import type { RequestInfo } from './types.js';

/** Rule 3's segment-exact words (spec §5.1): match whole key segments, not substrings. */
const SENSITIVE_SEGMENTS: ReadonlySet<string> = new Set(['code', 'state', 'key', 'signature', 'sig', 'jwt', 'otp', 'credential']);

const SCHEME_DOUBLE_SLASH = /^[A-Za-z][A-Za-z0-9+.-]*:\/\//;

/**
 * `URL_IN_TEXT` (spec §5.1): group 1 the captured prefix (start of string, or one non-word non-`/`
 * character — does a lookbehind's job without one, Safari 12); group 2 the base (an absolute `http(s)` URL
 * up to `?`/`#`, or a path starting with `/`); group 3 the query, without its `?`; group 4 the fragment,
 * `#` included. Flags `g` only.
 */
export const URL_IN_TEXT = /(^|[^\w/])(https?:\/\/[^\s"'<>?#]+|\/[^\s"'<>?#]*)(?:\?([^\s"'<>#]*))?(#[^\s"'<>]*)?/g;

/**
 * Rule 0: userinfo removed. The authority is the text after `//` up to the first `/`, `\`, `?`, `#` or the
 * end; if it holds `@`, everything through the LAST `@` is removed (the WHATWG parser's rule). A string
 * without `//` (or not starting with a recognised scheme) is untouched. Linear time.
 */
export function stripUserinfo(url: string): string {
  let authorityStart: number;
  if (url.startsWith('//')) {
    authorityStart = 2;
  } else {
    const m = SCHEME_DOUBLE_SLASH.exec(url);
    if (m === null) return url;
    authorityStart = m[0].length;
  }
  let authorityEnd = url.length;
  for (let i = authorityStart; i < url.length; i++) {
    const ch = url.charAt(i);
    if (ch === '/' || ch === '\\' || ch === '?' || ch === '#') {
      authorityEnd = i;
      break;
    }
  }
  const authority = url.slice(authorityStart, authorityEnd);
  const lastAt = authority.lastIndexOf('@');
  if (lastAt === -1) return url;
  return url.slice(0, authorityStart) + authority.slice(lastAt + 1) + url.slice(authorityEnd);
}

/** One maximal run of `%XX` triplets decoded the lenient way (rule 2): the whole run, or triplet-by-triplet. */
function decodeEscapeRun(run: string): string {
  try {
    return decodeURIComponent(run);
  } catch {
    // Fall through to the triplet-by-triplet scan below.
  }
  const triplets: string[] = [];
  for (let i = 0; i < run.length; i += 3) triplets.push(run.slice(i, i + 3));
  let out = '';
  let i = 0;
  while (i < triplets.length) {
    let decoded: string | undefined;
    let used = 0;
    for (let take = Math.min(4, triplets.length - i); take >= 1; take--) {
      const candidate = triplets.slice(i, i + take).join('');
      try {
        decoded = decodeURIComponent(candidate);
        used = take;
        break;
      } catch {
        // Try a shorter run.
      }
    }
    if (decoded !== undefined) {
      out += decoded;
      i += used;
    } else {
      out += triplets[i];
      i += 1;
    }
  }
  return out;
}

const ESCAPE_RUN = /(?:%[0-9A-Fa-f]{2})+/g;

function lenientDecodeStep(s: string): string {
  const spaced = s.replace(/\+/g, ' ');
  return spaced.replace(ESCAPE_RUN, (run) => decodeEscapeRun(run));
}

/** Rule 2 / rule 5's repeated lenient decode: at most 3 steps, stopping when the string stops changing. */
function lenientDecode(s: string): { value: string; steps: number } {
  let value = s;
  let steps = 0;
  for (let i = 0; i < 3; i++) {
    const next = lenientDecodeStep(value);
    if (next === value) break;
    value = next;
    steps++;
  }
  return { value, steps };
}

/** Rule 3: a key is sensitive by substring (`PII_KEYS`) or by an exact segment match. */
function isSensitiveKey(key: string): boolean {
  const lower = key.toLowerCase();
  if (PII_KEYS.some((k) => lower.includes(k))) return true;
  const segments = lower.split(/[-_.[\]]/);
  return segments.some((s) => SENSITIVE_SEGMENTS.has(s));
}

interface QueryPart {
  text: string;
  /** The separator that followed this part: '&', ';' or '' for the last part. */
  sep: string;
}

/** Rule 2's split on `&` and `;`, each part keeping the separator that followed it. */
function splitQueryParts(qs: string): QueryPart[] {
  const parts: QueryPart[] = [];
  let start = 0;
  for (let i = 0; i < qs.length; i++) {
    const ch = qs.charAt(i);
    if (ch === '&' || ch === ';') {
      parts.push({ text: qs.slice(start, i), sep: ch });
      start = i + 1;
    }
  }
  parts.push({ text: qs.slice(start), sep: '' });
  return parts;
}

/** Rule 5: is `decoded` URL-shaped — holds `?`, a fragment with a sensitive key, or userinfo? */
function isUrlShaped(decoded: string): boolean {
  if (decoded.includes('?')) return true;
  const hashIndex = decoded.indexOf('#');
  if (hashIndex !== -1) {
    const fragment = decoded.slice(hashIndex + 1);
    for (const part of splitQueryParts(fragment)) {
      const eq = part.text.indexOf('=');
      if (eq === -1) continue;
      const key = part.text.slice(0, eq);
      if (isSensitiveKey(lenientDecode(key).value)) return true;
    }
  }
  if (decoded.startsWith('//') || SCHEME_DOUBLE_SLASH.test(decoded)) {
    if (stripUserinfo(decoded) !== decoded) return true;
  }
  return false;
}

/** `encodeURIComponent` applied `n` times (the inverse of rule 5's `n`-step lenient decode). */
function encodeTimes(value: string, n: number): string {
  let out = value;
  for (let i = 0; i < n; i++) out = encodeURIComponent(out);
  return out;
}

/** Rule 5, recursive to at most 3 levels: filter a query-part VALUE that may itself carry a nested URL. */
function filterNestedValue(value: string, depth: number): string {
  const { value: decoded, steps } = lenientDecode(value);
  if (depth >= 3 || !isUrlShaped(decoded)) return value;
  const filtered = filterUrlAtDepth(decoded, depth + 1);
  if (filtered === decoded) return value;
  return encodeTimes(filtered, steps);
}

/** `filterUrl`'s body, parameterised by the recursion depth rule 5 tracks. */
function filterUrlAtDepth(url: string, depth: number): string {
  const stripped = stripUserinfo(url);
  const hashIndex = stripped.indexOf('#');
  const noFragment = hashIndex === -1 ? stripped : stripped.slice(0, hashIndex);
  const qIndex = noFragment.indexOf('?');
  if (qIndex === -1) return noFragment;
  return noFragment.slice(0, qIndex + 1) + filterQueryString(noFragment.slice(qIndex + 1), depth);
}

/** Rules 2-5 applied to a bare query string (no leading `?`), at `depth` levels of §5.1 rule 5's recursion. */
function filterQueryString(qs: string, depth: number): string {
  const parts = splitQueryParts(qs);
  let out = '';
  for (const part of parts) {
    const eq = part.text.indexOf('=');
    if (eq === -1) {
      // A keyless part is kept as is (rule 4).
      out += part.text + part.sep;
      continue;
    }
    const rawKey = part.text.slice(0, eq);
    const rawValue = part.text.slice(eq + 1);
    const key = lenientDecode(rawKey).value;
    if (isSensitiveKey(key)) {
      out += rawKey + '=[Filtered]' + part.sep;
      continue;
    }
    out += rawKey + '=' + filterNestedValue(rawValue, depth) + part.sep;
  }
  return out;
}

/** `filterQuery(qs)` (spec §5.1): rules 2-5 on a bare query string. */
export function filterQuery(qs: string): string {
  return filterQueryString(qs, 0);
}

/**
 * `filterUrl(url, base?)` (spec §5.1): userinfo removed (rule 0), the fragment dropped (rule 1), the query
 * filtered (rules 2-5). A string with no `?` and no `#` comes back unchanged except for rule 0 (rule 6).
 * `base` is accepted for the signature §5.1 pins; nothing in this revision resolves a relative URL against it.
 */
export function filterUrl(url: string, base?: string): string {
  void base;
  return filterUrlAtDepth(url, 0);
}

/**
 * `filterUrlsInText(text)` (spec §5.1): every `URL_IN_TEXT` match's base through `stripUserinfo`, its query
 * through `filterQuery`, its fragment dropped. Linear time; never throws.
 */
export function filterUrlsInText(text: string): string {
  URL_IN_TEXT.lastIndex = 0;
  return text.replace(URL_IN_TEXT, (_m, prefix: string, base: string, query: string | undefined) => {
    const strippedBase = stripUserinfo(base);
    return query === undefined ? prefix + strippedBase : prefix + strippedBase + '?' + filterQuery(query);
  });
}

/** Credential header names dropped outright (spec §4.3/§6.4), lower-cased — never even reach `isSensitiveKey`. */
const DROPPED_REQUEST_HEADER_NAMES: ReadonlySet<string> = new Set(['authorization', 'cookie', 'set-cookie', 'proxy-authorization', 'x-api-key']);

/**
 * A `RequestInfo`'s `url` / `query_string` / `headers` run through the same privacy filter as an automatic
 * request (spec §5.1, §6.4): `url` and `query_string` through `filterUrl` / `filterQuery`, credential headers
 * dropped, a PII-sensitive header name `[Filtered]`, and any other header value holding `?` or `#` run
 * through `filterUrl`. So a caller-supplied `CaptureContext.request` (the only path this never ran on, since
 * every automatic `request` is built already filtered) cannot smuggle a raw cookie or reset-token query past
 * the SDK. Idempotent on an already-filtered value.
 */
export function filterRequestInfo(request: RequestInfo): RequestInfo {
  const out: RequestInfo = { ...request };
  if (typeof out.url === 'string') out.url = filterUrl(out.url);
  if (typeof out.query_string === 'string') out.query_string = filterQuery(out.query_string);
  if (out.headers !== undefined) {
    const headers: Record<string, string> = {};
    for (const name of Object.keys(out.headers)) {
      const lower = name.toLowerCase();
      if (DROPPED_REQUEST_HEADER_NAMES.has(lower)) continue;
      const value = out.headers[name] as string;
      if (isPiiSensitiveKey(name)) headers[name] = '[Filtered]';
      else if (value.includes('?') || value.includes('#')) headers[name] = filterUrl(value);
      else headers[name] = value;
    }
    if (Object.keys(headers).length > 0) out.headers = headers;
    else delete out.headers;
  }
  return out;
}
