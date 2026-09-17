// `@sentinel/sdk-js/nextjs/route` (contract §4.1, §4.5): `createSentinelRouteHandler` — the tunnel route the app
// mounts as `app/api/sentinel/route.ts` with `export const { POST } = createSentinelRouteHandler()`. The factory
// returns `{ POST }` and nothing else (under Next any other method gets Next's own 405; step 1's envelope is
// reachable only when the handler is called as a function — T2 is unit-only), and the handler is a plain
// `(req: Request) => Promise<Response>` with no Next import, so it runs on the node and the edge runtime alike
// and is a pure function of the request and the environment.
//
// The nine steps, in the contract's order — the rate limit is step 4, AFTER method / origin / media type and
// BEFORE the body is read, so only a request that could be the same-origin client's spends a token and a
// cross-site flood cannot exhaust a legitimate client's bucket:
//   1 method → 405   2 origin → 403   3 media type → 415   4 rate limit → 429   5 size → 413
//   6 shape → 400 (the taxonomy)   7 configuration → 503   8 one forward, no client header   9 relay
//
// What the route never does (contract §4.5): echo the key or the endpoint in any response; forward to a host
// other than the configured endpoint; accept `project_key` / `project_id` from the body; log a body; relay any
// upstream header beyond `Content-Type` and `Retry-After`; retry its forward.
//
// Import-time inertness (contract §11; D7): the module body reads no env and registers nothing; the env is read
// per request inside the handler, the bucket store is created by the factory.

import { SHARED_KEY, storeOf, TokenBucketStore, type BucketKey } from './rate-limit.js';

export interface RouteHandlerOptions {
  /** The `/ingest` URL; default `process.env.SENTINEL_ENDPOINT`. */
  endpoint?: string;
  /**
   * The ingest key. Default `process.env.SENTINEL_API_KEY` — **only when the property is absent**. When the
   * property is present (own property, `undefined` or `''` included — `apiKey: process.env.SENTINEL_TUNNEL_API_KEY`
   * with that variable unset), the route answers `503 sentinel_not_configured` and never falls back to
   * `SENTINEL_API_KEY`: the server/edge key's rate-limit budget must not be spent by the browser (decision 41).
   * `| undefined` is deliberate: under `exactOptionalPropertyTypes` it is what lets a mount pass an env read.
   */
  apiKey?: string | undefined;
  /** Default `process.env.SENTINEL_PROJECT_KEY`. */
  projectKey?: string;
  /** Default 262144 (256 KiB); non-finite or ≤ 0 → default, one `warn`. */
  maxBodyBytes?: number;
  /** Default 50; non-finite or ≤ 0 → default, one `warn`. */
  maxItems?: number;
  /** Override of the same-origin rule (step 2); a throw ≡ cross-site (403). */
  isCrossSite?: (req: Request) => boolean;
  /** Override of the rate-limit key (step 4): `web` passes its `getClientIp`; `null` → the shared bucket; a throw ≡ `null`. */
  clientKey?: (req: Request) => string | null;
  /** Default 5000; non-finite or ≤ 0 → default, one `warn`. */
  forwardTimeoutMs?: number;
  /** Default `{ perMinute: 60 }` (decision 19); `false` disables; `perMinute` non-finite or ≤ 0 ≡ `false`. */
  rateLimit?: { perMinute: number } | false;
}

export type SentinelRouteHandler = (req: Request) => Promise<Response>;

export const DEFAULT_MAX_BODY_BYTES = 262_144;
export const DEFAULT_MAX_ITEMS = 50;
export const DEFAULT_FORWARD_TIMEOUT_MS = 5000;
export const DEFAULT_PER_MINUTE = 60;
/** `Retry-After` on `503 upstream_unavailable` (contract §4.5 step 9's guidance). */
export const UPSTREAM_RETRY_AFTER_SECONDS = 5;

/**
 * `validation.ErrorPayload`'s JSON keys (apps/ingestor-go/validation/validator.go) — what an item may carry —
 * minus `project_key` / `project_id`, which the route deletes: tenant scope derives from the key, never from
 * the body (step 6). Any other key is dropped; the SDK never sends one, so this is for foreign posters.
 */
