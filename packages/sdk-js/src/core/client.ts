// The client the transport hangs off (contract §4.2, §4.4): the transport options resolved with their
// degradations, the send target derived from the runtime (`{endpoint}/batch` with `X-API-Key` on node and edge;
// the tunnel URL on the browser), the browser's hide listeners, and `flush` / `close` (contract §4.4, §4.3). The
// runtime entries' `init` — env defaults, the tunnel's same-origin rule, the `not_configured` no-op client, the
// global handlers — is task 5's; this file is the plumbing that drives the transport.

import { debug, reportError } from './report.js';
import { type QueuedEvent, queuedEvent } from './queue.js';
import { getSlot, type InternalClient } from './state.js';
import { Transport, type TransportTarget } from './transport/transport.js';
import type { InitOptions, ResolvedOptions, Runtime, SentinelEvent } from './types.js';

export const DEFAULT_MAX_BUFFER_SIZE = 100;
export const DEFAULT_BATCH_SIZE = 10;
export const DEFAULT_BATCH_WAIT_MS = 1000;
/** The tunnel's `maxItems` (contract §4.5) — the clamp is the tunnel's cap, not the ingestor's. */
export const MAX_BATCH_SIZE = 50;
export const DEFAULT_FLUSH_TIMEOUT_MS = 2000;

/** Node 20 is the floor (contract §4.1): the resolver is captured by hand, not by the ES2024 helper. */
function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

function unref(timer: unknown): void {
  const t = timer as { unref?: () => void } | null;
  if (t !== null && typeof t === 'object' && typeof t.unref === 'function') t.unref();
}

/**
 * A numeric option (contract §4.2 *Numeric options degrade, never throw*): non-finite or ≤ 0 → the default with
 * `invalid_option` once, naming the option.
 */
function positiveNumber(client: InternalClient, name: string, given: unknown, fallback: number): number {
  if (given === undefined) return fallback;
  if (typeof given !== 'number' || !Number.isFinite(given) || given <= 0) {
    reportError(client, 'invalid_option', `${name} ${String(given)} is not a positive finite number; using ${fallback}`, { option: name });
    return fallback;
  }
  return given;
}

function resolveTransportOptions(client: InternalClient, given: InitOptions): void {
  const opts = client.options;
  opts.maxBufferSize = Math.floor(positiveNumber(client, 'maxBufferSize', given.maxBufferSize, DEFAULT_MAX_BUFFER_SIZE)) || DEFAULT_MAX_BUFFER_SIZE;

  const size = positiveNumber(client, 'batchSize', given.batchSize, DEFAULT_BATCH_SIZE);
  const clamped = Math.min(MAX_BATCH_SIZE, Math.max(1, Math.floor(size)));
  if (clamped !== size) {
    reportError(client, 'invalid_option', `batchSize ${size} is outside [1, ${MAX_BATCH_SIZE}]; clamped to ${clamped}`, { option: 'batchSize' });
  }
  opts.batchSize = clamped;

  if (client.runtime === 'edge') {
    // The edge build forces batchWaitMs = 0 — "drain on the next microtask, up to batchSize" (contract §4.4 *Edge*).
    if (given.batchWaitMs !== undefined) debug(client, `batchWaitMs is ignored on the edge build (forced to 0: drain on the next microtask, up to batchSize)`);
    opts.batchWaitMs = 0;
  } else {
    opts.batchWaitMs = positiveNumber(client, 'batchWaitMs', given.batchWaitMs, DEFAULT_BATCH_WAIT_MS);
  }

  opts.flushOnHide = given.flushOnHide !== false;
  if (typeof given.endpoint === 'string') opts.endpoint = given.endpoint;
  if (typeof given.projectKey === 'string' && given.projectKey !== '') opts.projectKey = given.projectKey;
  if (typeof given.tunnel === 'string') opts.tunnel = given.tunnel;
}

/** `new URL(value)` succeeding with an `http:` / `https:` protocol (contract §4.2's `endpoint` row). */
function absoluteHttpUrl(value: string, base?: string): URL | undefined {
  try {
    const url = base === undefined ? new URL(value) : new URL(value, base);
    return url.protocol === 'http:' || url.protocol === 'https:' ? url : undefined;
  } catch {
    return undefined;
  }
}

