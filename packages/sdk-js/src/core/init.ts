// The runtime-agnostic half of `init` (contract §4.2): the options the event pipeline reads, resolved with the
// degradations their rows pin — `release` over 100 bytes omitted with `release_too_long` (N10), `environment`
// normalised with `invalid_option` (N11), `sampleRate` defaulted / clamped with `invalid_option` — and the client
// installed on the `globalThis` slot, with the transport of §4.4 attached by client.ts when the entry's plan
// (options.ts) carries a target. The runtime entries (`./browser`, `./node`, `./edge`) wrap this with their own
// option types, the env defaults, the plan and the global handlers (handlers/); `initCore` is synchronous,
// idempotent and never throws — a second call drains the previous client and runs its teardown (the old global
// handlers go) before the new client is installed.

import { cutUtf8, utf8Bytes } from './bytes.js';
import { createClient, replaceClient } from './client.js';
import { type InitPlan, planPermissive } from './options.js';
import { debug, reportError } from './report.js';
import { getSlot, type InternalClient } from './state.js';
import type { InitOptions, ResolvedOptions, Runtime, SentinelClient } from './types.js';

export const MAX_RELEASE_BYTES = 100;
export const DEFAULT_ENVIRONMENT = 'production';

/**
 * `environment` → `^[a-z0-9][a-z0-9._-]{0,49}$` in this order (contract §4.2): lower-cased; every character
 * outside `[a-z0-9._-]` replaced with `-`; leading characters outside `[a-z0-9]` stripped; cut to 50 bytes;
 * empty → `production`. Returns the value and whether it changed.
 */
export function normaliseEnvironment(given: unknown): { value: string; changed: boolean } {
  const input = typeof given === 'string' ? given : '';
  let out = input.toLowerCase();
  out = Array.from(out, (ch) => (/^[a-z0-9._-]$/.test(ch) ? ch : '-')).join('');
  out = out.replace(/^[^a-z0-9]+/, '');
  out = cutUtf8(out, 50);
  if (out === '') out = DEFAULT_ENVIRONMENT;
  return { value: out, changed: out !== input };
}

function resolve(client: InternalClient, given: InitOptions): void {
  const opts = client.options;

  const release = typeof given.release === 'string' ? given.release : '';
  if (utf8Bytes(release) > MAX_RELEASE_BYTES) {
    reportError(client, 'release_too_long', `release is ${utf8Bytes(release)} bytes, over ${MAX_RELEASE_BYTES}; release_version is omitted from every event`);
    opts.release = '';
  } else {
    opts.release = release;
  }

  const env = normaliseEnvironment(given.environment ?? DEFAULT_ENVIRONMENT);
  opts.environment = env.value;
  if (env.changed) {
    debug(client, `environment normalised: ${JSON.stringify(given.environment)} → ${JSON.stringify(env.value)}`);
    reportError(client, 'invalid_option', `environment ${JSON.stringify(given.environment)} normalised to ${JSON.stringify(env.value)}`, { option: 'environment' });
  }

  const rate = given.sampleRate;
  if (rate === undefined) {
    opts.sampleRate = 1;
  } else if (typeof rate !== 'number' || !Number.isFinite(rate)) {
    opts.sampleRate = 1;
    reportError(client, 'invalid_option', `sampleRate ${String(rate)} is not a finite number; using 1`, { option: 'sampleRate' });
  } else if (rate < 0 || rate > 1) {
    opts.sampleRate = rate < 0 ? 0 : 1;
    reportError(client, 'invalid_option', `sampleRate ${rate} is outside [0, 1]; clamped to ${opts.sampleRate}`, { option: 'sampleRate' });
  } else {
    opts.sampleRate = rate;
  }

  if (given.tracesSampleRate !== undefined) debug(client, 'tracesSampleRate is accepted and ignored');
}

/**
 * Install the client for `runtime` with `options` resolved and `plan` deciding the target (the entry's verdict:
 * options.ts's `planServer` / `planBrowser`; the permissive plan when a test calls this directly). Synchronous,
 * idempotent, never throws.
 */
export function initCore(options: InitOptions | undefined, runtime: Runtime, plan?: InitPlan): InternalClient {
  const given: InitOptions = options !== null && typeof options === 'object' ? options : {};
  const resolved: ResolvedOptions = {
    release: '',
    environment: DEFAULT_ENVIRONMENT,
    sampleRate: 1,
    debug: given.debug === true,
    maxBufferSize: 100,
    batchSize: 10,
    batchWaitMs: 1000,
    flushOnHide: true,
    installGlobalHandlers: true,
  };
  if (typeof given.beforeSend === 'function') resolved.beforeSend = given.beforeSend;
  if (typeof given.onError === 'function') resolved.onError = given.onError;
  if (typeof given.inApp === 'function') resolved.inApp = given.inApp;
  let planned: InitPlan;
  try {
    planned = plan ?? planPermissive(given, runtime);
  } catch {
    planned = { noop: false, reports: [] };
  }
  const client = createClient(resolved, runtime, given, planned);
  try {
    resolve(client, given);
  } catch (cause) {
    debug(client, `init: option resolution failed inside the SDK: ${String(cause)}`);
  }
  const slot = getSlot();
  const previous = slot.client;
  if (previous !== undefined) {
    // A second `init` drains the old client's not-yet-sent queue into the new one; a batch in flight or backing
    // off finishes under the old transport (contract §4.2).
    try {
      replaceClient(previous, client);
    } catch (cause) {
      debug(client, `init: draining the previous client failed inside the SDK: ${String(cause)}`);
    }
  }
  slot.client = client;
  return client;
}

/** Decision 36: the resolved options and the runtime, nothing callable; `undefined` before the first `init` and after `close`. */
export function getClient(): SentinelClient | undefined {
  const client = getSlot().client;
  if (client === undefined) return undefined;
  return Object.freeze({ options: Object.freeze({ ...client.options }), runtime: client.runtime });
}