export const ITEM_KEYS: ReadonlySet<string> = new Set([
  'event_id',
  'platform',
  'environment',
  'message',
  'error_class',
  'trace_id',
  'span_id',
  'stacktrace',
  'metadata',
  'timestamp',
  'trace_flags',
  'release_version',
  'fingerprint',
  'fingerprint_override',
]);

export type RouteErrorCode =
  | 'method_not_allowed'
  | 'cross_site_request'
  | 'unsupported_media_type'
  | 'payload_too_large'
  | 'invalid_request'
  | 'rate_limited'
  | 'sentinel_not_configured'
  | 'upstream_unavailable';

const PREFIX = '[@sentinel/sdk-js]';
/** Once per process (contract §4.5): the numeric-option warns, by option name, the not-configured warn, and the unset-`apiKey`-option warn. */
const warnedOptions = new Set<string>();
let warnedNotConfigured = false;
let warnedApiKeyOptionUnset = false;

function warn(line: string): void {
  try {
    console.warn(`${PREFIX} ${line}`);
  } catch {
    // A logger that throws is not the route's problem.
  }
}

function debugLine(line: string): void {
  try {
    console.debug(`${PREFIX} ${line}`);
  } catch {
    // ditto
  }
}

/** A thrown value's class name only — never its message, which is the app's text and could carry anything. */
function nameOf(cause: unknown): string {
  try {
    const name = (cause as { name?: unknown } | null)?.name;
    return typeof name === 'string' && name !== '' ? name : typeof cause;
  } catch {
    return 'unknown';
  }
}

/** The envelope of contract §7: `{ error: { code, message, details } }`, `application/json`. */
function errorResponse(status: number, code: RouteErrorCode, message: string, details: Record<string, unknown> = {}, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify({ error: { code, message, details } }), {
    status,
    headers: { 'content-type': 'application/json', ...headers },
  });
}

/** A numeric option (contract §4.5 *The factory never throws*): non-finite or ≤ 0 → the default, one `warn` per option per process. */
function positive(name: string, given: unknown, fallback: number): number {
  if (given === undefined) return fallback;
  if (typeof given === 'number' && Number.isFinite(given) && given > 0) return given;
  if (!warnedOptions.has(name)) {
    warnedOptions.add(name);
    warn(`createSentinelRouteHandler: ${name} ${String(given)} is not a positive finite number; using ${fallback}`);
  }
  return fallback;
}

/** A guarded `process.env[name]` read — the edge may have no `process`; unset or empty is `undefined`. */
function readEnv(name: string): string | undefined {
  try {
    const env = (globalThis as { process?: { env?: Record<string, string | undefined> } }).process?.env;
    if (env === null || typeof env !== 'object') return undefined;
    const value = env[name];
    return typeof value === 'string' && value !== '' ? value : undefined;
  } catch {
    return undefined;
  }
}

function header(req: Request, name: string): string | null {
  try {
    return req.headers.get(name);
  } catch {
    return null;
  }
}

const SAME_SITE_FETCH_VALUES: ReadonlySet<string> = new Set(['same-origin', 'same-site']);

/**
 * `web`'s `src/app/api/same-origin.ts: isCrossSiteRequest` at `origin/main` (read 2026-09-16), minus its env
 * lookup (contract §4.5 step 2): (a) `Sec-Fetch-Site` present → cross-site unless exactly `same-origin` or
 * `same-site`; (b) absent → `Origin` must be present and not `null`, must parse, and `new URL(origin).host` —
 * port INCLUDED — must equal, as a whole string, the raw `x-forwarded-host` or `host` header (`hosts.includes`,
 * no comma splitting, no port stripping). The app passes its own function through `isCrossSite` when it needs
 * `NEXT_PUBLIC_FRONTEND_URL` accepted too.
 */
