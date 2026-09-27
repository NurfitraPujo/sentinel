// `captureRequestError` (contract §4.7): Next's `onRequestError` hook signature, transcribed from
// `next/dist/server/instrumentation/types.d.ts` at 15.5.21 — `InstrumentationOnRequestError` and
// `RequestErrorContext`, key for key — so `export const onRequestError = captureRequestError` in an app's
// `instrumentation.ts` type-checks against Next's own type structurally, and this package needs no `next` types
// to build (`next` is an optional peer; D2's scratch app pins the assignment against the real type). The
// argument's `headers` is `NodeJS.Dict<string | string[]>`, spelled out below as the same index signature.
//
// One builder for the three builds: the server build returns `undefined` synchronously (enqueue only), the edge
// build returns the send's promise (client.ts `sendPromise`), the client build is a no-op. The capture carries
// `metadata.http_method` / `http_url` / `route` / `router_kind` / `route_type` (+ `render_source` /
// `revalidate_reason` when Next set them) and `traceparent` from the request headers — and NO `metadata.source`
// (decision 37: the app's own `instrumentation.ts` wired this call; `route` says where the error came from).
// Never throws: a malformed `errorRequest` / `errorContext` / `headers` (a `Proxy` that throws, `null`, a
// missing key) still captures the error with whatever could be read.

import { capture } from '../core/capture.js';
import { isSensitiveKey } from '../core/pii.js';
import { filterQuery, filterUrl } from '../core/url.js';
import type { CaptureContext, RequestInfo } from '../core/types.js';

function codePoints(s: string): string[] {
  return Array.from(s);
}

function cutCodePoints(s: string, max: number): string {
  const cps = codePoints(s);
  return cps.length > max ? cps.slice(0, max).join('') : s;
}

/** Names dropped outright, lower-cased (contract §4.3) — never even reach `isSensitiveKey`. */
const DROPPED_HEADER_NAMES: ReadonlySet<string> = new Set(['authorization', 'cookie', 'set-cookie', 'proxy-authorization', 'x-api-key']);

/** `RequestErrorContext` — next/dist/server/instrumentation/types.d.ts at 15.5.21, verbatim. */
export type RequestErrorContext = {
  routerKind: 'Pages Router' | 'App Router';
  routePath: string;
  routeType: 'render' | 'route' | 'action' | 'middleware';
  renderSource?: 'react-server-components' | 'react-server-components-payload' | 'server-rendering';
  revalidateReason: 'on-demand' | 'stale' | undefined;
};

/** `errorRequest` as Next passes it: `{ path, method, headers: NodeJS.Dict<string | string[]> }`. */
export type RequestErrorRequest = Readonly<{
  path: string;
  method: string;
  headers: { [key: string]: string | string[] | undefined };
}>;

/** `InstrumentationOnRequestError` at 15.5.21 — the shape `instrumentation.ts`'s `onRequestError` must have. */
export type InstrumentationOnRequestError = (
  error: unknown,
  errorRequest: RequestErrorRequest,
  errorContext: Readonly<RequestErrorContext>,
) => void | Promise<void>;

/** A guarded property read: a getter or `Proxy` trap that throws reads as `undefined`. */
function read(source: unknown, key: string): unknown {
  if (source === null || (typeof source !== 'object' && typeof source !== 'function')) return undefined;
  try {
    return (source as Record<string, unknown>)[key];
  } catch {
    return undefined;
  }
}

/** `headers.traceparent`: a string, or the first of an array (Next's `NodeJS.Dict<string | string[]>`). */
function traceparentOf(headers: unknown): string | undefined {
  let value = read(headers, 'traceparent');
  if (Array.isArray(value)) {
    try {
      value = value[0];
    } catch {
      value = undefined;
    }
  }
  return typeof value === 'string' ? value : undefined;
}

/** One header value as a string: an array is joined with `, `; `undefined` is skipped (spec §6.4). */
function headerValueOf(value: unknown): string | undefined {
  if (typeof value === 'string') return value;
  if (Array.isArray(value)) return value.filter((v): v is string => typeof v === 'string').join(', ');
  return undefined;
}

