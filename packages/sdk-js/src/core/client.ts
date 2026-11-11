// The client the transport hangs off (contract §4.2, §4.4): the transport options resolved with their
// degradations, the send target the entry planned (options.ts — `{endpoint}/batch` with `X-API-Key` on node and
// edge; the same-origin tunnel URL on the browser), the no-op client that accepts and drops every capture, the
// browser's hide listeners, and `flush` / `close` (contract §4.4, §4.3). The global handlers are handlers/ and
// hang off the same `teardown` list, so `close` and a second `init` uninstall them here.

import { debug, reportError } from './report.js';
import type { InitPlan } from './options.js';
import { type QueuedEvent, queuedEvent } from './queue.js';
import { getSlot, type InternalClient } from './state.js';
import { REQUEST_TIMEOUT_MS, Transport } from './transport/transport.js';
import type { AutoContexts, InitOptions, ResolvedOptions, Runtime, SentinelEvent } from './types.js';

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
  opts.installGlobalHandlers = given.installGlobalHandlers !== false;
  if (typeof given.endpoint === 'string') opts.endpoint = given.endpoint;
  if (typeof given.projectKey === 'string' && given.projectKey !== '') opts.projectKey = given.projectKey;
  if (typeof given.tunnel === 'string') opts.tunnel = given.tunnel;
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
 * Build the client for `runtime`: `core` is the runtime-agnostic half init.ts resolved; the transport options are
 * resolved here and the transport attached when the plan carries a target. A no-op plan (contract §4.2) gives a
 * client with no transport and no queue: `push` drops with a `debug` line, and the plan's reports fire once
 * through the new client's `onError`.
 */
export function createClient(core: ResolvedOptions, runtime: Runtime, given: InitOptions, plan: InitPlan, sdkName: string, autoContexts?: () => AutoContexts): InternalClient {
  const plain: string[] = [];
  let transport: Transport | undefined;
  const client: InternalClient = {
    options: core,
    runtime,
    sdkName,
    autoContexts,
    noop: plan.noop,
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
      if (plan.noop) debug(client, `no-op client: event ${event.id} dropped`);
      else if (transport === undefined) plain.push(event.json);
      else transport.enqueue(event);
    },
    teardown: [],
  };
  try {
    resolveTransportOptions(client, given);
    for (const r of plan.reports) reportError(client, r.code, r.message, r.option === undefined ? {} : { option: r.option });
    if (!plan.noop && plan.target !== undefined) {
      transport = new Transport({
        runtime,
        target: plan.target,
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
 * off finishes under the previous transport. The previous client's teardown hooks run: its hide listeners and
 * its global handlers go — uninstalled BEFORE the entry installs the new ones (contract §4.2), which is why this
 * runs before `next` is installed and the entry installs handlers only after `initCore` returns.
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

/**
 * The send's promise (contract §4.4 *Edge*, §4.7): resolves once the first attempt of the POST carrying `eventId`
 * has been acted on, so an edge isolate that awaits it stays alive for that one POST — and no longer than the
 * per-attempt timeout plus a pending async `beforeSend`, which is waited for up to that same timeout (a hook that
 * never settles must not hold a middleware response open forever). Resolves at once when nothing was enqueued
 * (no client, the no-op client, a dropped event). Never rejects.
 */
export async function sendPromise(eventId: string): Promise<void> {
  try {
    const slot = getSlot();
    if (slot.pending.size > 0) {
      const expired = deferred<void>();
      const timer = setTimeout(() => expired.resolve(), REQUEST_TIMEOUT_MS);
      unref(timer);
      try {
        while (slot.pending.size > 0) {
          const settled = await Promise.race([Promise.all([...slot.pending]).then(() => true as const), expired.promise.then(() => false as const)]);
          if (!settled) return;
        }
      } finally {
        clearTimeout(timer);
      }
    }
    const transport = slot.client?.transport;
    if (transport === undefined) return;
    await transport.sent(eventId);
  } catch {
    // Never rejects: Next awaits this before it writes the error response (contract §4.7).
  }
}