/**
 * The send target (contract §4.4 *Batching*): `{endpoint}/batch` on node and edge with the key headers; the
 * resolved tunnel URL on the browser (the same-origin rule that refuses it is task 5's, B19). `undefined` when
 * nothing usable was given — the client then keeps a plain queue and sends nothing.
 */
function resolveTarget(client: InternalClient, given: InitOptions): TransportTarget | undefined {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (client.runtime === 'browser') {
    const tunnel = client.options.tunnel;
    if (tunnel === undefined) return undefined;
    const origin = (globalThis as { location?: { origin?: string } }).location?.origin;
    const url = absoluteHttpUrl(tunnel, typeof origin === 'string' ? origin : undefined);
    if (url === undefined) {
      debug(client, `tunnel ${JSON.stringify(tunnel)} does not resolve to an http(s) URL; nothing will be sent`);
      return undefined;
    }
    return { url: url.href, headers };
  }
  const endpoint = client.options.endpoint;
  if (endpoint === undefined) return undefined;
  const url = absoluteHttpUrl(endpoint);
  if (url === undefined) {
    debug(client, `endpoint ${JSON.stringify(endpoint)} is not an absolute http(s) URL; nothing will be sent`);
    return undefined;
  }
  if (typeof given.apiKey === 'string' && given.apiKey !== '') headers['X-API-Key'] = given.apiKey;
  if (client.options.projectKey !== undefined) headers['X-Project-Key'] = client.options.projectKey;
  return { url: `${endpoint.replace(/\/$/, '')}/batch`, headers };
}

/**
 * `flushOnHide` (contract §4.4 *Page hide*): `visibilitychange → hidden` and `pagehide` trigger the hide-send.
 * Installed only where a `document` with `addEventListener` exists; removed by the client's teardown.
 */
function installHideListeners(client: InternalClient, transport: Transport): void {
  const doc = (globalThis as { document?: EventTarget & { visibilityState?: string } }).document;
  const win = (globalThis as { window?: EventTarget }).window;
  if (doc === undefined || typeof doc.addEventListener !== 'function') return;
  const onVisibility = (): void => {
    if (doc.visibilityState === 'hidden') void transport.hideSend();
  };
  const onPageHide = (): void => {
    void transport.hideSend();
  };
  try {
    doc.addEventListener('visibilitychange', onVisibility);
    win?.addEventListener?.('pagehide', onPageHide);
  } catch (cause) {
    reportError(client, 'handler_install_failed', `flushOnHide listeners could not be installed: ${String(cause)}`, { cause });
    return;
  }
  client.teardown.push(() => {
    doc.removeEventListener('visibilitychange', onVisibility);
    win?.removeEventListener?.('pagehide', onPageHide);
  });
}

/**
 * Build the client for `runtime`: `core` is the runtime-agnostic half init.ts resolved; the transport options and
 * the target are resolved here and the transport attached when the target exists.
 */
export function createClient(core: ResolvedOptions, runtime: Runtime, given: InitOptions): InternalClient {
  const plain: string[] = [];
  let transport: Transport | undefined;
  const client: InternalClient = {
    options: core,
    runtime,
    get transport() {
      return transport;
    },
    get queue(): readonly string[] {
      return transport === undefined ? plain : transport.queue.snapshot().map((e) => e.json);
    },
    enqueue(json: string, event: SentinelEvent) {
      client.push(queuedEvent(event.event_id, json));
    },
    push(event: QueuedEvent) {
      if (transport === undefined) plain.push(event.json);
      else transport.enqueue(event);
    },
    teardown: [],
  };
  try {
    resolveTransportOptions(client, given);
    const target = resolveTarget(client, given);
    if (target !== undefined) {
      transport = new Transport({
        runtime,
        target,
        maxBufferSize: core.maxBufferSize,
        batchSize: core.batchSize,
        batchWaitMs: core.batchWaitMs,
        debug: (line) => debug(client, line),
        report: (code, message, init) => reportError(client, code, message, init),
      });
      if (runtime === 'browser' && core.flushOnHide) installHideListeners(client, transport);
    }
  } catch (cause) {
    debug(client, `init: transport setup failed inside the SDK: ${String(cause)}`);
  }
  return client;
}

