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
//   6 shape → 400 (the taxonomy)   7 configuration → 503   9 relay
//   8 one forward: the browser's User-Agent is the only client header; the derived IP is written into each
//     item's `user.ip_address`, `user` is rebuilt from an exact-key allow-list, and `x-sentinel-derive-ip: off`
//     tells the ingestor never to derive an IP from this connection (sentinel-event-enrichment spec §4.4).
//
// The client IP (sentinel-event-enrichment spec §4.2, contract decision 11): ONE derivation, the entry at
// `len − trustedProxyHops` of `X-Forwarded-For` (web's shape: one proxy, the rightmost entry), re-serialised in
// canonical form, feeds both the IP the forward writes into every item and the default rate-limit key. The
// leftmost entry is the browser's to write and is never used. `clientKey`, when given, decides both instead.
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
  /**
   * Trusted reverse proxies in front of the app (web's TRUSTED_PROXY_HOPS). Default 1; an integer ≥ 0; anything
   * else → default, one `warn`. It MUST equal the number of proxies that append to `X-Forwarded-For`: a larger
   * value lets a client choose the IP its events are stored under. `0` never reads the header and uses the
   * socket address on the request, which the Next.js App Router does not expose — so under Next, `0` derives no
   * IP at all (sentinel-event-enrichment spec §4.3).
   */
  trustedProxyHops?: number;
}

export type SentinelRouteHandler = (req: Request) => Promise<Response>;

export const DEFAULT_MAX_BODY_BYTES = 262_144;
export const DEFAULT_MAX_ITEMS = 50;
export const DEFAULT_FORWARD_TIMEOUT_MS = 5000;
export const DEFAULT_PER_MINUTE = 60;
export const DEFAULT_TRUSTED_PROXY_HOPS = 1;
/** The forwarded `user-agent` must be 1–1024 bytes (the ingestor parses no longer one, spec §6.2). */
export const MAX_FORWARDED_USER_AGENT_BYTES = 1024;
/** The no-derive marker (spec §3.8): sent as `off` on every forward, never copied from the inbound request. */
export const DERIVE_IP_HEADER = 'x-sentinel-derive-ip';

/**
 * The `user` keys a forward may carry from the item it was given (spec §4.4): the schema-v2 `UserInfo` string
 * fields, EXACT names only. `ip_address` and `geo` are deliberately absent — the ingestor decodes `user` with Go's
 * case-folding `encoding/json`, so `IP_ADDRESS`, `Ip_Address` or `ip_addreſſ` left in place would be read as the
 * IP. The route rebuilds `user` from these names and writes `ip_address` itself.
 */
export const USER_KEYS: readonly string[] = ['id', 'username', 'email'];
/** `Retry-After` on `503 upstream_unavailable` (contract §4.5 step 9's guidance). */
export const UPSTREAM_RETRY_AFTER_SECONDS = 5;

