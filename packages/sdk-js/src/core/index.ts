// `@sentinel/sdk-js` — the runtime-agnostic API (contract §4.1, subpath `.`): the capture calls, the scope
// writers and `getClient` (contract §4.3). `init` lives in the runtime entries; `flush` / `close` arrive with
// the transport (§4.4).
//
// Import-time inertness (contract §11): nothing in this module body may touch `window`, `process`, timers,
// `fetch` or the environment — that rule holds for every entry under src/. The `globalThis` slot the state
// lives on is created lazily, on the first call.

export { SDK_NAME } from './state.js';
export { captureException, captureMessage } from './capture.js';
export { setTag, setTenant, setUser } from './scope.js';
export { getClient } from './init.js';
export { SentinelSdkError } from './types.js';
export type {
  BeforeSend,
  BeforeSendHint,
  CaptureContext,
  CoreOptions,
  Level,
  ResolvedCoreOptions,
  Runtime,
  SentinelClient,
  SentinelEvent,
  SentinelSdkErrorCode,
  StackFrame,
} from './types.js';
