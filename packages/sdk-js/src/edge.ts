// `@sentinel/sdk-js/edge` (contract §4.1, subpath `./edge`): `init` for edge / worker runtimes — `fetch` only, no
// `process`, no `window`, no global handlers to install (contract §4.6 *Edge*; `captureRequestError` is the
// path, §4.7) — plus §4.3's runtime-agnostic API. The options type is the edge's own — `endpoint`, `apiKey`,
// `projectKey`, no `installGlobalHandlers`, no `tunnel` (decision 31).
//
// Import-time inertness (contract §4.7, §11): this module body registers nothing — no timer, no `fetch`, no env
// read — until `init` is called; `init` is synchronous, idempotent and never throws.

import { initCore } from './core/init.js';
import { planServer, withServerEnvDefaults } from './core/options.js';
import type { AutoContexts, EdgeInitOptions } from './core/types.js';

export * from './core/index.js';
export type { EdgeInitOptions } from './core/types.js';

/** `contexts.runtime` only (spec §6.3): `EdgeRuntime` when Next defines it as a string, else `'edge'`; no `version`. */
function edgeAutoContexts(): AutoContexts {
  try {
    const runtimeGlobal = (globalThis as unknown as { EdgeRuntime?: unknown }).EdgeRuntime;
    const name = typeof runtimeGlobal === 'string' ? runtimeGlobal : 'edge';
    return { contexts: { runtime: { name } } };
  } catch {
    return {};
  }
}

/**
 * Build the edge client (contract §4.2): the same env defaults and `endpoint` rule as node (`process.env` read
 * guardedly — the edge may have none), `batchWaitMs` forced to `0` by the transport (§4.4 *Edge*), nothing
 * installed on any global.
 */
export function initEdge(options: EdgeInitOptions | undefined, entry: string): void {
  try {
    const given = withServerEnvDefaults<EdgeInitOptions>(options !== null && typeof options === 'object' ? options : {});
    initCore(given, 'edge', planServer(given), entry, edgeAutoContexts);
  } catch {
    // `init` never throws (contract §4.2).
  }
}

export function init(options?: EdgeInitOptions): void {
  initEdge(options, 'edge');
}
