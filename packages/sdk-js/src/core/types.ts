// The public shapes of contract §4.2 / §4.3 and the internal ones the pipeline shares.
//
// `SentinelEvent` is exactly `validation.ErrorPayload`'s keys (apps/ingestor-go/validation/validator.go) minus
// the three the SDK never sends — `project_key`, `project_id`, `fingerprint_override` (contract §4.3's event
// table) — so a fixture decoded with `DisallowUnknownFields` on the Go side is the same object this file types.

/** One parsed frame, innermost first, in `validation.StackFrame`'s key set (contract §4.3). */
export interface StackFrame {
  file: string;
  /** 1-based as printed; absent when the engine printed none — never `0`, never negative. */
  line?: number;
  column?: number;
  function: string;
  in_app: boolean;
}

/** The wire event (contract §4.3). Every key is `validation.ErrorPayload`'s. */
export interface SentinelEvent {
  event_id: string;
  platform: 'javascript';
  environment: string;
  release_version?: string;
  error_class: string;
  message: string;
  stacktrace: StackFrame[];
  metadata: Record<string, unknown>;
  timestamp: string;
  fingerprint?: string;
  trace_id?: string;
  span_id?: string;
  trace_flags?: number;
}

export type Level = 'error' | 'warning' | 'info';

/** The per-capture context (contract §4.3). */
export interface CaptureContext {
  metadata?: Record<string, unknown>;
  contexts?: { react?: { componentStack?: string | null } };
  fingerprint?: string;
  traceparent?: string;
  level?: Level;
}

export type Runtime = 'browser' | 'node' | 'edge';

/** `metadata.source`'s four values (decision 37) — set only by a global handler (§4.6), never by a capture call. */
export type CaptureSource = 'window.onerror' | 'unhandledrejection' | 'uncaughtException' | 'unhandledRejection';

/** The `onError` vocabulary of contract §7. */
export type SentinelSdkErrorCode =
  | 'not_configured'
  | 'invalid_option'
  | 'before_send_threw'
  | 'handler_install_failed'
  | 'item_rejected'
  | 'batch_dropped'
  | 'buffer_overflow'
  | 'fingerprint_too_long'
  | 'release_too_long'
  | 'metadata_too_large';

export interface SentinelSdkErrorInit {
  option?: string;
  status?: number;
  attempts?: number;
  count?: number;
  cause?: unknown;
}

/** `{ code, message, option?, status?, attempts?, count?, cause? }`, extends `Error` (contract §7). */
export class SentinelSdkError extends Error {
  readonly code: SentinelSdkErrorCode;
  readonly option?: string;
  readonly status?: number;
  readonly attempts?: number;
  readonly count?: number;
  override readonly cause?: unknown;

  constructor(code: SentinelSdkErrorCode, message: string, init: SentinelSdkErrorInit = {}) {
    super(message);
    this.name = 'SentinelSdkError';
    this.code = code;
    if (init.option !== undefined) this.option = init.option;
    if (init.status !== undefined) this.status = init.status;
    if (init.attempts !== undefined) this.attempts = init.attempts;
    if (init.count !== undefined) this.count = init.count;
    if ('cause' in init) this.cause = init.cause;
  }
}

export interface BeforeSendHint {
  /** The value passed to `captureException`, or the message string for `captureMessage` (contract §4.2). */
  originalException: unknown;
}

export type BeforeSend = (
  event: SentinelEvent,
  hint: BeforeSendHint,
) => SentinelEvent | null | Promise<SentinelEvent | null>;

/**
 * The runtime-agnostic half of `init`'s options (contract §4.2) — the ones the event pipeline reads. The
 * transport's (`maxBufferSize`, `batchSize`, `batchWaitMs`) and the per-runtime ones (`tunnel`, `endpoint`,
 * `apiKey`, `projectKey`, `installGlobalHandlers`, `flushOnHide`) are added by the entries that own them.
 */
