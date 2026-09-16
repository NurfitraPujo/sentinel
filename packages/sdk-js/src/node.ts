// `@sentinel/sdk-js/node` (contract §4.1, subpath `./node`): `init` for plain node apps plus §4.3's
// runtime-agnostic API. The options type is node's own — `endpoint`, `apiKey`, `projectKey`,
// `installGlobalHandlers` (decision 31).
//
// Import-time inertness (contract §4.7, §11): this module body registers nothing — no `process.on`, no timer, no
// `fetch`, no `process.env` read — until `init` is called; `init` is synchronous, idempotent and never throws.

import { installNodeHandlers } from './core/handlers/node.js';
import { initCore } from './core/init.js';
import { planServer, withServerEnvDefaults } from './core/options.js';
import type { NodeInitOptions } from './core/types.js';

export * from './core/index.js';
export type { NodeInitOptions } from './core/types.js';

/**
 * Build the node client (contract §4.2): `endpoint` / `apiKey` / `projectKey` defaulted from `SENTINEL_ENDPOINT`
 * / `SENTINEL_API_KEY` / `SENTINEL_PROJECT_KEY` (read now, never at import), the `not_configured` no-op client
 * when either is still missing (N4), the no-op client with `invalid_option` for an `endpoint` that is not an
 * absolute `http(s)` URL (N15), the numeric options degraded, a previous client drained and its hooks
 * uninstalled first; then the `uncaughtExceptionMonitor` / `unhandledRejection` hooks of §4.6 installed unless
 * `installGlobalHandlers: false`.
 */
export function init(options?: NodeInitOptions): void {
  try {
    const given = withServerEnvDefaults<NodeInitOptions>(options !== null && typeof options === 'object' ? options : {});
    const client = initCore(given, 'node', planServer(given));
    if (!client.noop && client.options.installGlobalHandlers) installNodeHandlers(client);
  } catch {
    // `init` never throws (contract §4.2); an internal failure leaves whatever was installed.
  }
}
