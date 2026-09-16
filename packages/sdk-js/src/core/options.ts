// The runtime entries' half of `init` (contract §4.2): the env defaults (`SENTINEL_ENDPOINT` / `SENTINEL_API_KEY` /
// `SENTINEL_PROJECT_KEY`, read here and only here — never in a module body, §4.7), the `endpoint` rule (an
// absolute `http:` / `https:` URL or the no-op client with `invalid_option`, N15), the `tunnel` rule (resolved
// against the page's origin and accepted only when the origin is the page's own — a protocol-relative
// `//evil.example/collect` is refused, B19) and the `not_configured` no-op client (N4). The result is an
// `InitPlan`: the send target the transport posts to, or the no-op verdict with the report `init` fires once
// the client exists to carry its `onError`.
//
// The numeric options (`maxBufferSize`, `batchSize`, `batchWaitMs`) degrade in client.ts; `release`,
// `environment` and `sampleRate` in init.ts — one rule each, one `invalid_option` per option (B25).

import type { TransportTarget } from './transport/transport.js';
import type { InitOptions, Runtime, SentinelSdkErrorCode } from './types.js';

export const DEFAULT_TUNNEL = '/api/sentinel';

export interface PlannedReport {
  code: SentinelSdkErrorCode;
  message: string;
  option?: string;
}

/** What the entry decided before the client exists: where to post, or why nothing will be posted. */
export interface InitPlan {
  /** The send target; `undefined` with `noop: false` is the plain queue the test helpers read (no transport). */
  target?: TransportTarget;
  /** The no-op client (contract §4.2 *Unconfigured server runtime*, the `tunnel` row): captures accepted and dropped. */
  noop: boolean;
  /** `onError` reports to fire through the new client, in no particular order (B25 asserts the set). */
  reports: PlannedReport[];
}

/** `new URL(value[, base])` succeeding with an `http:` / `https:` protocol (contract §4.2's `endpoint` and `tunnel` rows). */
export function absoluteHttpUrl(value: string, base?: string): URL | undefined {
  try {
    const url = base === undefined ? new URL(value) : new URL(value, base);
    return url.protocol === 'http:' || url.protocol === 'https:' ? url : undefined;
  } catch {
    return undefined;
  }
}

/** A guarded `process.env[name]` read — the edge has no `process`, and a missing variable is `undefined`, never `''`. */
export function readEnv(name: string): string | undefined {
  try {
    const env = (globalThis as { process?: { env?: Record<string, string | undefined> } }).process?.env;
    if (env === null || typeof env !== 'object') return undefined;
    const value = env[name];
    return typeof value === 'string' && value !== '' ? value : undefined;
  } catch {
    return undefined;
  }
}

function nonEmptyString(value: unknown): string | undefined {
  return typeof value === 'string' && value !== '' ? value : undefined;
}

/**
 * The server runtimes' defaults (contract §4.2's table): `endpoint`, `apiKey` and `projectKey` fall back to their
 * `SENTINEL_*` variables. Returns the options with the defaults applied — the `apiKey` goes no further than the
 * target's headers (decision 36).
 */
export function withServerEnvDefaults<T extends InitOptions>(given: T): T {
  const out: T = { ...given };
  const endpoint = nonEmptyString(given.endpoint) ?? readEnv('SENTINEL_ENDPOINT');
  const apiKey = nonEmptyString(given.apiKey) ?? readEnv('SENTINEL_API_KEY');
  const projectKey = nonEmptyString(given.projectKey) ?? readEnv('SENTINEL_PROJECT_KEY');
  if (endpoint !== undefined) out.endpoint = endpoint;
  else delete out.endpoint;
  if (apiKey !== undefined) out.apiKey = apiKey;
  else delete out.apiKey;
  if (projectKey !== undefined) out.projectKey = projectKey;
  else delete out.projectKey;
  return out;
}

/**
 * node / edge (contract §4.2): `endpoint` or `apiKey` missing after the env defaults → the no-op client with
 * `not_configured` once; an `endpoint` that is not an absolute `http(s)` URL → the no-op client with
 * `invalid_option`, option `endpoint` (`'localhost:8080/ingest'`, `'/ingest'`, `'ftp://x/ingest'` are refused,
 * N15); otherwise the batch URL is `endpoint.replace(/\/$/, '') + '/batch'` with `X-API-Key` and, when set,
 * `X-Project-Key` (`packages/sdk-go/transport.go: sendBatch`).
 */