/**
 * `errorRequest.headers` → `request.headers` (spec §6.4): the five credential names dropped outright; a
 * PII-sensitive name kept with `[Filtered]`; every other value holding `?` or `#` filtered whole through
 * `filterUrl` (D6 — a `referer`, `next-url` or a proxy's forwarded-path header). §3.10's `boundStringMap`
 * (entries, key length, value cut) is applied generically to every event's `request.headers` at `build`.
 */
function requestHeadersOf(headers: unknown): Record<string, string> | undefined {
  if (headers === null || typeof headers !== 'object') return undefined;
  let keys: string[];
  try {
    keys = Object.keys(headers as Record<string, unknown>);
  } catch {
    return undefined;
  }
  const out: Record<string, string> = {};
  for (const name of keys) {
    const lower = name.toLowerCase();
    if (DROPPED_HEADER_NAMES.has(lower)) continue;
    const raw = read(headers, name);
    const value = headerValueOf(raw);
    if (value === undefined) continue;
    if (isSensitiveKey(name)) {
      out[name] = '[Filtered]';
    } else if (value.includes('?') || value.includes('#')) {
      out[name] = filterUrl(value);
    } else {
      out[name] = value;
    }
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

/** `errorRequest.path` split into its `request.url` (up to `?`/`#`) and `request.query_string` (spec §6.4). */
function requestFromPath(path: string): { url: string; queryString?: string } {
  const hashIndex = path.indexOf('#');
  const noFragment = hashIndex === -1 ? path : path.slice(0, hashIndex);
  const qIndex = noFragment.indexOf('?');
  if (qIndex === -1) return { url: cutCodePoints(filterUrl(noFragment), 2048) };
  const url = cutCodePoints(filterUrl(noFragment.slice(0, qIndex)), 2048);
  const rawQuery = noFragment.slice(qIndex + 1);
  const queryString = rawQuery === '' ? undefined : cutCodePoints(filterQuery(rawQuery), 4096);
  return queryString === undefined || queryString === '' ? { url } : { url, queryString };
}

/**
 * Build the capture context for `onRequestError`'s three arguments and capture `error`. Returns the `event_id`
 * (the edge build waits on its send). Never throws.
 */
export function captureRequestErrorEvent(error: unknown, errorRequest: unknown, errorContext: unknown): string {
  const ctx: CaptureContext = { metadata: {} };
  try {
    const metadata: Record<string, unknown> = {};
    const method = read(errorRequest, 'method');
    const path = read(errorRequest, 'path');
    const routePath = read(errorContext, 'routePath');
    const routerKind = read(errorContext, 'routerKind');
    const routeType = read(errorContext, 'routeType');
    const renderSource = read(errorContext, 'renderSource');
    const revalidateReason = read(errorContext, 'revalidateReason');
    if (routePath !== undefined) metadata.route = routePath;
    if (routerKind !== undefined) metadata.router_kind = routerKind;
    if (routeType !== undefined) metadata.route_type = routeType;
    if (renderSource !== undefined) metadata.render_source = renderSource;
    if (revalidateReason !== undefined) metadata.revalidate_reason = revalidateReason;
    ctx.metadata = metadata;

    const request: RequestInfo = {};
    if (typeof method === 'string') request.method = cutCodePoints(method, 16);
    if (typeof path === 'string') {
      const { url, queryString } = requestFromPath(path);
      request.url = url;
      if (queryString !== undefined) request.query_string = queryString;
    }
    const headersIn = read(errorRequest, 'headers');
    const headers = requestHeadersOf(headersIn);
    if (headers !== undefined) request.headers = headers;
    if (Object.keys(request).length > 0) ctx.request = request;

    if (typeof routePath === 'string') ctx.transaction = cutCodePoints(routePath, 200);

    const traceparent = traceparentOf(headersIn);
    if (traceparent !== undefined) ctx.traceparent = traceparent;
  } catch {
    // Whatever could not be read is left out; the error itself is still captured.
  }
  try {
    return capture({ kind: 'exception', value: error, ctx });
  } catch {
    // `capture` is written not to throw; if it ever does, Next must not see it.
    return '';
  }
}