/**
 * `validation.ErrorPayload`'s JSON keys (apps/ingestor-go/validation/validator.go) — what an item may carry —
 * minus `project_key` / `project_id`, which the route deletes: tenant scope derives from the key, never from
 * the body (step 6). Any other key is dropped; the SDK never sends one, so this is for foreign posters.
 *
 * `level sdk user tags contexts request breadcrumbs` (spec §7.1) are the typed fields a browser event now
 * carries. Not admitted: `transaction` / `server_name` (the browser never sends them), `exception` / `logger` /
 * `dist` / `source` (no producer in this feature), `project_key` / `project_id` (deleted above), `received_at`
 * and anything else.
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
  'level',
  'sdk',
  'user',
  'tags',
  'contexts',
  'request',
  'breadcrumbs',
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

/** An integer option ≥ 0: anything else → the default, one `warn` per option per process. */
function nonNegativeInteger(name: string, given: unknown, fallback: number): number {
  if (given === undefined) return fallback;
  if (typeof given === 'number' && Number.isInteger(given) && given >= 0) return given;
  if (!warnedOptions.has(name)) {
    warnedOptions.add(name);
    warn(`createSentinelRouteHandler: ${name} ${String(given)} is not an integer ≥ 0; using ${fallback}`);
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

const IPV4 = /^(0|[1-9]\d{0,2})\.(0|[1-9]\d{0,2})\.(0|[1-9]\d{0,2})\.(0|[1-9]\d{0,2})$/;
const IPV6_CHARS = /^[0-9A-Fa-f:.]+$/;
const MAPPED = /^::ffff:([0-9a-f]{1,4}):([0-9a-f]{1,4})$/;

/**
 * An address's canonical text, or `null` when `value` is not an IP (spec §4.2): parsed and re-serialised, never
 * the substring as received. IPv4 is four decimal octets 0–255 with no leading zeros, whitespace or port. A
 * string with `:` takes the IPv6 path: only hex digits, `:` and `.` are admitted BEFORE any URL parsing (so no
 * zone, bracket, `@`, `/`, `?`, port, or the tab/LF/CR the WHATWG parser silently deletes — `::1]@6.6.6.6:80/?[`
 * would otherwise parse as host `6.6.6.6`), then the WHATWG parser must accept it as an IPv6 literal, and an
 * IPv4-mapped address is unmapped to dotted IPv4, the form the ingestor stores. Pure: no `node:net`, so it runs
 * on the edge runtime too.
 *
 * There is deliberately no second check that the parsed hostname is bracketed: with the input limited to
 * `[0-9A-Fa-f:.]`, `http://[s]` has no `]`, `@` or `/` to end the host early, so the parser either throws or
 * returns `[…]`. Such a check could never fail, and no test could prove it (enr-code-canonicalip-guards-
 * individually-unproven); the character filter is the one guard, and route.test.ts proves it alone.
 */
export function canonicalIp(value: unknown): string | null {
  if (typeof value !== 'string') return null;
  const s = value.trim();
  if (!s.includes(':')) {
    const m = IPV4.exec(s);
    if (m === null) return null;
    const octets = m.slice(1, 5).map(Number);
    return octets.every((o) => o <= 255) ? octets.join('.') : null;
  }
  if (!IPV6_CHARS.test(s)) return null;
  let host: string;
  try {
    host = new URL(`http://[${s}]`).hostname;
  } catch {
    return null;
  }
  const v6 = host.slice(1, -1).toLowerCase();
  const mapped = MAPPED.exec(v6);
  if (mapped !== null) {
    const hi = parseInt(mapped[1] as string, 16);
    const lo = parseInt(mapped[2] as string, 16);
    return `${hi >> 8}.${hi & 255}.${lo >> 8}.${lo & 255}`;
  }
  return v6;
}

/**
 * The client's address (spec §4.2), canonical, or `null`:
 *   1. `hops === 0`: skip to 3.
 *   2. `X-Forwarded-For` present: the entry at `len − hops`, trimmed and canonical; out of range, empty or not an
 *      IP → `null`, NEVER the socket (behind a proxy the socket is the proxy).
 *   3. `hops` 0 or no header: the socket address (`req.ip`, else `req.socket.remoteAddress`) when the runtime
 *      exposes one, canonical. The Next.js App Router exposes none, so under Next this step yields `null`.
 */
export function clientAddress(req: Request, hops: number): string | null {
  if (hops > 0) {
    const xff = header(req, 'x-forwarded-for');
    if (xff !== null) {
      const entries = xff.split(',');
      const index = entries.length - hops;
      return index >= 0 ? canonicalIp(entries[index]) : null;
    }
  }
  try {
    const r = req as { ip?: unknown; socket?: { remoteAddress?: unknown } };
    if (typeof r.ip === 'string' && r.ip !== '') return canonicalIp(r.ip);
    return canonicalIp(r.socket?.remoteAddress);
  } catch {
    // No socket on this runtime's Request.
    return null;
  }
}

/**
 * The default rate-limit key (step 4): the written IP's derivation — `clientAddress(req, trustedProxyHops)` — else
 * the shared bucket. It used to be `X-Forwarded-For`'s LEFTMOST hop, which the browser writes, so any client could
 * mint a fresh bucket per request (spec §4.3).
 */
function defaultClientKey(clientIp: string | null): BucketKey {
  return clientIp ?? SHARED_KEY;
}

/**
 * Write the request's derived IP into every item (spec §4.4, contract decision 11), on the items `normaliseBody`
 * returned, just before step 8 serialises them. A plain-object `user` is REBUILT, never edited: only the exact
 * `USER_KEYS` with string values are copied, then `ip_address` last when `ip` is an address — so no key Go's
 * case-folding decoder could read as `ip_address` or `geo` survives, whatever its spelling, and a browser-sent
 * `geo` is always dropped. Per item:
 *   B1 ip, no/null user → `{ ip_address: ip }`     B2 ip, object → rebuilt + `ip_address: ip`
 *   B3 user not a plain object → left as it is (the ingestor rejects the batch)
 *   B4 no ip, object → rebuilt without any IP, and deleted entirely when the rebuild has no key (an empty
 *      `user: {}` is never forwarded, spec §7.1)      B5 no ip, no/null user → unchanged
 */
export function applyClientIp(items: Record<string, unknown>[], ip: string | null): void {
  for (const item of items) {
    const user = item.user;
    if (user === undefined || user === null) {
      if (ip !== null) item.user = { ip_address: ip };
      continue;
    }
    if (!isPlainObject(user)) continue;
    const rebuilt: Record<string, string> = {};
    for (const name of USER_KEYS) {
      const value = Object.prototype.hasOwnProperty.call(user, name) ? user[name] : undefined;
      if (typeof value === 'string') rebuilt[name] = value;
    }
    if (ip !== null) {
      rebuilt.ip_address = ip;
      item.user = rebuilt;
    } else if (Object.keys(rebuilt).length > 0) {
      item.user = rebuilt;
    } else {
      delete item.user;
    }
  }
}

/** The inbound `user-agent` when it may be forwarded: 1–1024 bytes, no control character (spec §4.4). */
function forwardableUserAgent(req: Request): string | undefined {
  const ua = header(req, 'user-agent');
  if (ua === null || ua === '') return undefined;
  if (new TextEncoder().encode(ua).byteLength > MAX_FORWARDED_USER_AGENT_BYTES) return undefined;
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f]/.test(ua)) return undefined;
  return ua;
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
  const trustedProxyHops = nonNegativeInteger('trustedProxyHops', given.trustedProxyHops, DEFAULT_TRUSTED_PROXY_HOPS);
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

    // The client: ONE clientKey call (or one derivation) per request, before step 4 and whether or not rate
    // limiting is on. It gives the rate-limit key and the IP step 8 writes into every item (spec §4.2).
    let keyed: string | null = null;
    let clientIp: string | null;
    if (clientKey !== undefined) {
      let returned: unknown = null;
      try {
        returned = clientKey(req);
      } catch (cause) {
        debugLine(`route: clientKey threw (${nameOf(cause)}); the request is keyed on the shared bucket`);
        returned = null;
      }
      keyed = typeof returned === 'string' && returned !== '' ? returned : null;
      clientIp = canonicalIp(returned);
    } else {
      clientIp = clientAddress(req, trustedProxyHops);
    }

    // 4. Rate limit — before the body is read (a refused request costs no parsing).
    if (store !== undefined) {
      const key: BucketKey = clientKey !== undefined ? (keyed ?? SHARED_KEY) : defaultClientKey(clientIp);
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

    // 8. Forward — one attempt within the timeout. Headers are built from scratch: the browser's User-Agent is
    // the only client header; no X-Forwarded-For, ever (the IP's only channel is the body); and the constant
    // no-derive marker, so the ingestor never fills an item from this connection (spec §3.8, §4.4).
    const headers: Record<string, string> = { 'content-type': 'application/json', 'x-api-key': apiKey };
    if (projectKey !== undefined) headers['x-project-key'] = projectKey;
    const userAgent = forwardableUserAgent(req);
    if (userAgent !== undefined) headers['user-agent'] = userAgent;
    headers[DERIVE_IP_HEADER] = 'off';
    applyClientIp(shape.items, clientIp);
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