export function planServer(given: InitOptions): InitPlan {
  const endpoint = nonEmptyString(given.endpoint);
  const apiKey = nonEmptyString(given.apiKey);
  if (endpoint === undefined || apiKey === undefined) {
    const missing = [endpoint === undefined ? 'endpoint (SENTINEL_ENDPOINT)' : '', apiKey === undefined ? 'apiKey (SENTINEL_API_KEY)' : ''].filter((s) => s !== '');
    return {
      noop: true,
      reports: [{ code: 'not_configured', message: `${missing.join(' and ')} not set: the SDK is a no-op — every capture is accepted and dropped` }],
    };
  }
  if (absoluteHttpUrl(endpoint) === undefined) {
    return {
      noop: true,
      reports: [{ code: 'invalid_option', option: 'endpoint', message: `endpoint ${JSON.stringify(endpoint)} is not an absolute http(s) URL: the SDK is a no-op — nothing is posted` }],
    };
  }
  const headers: Record<string, string> = { 'Content-Type': 'application/json', 'X-API-Key': apiKey };
  const projectKey = nonEmptyString(given.projectKey);
  if (projectKey !== undefined) headers['X-Project-Key'] = projectKey;
  return { target: { url: `${endpoint.replace(/\/$/, '')}/batch`, headers }, noop: false, reports: [] };
}

/** `window.location.origin` as the SDK sees it — `globalThis.location` is `window.location` in a browser. */
function pageOrigin(): string | undefined {
  try {
    const origin = (globalThis as { location?: { origin?: unknown } }).location?.origin;
    return typeof origin === 'string' && origin !== '' && origin !== 'null' ? origin : undefined;
  } catch {
    return undefined;
  }
}

/**
 * browser (contract §4.2's `tunnel` row): resolve `new URL(tunnel, window.location.origin)` and accept only when
 * its `origin` EQUALS the page's — a path and an absolute URL on the page's own origin pass; an absolute URL
 * elsewhere and a protocol-relative `//evil.example/collect` are refused → the no-op client with
 * `invalid_option`, option `tunnel`, and nothing is ever posted off-origin (B19). The SDK posts to the resolved URL.
 */
export function planBrowser(given: InitOptions): InitPlan {
  const tunnel = typeof given.tunnel === 'string' ? given.tunnel : DEFAULT_TUNNEL;
  const origin = pageOrigin();
  const refuse = (why: string): InitPlan => ({
    noop: true,
    reports: [{ code: 'invalid_option', option: 'tunnel', message: `tunnel ${JSON.stringify(tunnel)} ${why}: the SDK is a no-op — nothing is posted` }],
  });
  if (origin === undefined) return refuse('cannot be resolved: no window.location.origin');
  const url = absoluteHttpUrl(tunnel, origin);
  if (url === undefined) return refuse('does not resolve to an http(s) URL');
  if (url.origin !== origin) return refuse(`resolves to ${url.origin}, not the page's origin ${origin}`);
  return { target: { url: url.href, headers: { 'Content-Type': 'application/json' } }, noop: false, reports: [] };
}

/**
 * The permissive plan `initCore` uses when no entry planned (the test helpers): the URL rules without the verdicts
 * — an absent or unusable target is a plain queue and no report; `not_configured` and the `endpoint` / `tunnel`
 * refusals are the entries' alone.
 */
export function planPermissive(given: InitOptions, runtime: Runtime): InitPlan {
  const none: InitPlan = { noop: false, reports: [] };
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (runtime === 'browser') {
    if (typeof given.tunnel !== 'string') return none;
    const url = absoluteHttpUrl(given.tunnel, pageOrigin());
    return url === undefined ? none : { target: { url: url.href, headers }, noop: false, reports: [] };
  }
  const endpoint = nonEmptyString(given.endpoint);
  if (endpoint === undefined || absoluteHttpUrl(endpoint) === undefined) return none;
  const apiKey = nonEmptyString(given.apiKey);
  if (apiKey !== undefined) headers['X-API-Key'] = apiKey;
  const projectKey = nonEmptyString(given.projectKey);
  if (projectKey !== undefined) headers['X-Project-Key'] = projectKey;
  return { target: { url: `${endpoint.replace(/\/$/, '')}/batch`, headers }, noop: false, reports: [] };
}
