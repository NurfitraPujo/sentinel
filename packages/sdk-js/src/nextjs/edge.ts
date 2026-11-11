// `@sentinel/sdk-js/nextjs/edge` (contract §4.1, §4.7; decision 31): the edge build — a fixed typed entry with
// its own `.d.ts` and no export conditions, imported by the file `instrumentation.ts` loads under
// `NEXT_RUNTIME === 'edge'`. Its `init` is the edge's (`endpoint` / `apiKey` / `projectKey`; `batchWaitMs`
// forced to 0 — drain on the next microtask, §4.4 *Edge*), plus §4.3's API, `captureRequestError` returning the
// send's never-rejecting promise (Next awaits it, so the isolate stays alive until the POST completes — and an
// error response is held for at most the per-attempt timeout, the README's *Edge* line), and the router no-op
// under both names.
//
// Import-time inertness (contract §4.7, §11; D7): nothing here touches `process`, timers or `fetch` until `init`.

import { sendPromise } from '../core/client.js';
import { initEdge } from '../edge.js';
import type { EdgeInitOptions } from '../core/types.js';
import { captureRequestErrorEvent, type InstrumentationOnRequestError, type RequestErrorContext, type RequestErrorRequest } from './request-error.js';

export * from '../edge.js';
export { captureRouterTransitionStart, onRouterTransitionStart } from './router.js';
export type { NavigationType } from './router.js';
export type { InstrumentationOnRequestError, RequestErrorContext, RequestErrorRequest };

/** Shadows the star export's `init` (spec §3.3): `sdk.name` is `sentinel.javascript.nextjs`. */
export function init(options?: EdgeInitOptions): void {
  initEdge(options, 'nextjs');
}

/**
 * Next's `onRequestError` on the edge (contract §4.7): captures `error` with the request / route metadata and
 * the `traceparent`, no `metadata.source` (decision 37), and returns the send's `Promise<void>` — resolved once
 * the first attempt of the POST carrying the event has been acted on (retries run on timers after), at once when
 * nothing was enqueued. Never rejects, never throws.
 */
export const captureRequestError: (error: unknown, errorRequest: RequestErrorRequest, errorContext: Readonly<RequestErrorContext>) => Promise<void> =
  function captureRequestError(error, errorRequest, errorContext): Promise<void> {
    let id = '';
    try {
      id = captureRequestErrorEvent(error, errorRequest, errorContext);
    } catch {
      // Never throws (contract §4.7).
    }
    return sendPromise(id);
  };
