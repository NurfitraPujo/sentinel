// `@sentinel/sdk-js` — the runtime-agnostic API (contract §4.1, subpath `.`).
//
// Scaffold (sentinel-sdk-js/TASK-01): the entry exists so the exports table resolves and `dist/core/index.d.ts`
// is emitted. Event building, scope, PII, stack parsing and the transport land in tasks 2–5 under src/core/.
//
// Import-time inertness (contract §11): nothing in this module body may touch `window`, `process`, timers,
// `fetch` or the environment — that rule holds for every entry under src/.

/** The package name, also the key of the one `globalThis[Symbol.for('@sentinel/sdk-js')]` slot (contract §4.1). */
export const SDK_NAME = '@sentinel/sdk-js';