export interface CoreOptions {
  release?: string;
  environment?: string;
  sampleRate?: number;
  beforeSend?: BeforeSend;
  debug?: boolean;
  onError?: (err: SentinelSdkError) => void;
  inApp?: (frame: StackFrame) => boolean | undefined;
  /** Accepted and ignored so `web`'s init object compiles unchanged (contract §4.2). */
  tracesSampleRate?: number;
}

/** The options as `init` resolved them (contract §4.2 — defaults applied, degraded values replaced). */
export interface ResolvedCoreOptions {
  /** `''` when omitted, empty or over 100 bytes — then `release_version` is not sent (N10). */
  release: string;
  environment: string;
  sampleRate: number;
  beforeSend?: BeforeSend;
  debug: boolean;
  onError?: (err: SentinelSdkError) => void;
  inApp?: (frame: StackFrame) => boolean | undefined;
}

/**
 * The transport's options (contract §4.2's table, §4.4) — every runtime's, as one shape: the runtime entries
 * (task 5) narrow it per entry (`apiKey` does not exist on the browser type, decision 31). The defaults and
 * degradations are applied in client.ts.
 */
export interface TransportOptions {
  /** The ring buffer (§4.4); default 100. */
  maxBufferSize?: number;
  /** Default 10, clamped to `[1, 50]`. */
  batchSize?: number;
  /** Default 1 000; forced to `0` — "drain on the next microtask" — on the edge build. */
  batchWaitMs?: number;
  /** node, edge: the `/ingest` URL; the batch URL is `endpoint.replace(/\/$/, '') + '/batch'`. */
  endpoint?: string;
  /** node, edge: sent as `X-API-Key`; never in the body, never on `getClient().options`. */
  apiKey?: string;
  /** node, edge: sent as `X-Project-Key` when set. */
  projectKey?: string;
  /** browser: the same-origin URL the browser posts to. */
  tunnel?: string;
  /** browser: the page-hide send (§4.4); default `true`. */
  flushOnHide?: boolean;
}

/** The global handlers' switch (contract §4.6): browser and node only; the edge has nothing to install. */
export interface HandlerOptions {
  /** Default `true`. */
  installGlobalHandlers?: boolean;
}

export type InitOptions = CoreOptions & TransportOptions & HandlerOptions;

/** The transport options as `init` resolved them — the `apiKey` is not here (redacted, decision 36). */
export interface ResolvedTransportOptions {
  maxBufferSize: number;
  batchSize: number;
  batchWaitMs: number;
  flushOnHide: boolean;
  endpoint?: string;
  projectKey?: string;
  tunnel?: string;
}

export interface ResolvedHandlerOptions {
  installGlobalHandlers: boolean;
}

export type ResolvedOptions = ResolvedCoreOptions & ResolvedTransportOptions & ResolvedHandlerOptions;

/**
 * The per-entry option types of decision 31 (contract §4.1, §4.2's *Runtimes* column): `./browser` and
 * `./nextjs/client` have `tunnel` and `flushOnHide` and NO `apiKey` / `endpoint` — `init({ apiKey })` in a
 * client file is a compile error, not a bundled key (B18); `./node` and `./edge` have `endpoint` / `apiKey` /
 * `projectKey`; only the browser and node entries have `installGlobalHandlers`.
 */
export type BrowserInitOptions = CoreOptions &
  Pick<TransportOptions, 'maxBufferSize' | 'batchSize' | 'batchWaitMs' | 'tunnel' | 'flushOnHide'> &
  HandlerOptions;
export type NodeInitOptions = CoreOptions &
  Pick<TransportOptions, 'maxBufferSize' | 'batchSize' | 'batchWaitMs' | 'endpoint' | 'apiKey' | 'projectKey'> &
  HandlerOptions;
export type EdgeInitOptions = CoreOptions & Pick<TransportOptions, 'maxBufferSize' | 'batchSize' | 'batchWaitMs' | 'endpoint' | 'apiKey' | 'projectKey'>;

/** The opaque client of decision 36: nothing callable. */
export interface SentinelClient {
  readonly options: Readonly<ResolvedCoreOptions & Record<string, unknown>>;
  readonly runtime: Runtime;
}
