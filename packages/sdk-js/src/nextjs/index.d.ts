// `@sentinel/sdk-js/nextjs` — the types of the conditional runtime entry (contract §4.1, §4.7; decision 31).
//
// Hand-written and copied verbatim to dist/nextjs/index.d.ts by scripts/build.mjs as the `types` condition of
// `./nextjs`, whose runtime resolves per bundle (`browser` → client.js, `edge-light` / `worker` / `workerd` →
// edge.js, `node` / `default` → server.js). It declares ONLY the runtime-agnostic re-exports — what every one of
// the three builds provides — and never `init`, so `instrumentation.ts`, `report-boundary-error.ts`,
// `stale-deploy.ts` and any component compile in every bundle, and `import { init } from
// '@sentinel/sdk-js/nextjs'` is a type error (flows B18): an init file imports its own fixed subpath.

export { captureException, captureMessage, close, flush, getClient, SentinelSdkError, setTag, setTenant, setUser, SDK_NAME } from '../core/index.js';
export type {
  BeforeSend,
  BeforeSendHint,
  CaptureContext,
  CoreOptions,
  InitOptions,
  Level,
  ResolvedCoreOptions,
  ResolvedOptions,
  ResolvedTransportOptions,
  Runtime,
  SentinelClient,
  SentinelEvent,
  SentinelSdkErrorCode,
  StackFrame,
  TransportOptions,
} from '../core/index.js';
export type { InstrumentationOnRequestError, RequestErrorContext, RequestErrorRequest } from './request-error.js';
export type { NavigationType } from './router.js';

/**
 * Next's `onRequestError` (contract §4.7): `void` on the server build (enqueue only), the send's never-rejecting
 * `Promise<void>` on the edge build, a no-op on the client build — typed once as Next's own `void | Promise<void>`
 * so `export const onRequestError = captureRequestError` compiles in every bundle.
 */
export declare const captureRequestError: (
  error: unknown,
  errorRequest: import('./request-error.js').RequestErrorRequest,
  errorContext: Readonly<import('./request-error.js').RequestErrorContext>,
) => void | Promise<void>;

/** The router-transition hook, a no-op (decision D7; flows B20), under both names. */
export declare function onRouterTransitionStart(url: string, navigationType: import('./router.js').NavigationType): void;
export declare const captureRouterTransitionStart: (url: string, navigationType: import('./router.js').NavigationType) => void;