/** What a previous client still holds unsent: its ring buffer's events, or its plain queue's strings. */
function unsent(previous: InternalClient): QueuedEvent[] {
  const transport = previous.transport;
  if (transport !== undefined) return transport.detach();
  const events = [...previous.queue];
  (previous.queue as string[]).length = 0;
  return events.map((json) => {
    let id = '';
    try {
      id = String((JSON.parse(json) as { event_id?: unknown }).event_id ?? '');
    } catch {
      // A string the pipeline wrote always parses; keep the event in the drain anyway.
    }
    return queuedEvent(id, json);
  });
}

/**
 * A second `init`'s drain (contract §4.2): the previous client's not-yet-sent queue moves into `next` — the same
 * `event_id`s, the same bytes (an already-built event is immutable, B22) — while a batch in flight or backing
 * off finishes under the previous transport. The previous client's teardown hooks run: its hide listeners go,
 * and the global handlers with them once task 5 registers theirs (they are uninstalled before the new ones are
 * installed, which is why this runs before `next` is installed).
 */
export function replaceClient(previous: InternalClient, next: InternalClient): void {
  const events = unsent(previous);
  for (const hook of previous.teardown.splice(0)) {
    try {
      hook();
    } catch {
      // A teardown that throws must not stop the others.
    }
  }
  for (const e of events) next.push(e);
  if (events.length > 0) debug(next, `init: ${events.length} queued event(s) drained from the previous client`);
}

/** `timeoutMs` under the numeric-options rule (contract §4.2): non-finite or ≤ 0 → 2 000 with `invalid_option` once. */
function resolveTimeout(client: InternalClient | undefined, timeoutMs: unknown): number {
  if (timeoutMs === undefined) return DEFAULT_FLUSH_TIMEOUT_MS;
  if (typeof timeoutMs !== 'number' || !Number.isFinite(timeoutMs) || timeoutMs <= 0) {
    reportError(client, 'invalid_option', `timeoutMs ${String(timeoutMs)} is not a positive finite number; using ${DEFAULT_FLUSH_TIMEOUT_MS}`, { option: 'timeoutMs' });
    return DEFAULT_FLUSH_TIMEOUT_MS;
  }
  return timeoutMs;
}

/**
 * `flush(timeoutMs = 2000)` (contract §4.4): waits for pending async `beforeSend` hooks, drains the queue and
 * waits for in-flight sends, all up to the one timeout; `true` when empty, `false` on timeout — never rejects.
 */
export async function flush(timeoutMs?: number): Promise<boolean> {
  const slot = getSlot();
  const client = slot.client;
  const timeout = resolveTimeout(client, timeoutMs);
  if (client === undefined) return true;
  const expired = deferred<false>();
  const timer = setTimeout(() => expired.resolve(false), timeout);
  unref(timer);
  try {
    // Pending hooks first: each enqueues into the client current when it settles, and that event is then part
    // of what this flush drains.
    while (slot.pending.size > 0) {
      const settled = await Promise.race([Promise.all([...slot.pending]).then(() => true as const), expired.promise]);
      if (!settled) return false;
    }
    const transport = slot.client?.transport;
    if (transport === undefined) return true;
    return await transport.flush(expired.promise);
  } catch {
    return false;
  } finally {
    clearTimeout(timer);
  }
}

/**
 * `close(timeoutMs = 2000)` (contract §4.3): `flush`, then — whatever it resolved — the in-flight attempt aborted,
 * every sleep cancelled, the leftovers dropped with one `debug` line naming the count, the teardown hooks run and
 * the client cleared so `getClient()` is `undefined`. Resolves with `flush`'s boolean; never rejects.
 */
export async function close(timeoutMs?: number): Promise<boolean> {
  const slot = getSlot();
  const client = slot.client;
  if (client === undefined) return true;
  let flushed = false;
  try {
    flushed = await flush(timeoutMs);
  } catch {
    flushed = false;
  }
  let count = 0;
  try {
    count = client.transport !== undefined ? client.transport.close() : client.queue.length;
    if (client.transport === undefined) (client.queue as string[]).length = 0;
    count += slot.pending.size;
  } catch {
    // Silence is the rule after close; nothing here may throw.
  }
  for (const hook of client.teardown.splice(0)) {
    try {
      hook();
    } catch {
      // A teardown that throws must not stop the others.
    }
  }
  if (slot.client === client) delete slot.client;
  debug(client, `close: ${count} event(s) dropped (queued, in flight, sleeping or held by a pending beforeSend); flush resolved ${flushed}`);
  return flushed;
}
