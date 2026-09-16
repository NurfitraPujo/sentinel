// `@sentinel/sdk-js/browser` (contract §4.1, subpath `./browser`): `init` for plain browser apps plus §4.3's
// runtime-agnostic API. The options type is the browser's own — `tunnel`, `flushOnHide`, `installGlobalHandlers`,
// and NO `apiKey` / `endpoint` (decision 31, B18).
//
// Import-time inertness (contract §4.7, §11): this module body registers nothing — no listener, no timer, no
// `fetch`, no env read — until `init` is called; `init` is synchronous, idempotent and never throws (§4.2).

import { installBrowserHandlers } from './core/handlers/browser.js';
import { initCore } from './core/init.js';
import { planBrowser } from './core/options.js';
import type { BrowserInitOptions } from './core/types.js';

export * from './core/index.js';
export type { BrowserInitOptions } from './core/types.js';

/**
 * Build the browser client (contract §4.2): the `tunnel` resolved against `window.location.origin` and accepted
 * only on the page's own origin (else the no-op client with `invalid_option`, B19), the numeric options
 * degraded, a previous client drained and its handlers uninstalled first; then the `error` /
 * `unhandledrejection` listeners of §4.6 installed unless `installGlobalHandlers: false`.
 */
export function init(options?: BrowserInitOptions): void {
  try {
    const given: BrowserInitOptions = options !== null && typeof options === 'object' ? options : {};
    const client = initCore(given, 'browser', planBrowser(given));
    if (!client.noop && client.options.installGlobalHandlers) installBrowserHandlers(client);
  } catch {
    // `init` never throws (contract §4.2); an internal failure leaves whatever was installed.
  }
}
