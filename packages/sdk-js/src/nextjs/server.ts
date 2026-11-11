// `@sentinel/sdk-js/nextjs/server` (contract §4.1, §4.7; decision 31): the server build — a fixed typed entry
// with its own `.d.ts` and no export conditions, imported by the file `instrumentation.ts` loads under
// `NEXT_RUNTIME === 'nodejs'`. Its `init` is node's (`endpoint` / `apiKey` / `projectKey`, the process hooks of
// §4.6), plus §4.3's API, `captureRequestError` returning `undefined` synchronously (enqueue only — the node
// worker sends on its own timers), and the router no-op under both names.
//
// The runtime is fixed by this subpath, never sniffed: `process.env.NEXT_RUNTIME` is read only for one `debug`
// line, inside `init`. Import-time inertness (contract §4.7, §11; D7): nothing here touches `process`, timers
// or `fetch` until `init`.

import { debug } from '../core/report.js';
import { getInternalClient } from '../core/state.js';
import { readEnv } from '../core/options.js';
import { initNode } from '../node.js';
import type { NodeInitOptions } from '../core/types.js';
import { captureRequestErrorEvent, type InstrumentationOnRequestError, type RequestErrorContext, type RequestErrorRequest } from './request-error.js';

export * from '../core/index.js';
export type { NodeInitOptions } from '../core/types.js';
export { captureRouterTransitionStart, onRouterTransitionStart } from './router.js';
export type { NavigationType } from './router.js';
export type { InstrumentationOnRequestError, RequestErrorContext, RequestErrorRequest };

/** The node `init` (contract §4.2, §4.6) — `NEXT_RUNTIME` noted in `debug` output only. */
export function init(options?: NodeInitOptions): void {
  initNode(options, 'nextjs');
  try {
    const client = getInternalClient();
    if (client !== undefined) debug(client, `nextjs/server init (NEXT_RUNTIME=${readEnv('NEXT_RUNTIME') ?? '(unset)'})`);
  } catch {
    // `init` never throws (contract §4.2).
  }
}

/**
 * Next's `onRequestError` (contract §4.7): captures `error` with the request / route metadata and the
 * `traceparent`, no `metadata.source` (decision 37), and returns `undefined` synchronously — the event is
 * enqueued and the worker sends it. Never throws.
 */
export const captureRequestError: (error: unknown, errorRequest: RequestErrorRequest, errorContext: Readonly<RequestErrorContext>) => void =
  function captureRequestError(error, errorRequest, errorContext): void {
    captureRequestErrorEvent(error, errorRequest, errorContext);
  };