export function isCrossSiteRequest(req: Request): boolean {
  const secFetchSite = header(req, 'sec-fetch-site');
  if (secFetchSite) return !SAME_SITE_FETCH_VALUES.has(secFetchSite);

  const origin = header(req, 'origin');
  if (!origin || origin === 'null') return true;

  const hosts = [header(req, 'x-forwarded-host'), header(req, 'host')].filter((host): host is string => !!host);
  if (hosts.length === 0) return true;
  try {
    const originHost = new URL(origin).host;
    return !hosts.includes(originHost);
  } catch {
    return true;
  }
}

/** `Content-Type`'s media type, parameters ignored, case-insensitive. */
function mediaType(req: Request): string {
  const value = header(req, 'content-type');
  if (value === null) return '';
  const semicolon = value.indexOf(';');
  return (semicolon === -1 ? value : value.slice(0, semicolon)).trim().toLowerCase();
}

/**
 * The default rate-limit key (contract §4.5 step 4): `X-Forwarded-For`'s first non-empty hop after trimming —
 * `, 1.2.3.4` keys on `1.2.3.4`, never on the empty string every spoofer would share; `,` alone, or no header,
 * falls through to the socket address when the runtime exposes one (a plain `Request` has none — under Next the
 * header is already filled from the socket), else the shared bucket.
 */
function defaultClientKey(req: Request): BucketKey {
  const xff = header(req, 'x-forwarded-for');
  if (xff !== null) {
    for (const hop of xff.split(',')) {
      const trimmed = hop.trim();
      if (trimmed !== '') return trimmed;
    }
  }
  try {
    const r = req as { ip?: unknown; socket?: { remoteAddress?: unknown } };
    if (typeof r.ip === 'string' && r.ip !== '') return r.ip;
    const remote = r.socket?.remoteAddress;
    if (typeof remote === 'string' && remote !== '') return remote;
  } catch {
    // No socket on this runtime's Request.
  }
  return SHARED_KEY;
}

/**
 * Read the body with a cap (step 5): never buffered past `max` bytes — the stream is cancelled at the first chunk
 * that crosses it. `undefined` means the cap was crossed.
 */
async function readCapped(req: Request, max: number): Promise<string | undefined> {
  const body = req.body;
  if (body === null || body === undefined || typeof body.getReader !== 'function') {
    const text = await req.text();
    return new TextEncoder().encode(text).byteLength > max ? undefined : text;
  }
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > max) {
        reader.cancel().catch(() => undefined);
        return undefined;
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock?.();
  }
  const joined = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    joined.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return new TextDecoder().decode(joined);
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

type Shape = { ok: true; items: Record<string, unknown>[] } | { ok: false; reason: 'shape' | 'items' | 'metadata' };

/**
 * Step 6, the taxonomy complete (contract §4.5): unparseable, `null`, a primitive, or an array with a non-object
 * element → `shape`; `[]` or more than `maxItems` → `items`; a single object (`{}` included) is wrapped. Each
 * item: `project_key` / `project_id` deleted, `platform` forced, unknown keys deleted, `metadata` when present
 * must be an object → else `metadata`. Shape only — never item content (the ingestor answers for that).
 */
export function normaliseBody(text: string, maxItems: number): Shape {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { ok: false, reason: 'shape' };
  }
  let list: unknown[];
  if (Array.isArray(parsed)) list = parsed;
  else if (isPlainObject(parsed)) list = [parsed];
  else return { ok: false, reason: 'shape' };
  if (list.length === 0 || list.length > maxItems) return { ok: false, reason: 'items' };
  const items: Record<string, unknown>[] = [];
  for (const entry of list) {
    if (!isPlainObject(entry)) return { ok: false, reason: 'shape' };
    const item: Record<string, unknown> = {};
    for (const key of Object.keys(entry)) {
      if (ITEM_KEYS.has(key)) item[key] = entry[key];
    }
    item.platform = 'javascript';
    if ('metadata' in item && !isPlainObject(item.metadata)) return { ok: false, reason: 'metadata' };
    items.push(item);
  }
  return { ok: true, items };
}

/** Statuses a `Response` may not carry a body with (`new Response(body, { status })` throws). */
function bodyless(status: number): boolean {
  return status === 204 || status === 205 || status === 304;
}

/**
 * Build the tunnel route. Never throws on a bad option; the returned object's only key is `POST`.
 */
