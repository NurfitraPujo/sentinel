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
import type { CaptureContext } from '../core/types.js';

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
    if (method !== undefined) metadata.http_method = method;
    if (path !== undefined) metadata.http_url = path;
    if (routePath !== undefined) metadata.route = routePath;
    if (routerKind !== undefined) metadata.router_kind = routerKind;
    if (routeType !== undefined) metadata.route_type = routeType;
    if (renderSource !== undefined) metadata.render_source = renderSource;
    if (revalidateReason !== undefined) metadata.revalidate_reason = revalidateReason;
    ctx.metadata = metadata;
    const traceparent = traceparentOf(read(errorRequest, 'headers'));
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
