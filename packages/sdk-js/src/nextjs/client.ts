// `@sentinel/sdk-js/nextjs/client` (contract §4.1, §4.7; decision 31): the client build — a fixed typed entry
// with its own `.d.ts` and no export conditions, imported by `instrumentation-client.ts` and nothing else. Its
// `init` is the browser's (`tunnel`, `flushOnHide`, `installGlobalHandlers`; NO `apiKey` / `endpoint` — B18),
// plus §4.3's API, the router no-op under both names (B20) and `captureRequestError` as a no-op so
// `instrumentation.ts` type-checks in every bundle.
//
// Import-time inertness (contract §4.7, §11; D7): `instrumentation-client.ts` runs before hydration on every
// page and Next evaluates this graph wherever it bundles it — nothing here touches `window`, `process`, timers
// or `fetch` until `init`.

import { initBrowser } from '../browser.js';
import type { BrowserInitOptions } from '../core/types.js';
import type { InstrumentationOnRequestError } from './request-error.js';

export * from '../browser.js';
export { captureRouterTransitionStart, onRouterTransitionStart } from './router.js';
export type { NavigationType } from './router.js';
export type { InstrumentationOnRequestError, RequestErrorContext, RequestErrorRequest } from './request-error.js';

/** Shadows the star export's `init` (an explicit export wins, spec §3.3): `sdk.name` is `sentinel.javascript.nextjs`. */
export function init(options?: BrowserInitOptions): void {
  initBrowser(options, 'nextjs');
}

/** The client build's `captureRequestError`: a no-op with Next's signature (contract §4.7). Never throws. */
export const captureRequestError: InstrumentationOnRequestError = function captureRequestError(): void {
  // `onRequestError` is a server-side hook; on the client bundle it exists only so the import resolves.
};