export function createSentinelRouteHandler(options: RouteHandlerOptions = {}): { POST: SentinelRouteHandler } {
  const given: RouteHandlerOptions = options !== null && typeof options === 'object' ? options : {};
  const maxBodyBytes = positive('maxBodyBytes', given.maxBodyBytes, DEFAULT_MAX_BODY_BYTES);
  const maxItems = positive('maxItems', given.maxItems, DEFAULT_MAX_ITEMS);
  const forwardTimeoutMs = positive('forwardTimeoutMs', given.forwardTimeoutMs, DEFAULT_FORWARD_TIMEOUT_MS);
  const isCrossSite = typeof given.isCrossSite === 'function' ? given.isCrossSite : undefined;
  const clientKey = typeof given.clientKey === 'function' ? given.clientKey : undefined;
  // Presence, not value: `{ apiKey: process.env.SENTINEL_TUNNEL_API_KEY }` with the variable unset is an own
  // property holding `undefined`, and it means "the tunnel's own key" — never `SENTINEL_API_KEY`.
  const apiKeyGiven = Object.prototype.hasOwnProperty.call(given, 'apiKey');

  // Decision 19: on by default at 60/min; `false` off; a non-finite or ≤ 0 `perMinute` ≡ `false`.
  let store: TokenBucketStore | undefined;
  if (given.rateLimit !== false) {
    const perMinute = isPlainObject(given.rateLimit) ? given.rateLimit.perMinute : DEFAULT_PER_MINUTE;
    if (typeof perMinute === 'number' && Number.isFinite(perMinute) && perMinute > 0) store = new TokenBucketStore(perMinute);
  }

  const POST: SentinelRouteHandler = async (req: Request): Promise<Response> => {
    // 1. Method — reachable as a function only (T2).
    let method = '';
    try {
      method = String(req.method).toUpperCase();
    } catch {
      method = '';
    }
    if (method !== 'POST') {
      return errorResponse(405, 'method_not_allowed', 'Only POST is accepted.', {}, { allow: 'POST' });
    }

    // 2. Origin — the option decides when given; a throw reads as cross-site, failing closed (T15).
    let crossSite: boolean;
    if (isCrossSite !== undefined) {
      try {
        crossSite = isCrossSite(req) !== false;
      } catch (cause) {
        debugLine(`route: isCrossSite threw (${nameOf(cause)}); the request is read as cross-site`);
        crossSite = true;
      }
    } else {
      crossSite = isCrossSiteRequest(req);
    }
    if (crossSite) return errorResponse(403, 'cross_site_request', 'Cross-site requests are not accepted.');

    // 3. Media type.
    if (mediaType(req) !== 'application/json') {
      return errorResponse(415, 'unsupported_media_type', 'Content-Type must be application/json.');
    }

    // 4. Rate limit — before the body is read (a refused request costs no parsing).
    if (store !== undefined) {
      let key: BucketKey;
      if (clientKey !== undefined) {
        let returned: unknown = null;
        try {
          returned = clientKey(req);
        } catch (cause) {
          debugLine(`route: clientKey threw (${nameOf(cause)}); the request is keyed on the shared bucket`);
          returned = null;
        }
        key = typeof returned === 'string' && returned !== '' ? returned : SHARED_KEY;
      } else {
        key = defaultClientKey(req);
      }
      const taken = store.take(key);
      if (!taken.ok) {
        return errorResponse(429, 'rate_limited', 'Too many requests; retry later.', {}, { 'retry-after': String(taken.retryAfterSeconds) });
      }
    }

    // 5. Size — the declared length first, then the stream under a cap.
    const declared = Number(header(req, 'content-length'));
    if (Number.isFinite(declared) && declared > maxBodyBytes) {
      return errorResponse(413, 'payload_too_large', `The body exceeds ${maxBodyBytes} bytes.`, { max_body_bytes: maxBodyBytes });
    }
    let text: string | undefined;
    try {
      text = await readCapped(req, maxBodyBytes);
    } catch {
      return errorResponse(400, 'invalid_request', 'The body could not be read.', { reason: 'shape' });
    }
    if (text === undefined) {
      return errorResponse(413, 'payload_too_large', `The body exceeds ${maxBodyBytes} bytes.`, { max_body_bytes: maxBodyBytes });
    }

    // 6. Shape.
    const shape = normaliseBody(text, maxItems);
    if (!shape.ok) {
      const message =
        shape.reason === 'items'
          ? `The body must be a JSON array of 1 to ${maxItems} objects.`
          : shape.reason === 'metadata'
            ? 'An item’s metadata must be an object.'
            : 'The body must be a JSON array of objects.';
      return errorResponse(400, 'invalid_request', message, { reason: shape.reason });
    }

    // 7. Configuration — resolved per request (never at import), never echoed.
    const endpoint = typeof given.endpoint === 'string' && given.endpoint !== '' ? given.endpoint : readEnv('SENTINEL_ENDPOINT');
    const projectKey = typeof given.projectKey === 'string' && given.projectKey !== '' ? given.projectKey : readEnv('SENTINEL_PROJECT_KEY');
    let apiKey: string | undefined;
    if (apiKeyGiven) {
      // The option is present: its value is the key, and an unset value is a configuration error, not a
      // request to read the environment — the browser must never spend the server/edge key's budget.
      apiKey = typeof given.apiKey === 'string' && given.apiKey !== '' ? given.apiKey : undefined;
      if (apiKey === undefined) {
        if (!warnedApiKeyOptionUnset) {
          warnedApiKeyOptionUnset = true;
          warn('createSentinelRouteHandler: the route apiKey option is unset; not falling back to SENTINEL_API_KEY (set the tunnel’s own key, e.g. SENTINEL_TUNNEL_API_KEY); answering 503 sentinel_not_configured');
        }
        return errorResponse(503, 'sentinel_not_configured', 'The Sentinel tunnel is not configured.');
      }
    } else {
      apiKey = readEnv('SENTINEL_API_KEY');
    }
    if (endpoint === undefined || apiKey === undefined) {
      if (!warnedNotConfigured) {
        warnedNotConfigured = true;
        warn('createSentinelRouteHandler: SENTINEL_ENDPOINT and SENTINEL_API_KEY (or the endpoint / apiKey options) must both be set; answering 503 sentinel_not_configured');
      }
      return errorResponse(503, 'sentinel_not_configured', 'The Sentinel tunnel is not configured.');
    }

    // 8. Forward — one attempt, no client header, within the timeout.
    const headers: Record<string, string> = { 'content-type': 'application/json', 'x-api-key': apiKey };
    if (projectKey !== undefined) headers['x-project-key'] = projectKey;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), forwardTimeoutMs);
    (timer as { unref?: () => void }).unref?.();
    let upstream: Response;
    try {
      upstream = await globalThis.fetch(`${endpoint.replace(/\/$/, '')}/batch`, {
        method: 'POST',
        headers,
        body: JSON.stringify(shape.items),
        signal: controller.signal,
      });
    } catch {
      clearTimeout(timer);
      return errorResponse(503, 'upstream_unavailable', 'The Sentinel ingestor did not answer.', {}, { 'retry-after': String(UPSTREAM_RETRY_AFTER_SECONDS) });
    }

    // 9. Relay — status and body verbatim, `Content-Type` and `Retry-After` only.
    const relayed = new Headers();
    let body: string | null = null;
    try {
      const contentType = upstream.headers.get('content-type');
      const retryAfter = upstream.headers.get('retry-after');
      if (contentType !== null) relayed.set('content-type', contentType);
      if (retryAfter !== null) relayed.set('retry-after', retryAfter);
      body = bodyless(upstream.status) ? null : await upstream.text();
    } catch {
      clearTimeout(timer);
      return errorResponse(503, 'upstream_unavailable', 'The Sentinel ingestor did not answer.', {}, { 'retry-after': String(UPSTREAM_RETRY_AFTER_SECONDS) });
    } finally {
      clearTimeout(timer);
    }
    return new Response(body, { status: upstream.status, headers: relayed });
  };

  if (store !== undefined) storeOf.set(POST, store);
  return { POST };
}
