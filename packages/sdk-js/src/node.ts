// `@sentinel/sdk-js/node` (contract §4.1, subpath `./node`): `init` for plain node apps plus §4.3's
// runtime-agnostic API. The options type is node's own — `endpoint`, `apiKey`, `projectKey`,
// `installGlobalHandlers` (decision 31).
//
// Import-time inertness (contract §4.7, §11): this module body registers nothing — no `process.on`, no timer, no
// `fetch`, no `process.env` read — until `init` is called; `init` is synchronous, idempotent and never throws.

import { installNodeHandlers } from './core/handlers/node.js';
import { initCore } from './core/init.js';
import { planServer, withServerEnvDefaults } from './core/options.js';
import { reportError } from './core/report.js';
import type { NodeInitOptions } from './core/types.js';
import { nodeAutoContexts } from './node/contexts.js';

export * from './core/index.js';
export type { NodeInitOptions } from './core/types.js';

/**
 * Build the node client (contract §4.2): `endpoint` / `apiKey` / `projectKey` defaulted from `SENTINEL_ENDPOINT`
 * / `SENTINEL_API_KEY` / `SENTINEL_PROJECT_KEY` (read now, never at import), the `not_configured` no-op client
 * when either is still missing (N4), the no-op client with `invalid_option` for an `endpoint` that is not an
 * absolute `http(s)` URL (N15), the numeric options degraded, a previous client drained and its hooks
 * uninstalled first; then the `uncaughtExceptionMonitor` / `unhandledRejection` hooks of §4.6 installed unless
 * `installGlobalHandlers: false`. `serverName` (spec §6.2): a non-empty string, else `os.hostname()`, with
 * `invalid_option` for a non-string value.
 */
export function initNode(options: NodeInitOptions | undefined, entry: string): void {
  try {
    const given = withServerEnvDefaults<NodeInitOptions>(options !== null && typeof options === 'object' ? options : {});
    const rawServerName = given.serverName;
    const serverNameOption = typeof rawServerName === 'string' ? rawServerName : undefined;
    const client = initCore(given, 'node', planServer(given), entry, () => nodeAutoContexts(serverNameOption));
    if (rawServerName !== undefined && typeof rawServerName !== 'string') {
      reportError(client, 'invalid_option', `serverName ${String(rawServerName)} is not a string; using os.hostname()`, { option: 'serverName' });
    }
    if (!client.noop && client.options.installGlobalHandlers) installNodeHandlers(client);
  } catch {
    // `init` never throws (contract §4.2); an internal failure leaves whatever was installed.
  }
}

export function init(options?: NodeInitOptions): void {
  initNode(options, 'node');
}
