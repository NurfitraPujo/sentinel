// `debug` lines and `onError` reports (contract §4.2's rows): `debug` goes to `console.debug` in the browser and
// to stderr in node, never carrying the key or a body; `onError` is the caller's callback, wrapped so it never
// throws outward. Both are no-ops before the first `init` — there is no client to hold the options.

import type { InternalClient } from './state.js';
import { SentinelSdkError, type SentinelSdkErrorCode, type SentinelSdkErrorInit } from './types.js';

export function debug(client: InternalClient | undefined, line: string): void {
  if (client === undefined || !client.options.debug) return;
  const text = `[@sentinel/sdk-js] ${line}`;
  try {
    const proc = (globalThis as { process?: { stderr?: { write?: (s: string) => unknown } } }).process;
    if (client.runtime === 'node' && typeof proc?.stderr?.write === 'function') {
      proc.stderr.write(text + '\n');
      return;
    }
    console.debug(text);
  } catch {
    // A logger that throws is not the SDK's problem to surface.
  }
}

export function reportError(
  client: InternalClient | undefined,
  code: SentinelSdkErrorCode,
  message: string,
  init: SentinelSdkErrorInit = {},
): void {
  if (client === undefined) return;
  debug(client, `onError ${code}: ${message}`);
  const onError = client.options.onError;
  if (typeof onError !== 'function') return;
  try {
    onError(new SentinelSdkError(code, message, init));
  } catch {
    // Wrapped: a throwing onError never reaches the caller of captureException (contract §4.2).
  }
}
