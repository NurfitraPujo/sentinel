// The transport of contract §4.4 — `packages/sdk-go/transport.go: workerLoop` and `sendBatch` (`sendMaxAttempts =
// 4`) ported by specification onto promises and timers:
//
// - one ring buffer (queue.ts) and ONE sequential worker: a batch is dequeued, sent, retried through every
//   backoff sleep, and only then is the next one packed — a backoff stalls the drain (contract §4.4 *Sequential*);
// - a batch has a fixed identity from its first send: its events leave the ring buffer at dequeue, it is never
//   overflow-dropped or re-packed, it keeps its attempt count across the worker's retries, `flush` and hide-sends,
//   and it does not count toward `maxBufferSize` (contract §4.4 *Queue*);
// - every send counts as an attempt, wherever it came from, and no batch is sent more than four times by any
//   combination of worker, `flush` and hide (contract §4.4 *Retry*; B13, B14, B16);
// - `flush` interrupts the sleep a batch is in — backoff or `Retry-After` — once per batch, and never duplicates
//   an in-flight `fetch`; a hide-send re-sends only a batch in the SDK's OWN backoff (never one the server parked
//   with `Retry-After`), leaves an in-flight `fetch` alone, and then drains the queue head-by-head with
//   `keepalive`, each batch only after the previous resolved (contract §4.4 *`flush`*, *Page hide*);
// - `close` aborts the in-flight attempt, cancels any sleep, and is silent afterwards: no `fetch`, no `onError`
//   (contract §4.3 *`close`*; B23).
//
// Nothing here touches `window`, `document` or `process`: the runtime's hide listeners live in client.ts and
// call `hideSend`; `fetch` is whatever the config hands in (the global one by default, read at send time).

import { batchBytes, MAX_BATCH_BYTES, type QueuedEvent, RingBuffer } from '../queue.js';
import type { Runtime, SentinelSdkErrorCode, SentinelSdkErrorInit } from '../types.js';
import { parseRetryAfter } from './retry-after.js';

/** Total sends per batch — the first plus at most three retries (`sdk-go`'s `sendMaxAttempts`). */
export const MAX_ATTEMPTS = 4;
/** Per-attempt request timeout, via `AbortController` (contract §4.4 guidance). */
export const REQUEST_TIMEOUT_MS = 5000;
/** How much of a response body is read — the ingestor's error text is small; a misbehaving server's is not. */
export const MAX_BODY_READ_BYTES = 4096;
/** The exponential ladder (contract §4.4 guidance): 1 s, doubled per attempt, ±20 % jitter, never above 60 s. */
export const BACKOFF_BASE_MS = 1000;
export const BACKOFF_CAP_MS = 60_000;
export const BACKOFF_JITTER = 0.2;

export interface TransportTarget {
  /** `{endpoint}/batch` (node, edge) or the resolved tunnel URL (browser). */
  readonly url: string;
  /** `Content-Type` plus `X-API-Key` / `X-Project-Key` when set — never logged. */
  readonly headers: Readonly<Record<string, string>>;
}

export interface TransportConfig {
  readonly runtime: Runtime;
  readonly target: TransportTarget;
  readonly maxBufferSize: number;
  readonly batchSize: number;
  /** `0` means "drain on the next microtask, up to `batchSize`" — the edge build (contract §4.4 *Edge*). */
  readonly batchWaitMs: number;
  /** Injected for tests; the global `fetch` otherwise, looked up per send so a late polyfill is honoured. */
  readonly fetch?: typeof globalThis.fetch;
  readonly debug: (line: string) => void;
  readonly report: (code: SentinelSdkErrorCode, message: string, init?: SentinelSdkErrorInit) => void;
}

/** Where a send came from — the worker's ladder, a `flush`, or a page hide (keepalive on the browser). */
export type SendMode = 'worker' | 'flush' | 'hide';
type Wake = 'timer' | 'flush' | 'hide' | 'closed';
export type SleepReason = 'backoff' | 'retry-after';

export interface TransportStats {
  /** Events in the ring buffer. */
  queued: number;
  /** Events of the batch that has left the buffer (in flight or sleeping) — the "+ one batch" of the memory bound. */
  inFlight: number;
  /** Sends of that batch so far. */
  attempts: number;
  phase: 'idle' | 'sending' | 'sleeping';
  sleepReason?: SleepReason;
}

interface Deferred<T> {
  promise: Promise<T>;
  resolve: (value: T) => void;
}

// Node 20 is the floor (contract §4.1): the resolver is captured by hand, not by the ES2024 helper.
function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

/** Timers are `unref()`ed so an idle SDK never keeps a node process alive (contract §4.4 *Node process exit*). */
function unref(timer: unknown): void {
  const t = timer as { unref?: () => void } | null;
  if (t !== null && typeof t === 'object' && typeof t.unref === 'function') t.unref();
}

interface Batch {
  readonly events: readonly QueuedEvent[];
  readonly body: string;
  /** A single event over the 60 KiB cap: sent alone and never with `keepalive` (contract §4.4). */
  readonly oversize: boolean;
  attempts: number;
  /** The attempt whose `fetch` is in flight, if any. */
  inflight: { controller: AbortController } | undefined;
  /** The sleep the batch is waiting in, if any; `wake` ends it early. */
  sleep: { reason: SleepReason; wake: (how: Wake) => void } | undefined;
  /**
   * Resolves once the loop has acted on the current attempt's outcome — the batch is finished, or its next sleep
   * is set up. Replaced by `wake`, so a caller that woke the batch can await the send it triggered.
   */
  decided: Deferred<void>;
  /** The `flush` generation that last interrupted this batch's sleep: one interruption per batch per `flush`. */
  flushGen: number;
}

type Outcome =
  | { kind: 'done' }
  | { kind: 'closed' }
  | { kind: 'retry'; status: number | undefined; retryAfterMs: number | undefined; ignoredRetryAfter: string | undefined; message: string };

interface BatchResult {
  errors: { index: number; message: string }[];
}

/**
 * `apps/ingestor-go/main.go: batchResult` — `{ ingested, failed, errors: [{ index, message }], event_ids: [...] }`.
 * A cross-boundary shape with no compiler checking it (B5). `event_ids[].timestamp` is deliberately not read
 * (N14). `undefined` when the text is not that JSON — a plain-text body from `http.Error`.
 */
function parseBatchResult(text: string): BatchResult | undefined {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return undefined;
  const record = parsed as Record<string, unknown>;
  if (!Array.isArray(record.errors) && typeof record.ingested !== 'number') return undefined;
  const errors: BatchResult['errors'] = [];
  if (Array.isArray(record.errors)) {
    for (const entry of record.errors) {
      if (typeof entry !== 'object' || entry === null) continue;
      const e = entry as Record<string, unknown>;
      errors.push({ index: typeof e.index === 'number' ? e.index : -1, message: typeof e.message === 'string' ? e.message : '' });
    }
  }
  return { errors };
}

/** The body as opaque text, at most `max` bytes of it (contract §4.4: the transport never assumes JSON). */
async function readCapped(res: Response, max: number): Promise<string> {
  const body = res.body;
  if (body === null || body === undefined || typeof body.getReader !== 'function') {
    const text = await res.text();
    return text.length > max ? text.slice(0, max) : text;
  }
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (total < max) {
      const { done, value } = await reader.read();
      if (done) break;
      chunks.push(value);
      total += value.byteLength;
    }
  } finally {
    reader.cancel().catch(() => undefined);
  }
  const joined = new Uint8Array(Math.min(total, max));
  let offset = 0;
  for (const chunk of chunks) {
    const room = joined.length - offset;
    if (room <= 0) break;
    joined.set(chunk.byteLength > room ? chunk.subarray(0, room) : chunk, offset);
    offset += Math.min(chunk.byteLength, room);
  }
  return new TextDecoder().decode(joined);
}

export class Transport {
  readonly queue: RingBuffer;
  private current: Batch | undefined;
  private closed = false;
  private detached = false;
  private waitTimer: ReturnType<typeof setTimeout> | undefined;
  private drainQueued = false;
  private flushActive = 0;
  private flushGen = 0;
  private hiding = false;
  private idleWaiters: Array<() => void> = [];

  constructor(private readonly cfg: TransportConfig) {
    this.queue = new RingBuffer(cfg.maxBufferSize);
  }

  /** O(1), synchronous, never throws (contract §4.4 *Never blocks the caller*). */
  enqueue(event: QueuedEvent): void {
    if (this.closed || this.detached) return;
    this.queue.push(event);
    this.kick();
  }

  stats(): TransportStats {
    const b = this.current;
    const stats: TransportStats = {
      queued: this.queue.size,
      inFlight: b?.events.length ?? 0,
      attempts: b?.attempts ?? 0,
      phase: b === undefined ? 'idle' : b.sleep !== undefined ? 'sleeping' : 'sending',
    };
    if (b?.sleep !== undefined) stats.sleepReason = b.sleep.reason;
    return stats;
  }

  /**
   * `flush` (contract §4.4): interrupt the sleep the current batch is in — backoff or `Retry-After` — drain the
   * queue without waiting for `batchWaitMs`, and resolve `true` once nothing is queued or in flight, or `false`
   * when `until` settles first. An in-flight `fetch` is awaited, never re-sent. Never rejects.
   */
  async flush(until: Promise<false>): Promise<boolean> {
    if (this.closed || this.detached) return true;
    this.flushGen++;
    this.flushActive++;
    const gen = this.flushGen;
    try {
      const b = this.current;
      if (b?.sleep !== undefined && b.flushGen < gen) {
        b.flushGen = gen;
        b.sleep.wake('flush');
      }
      this.kick();
      return await Promise.race([this.idle(), until]);
    } catch {
      return false;
    } finally {
      this.flushActive--;
    }
  }

  /**
   * The page-hide send (contract §4.4 *Page hide*): after any in-flight `fetch` resolves, the batch in the SDK's
   * own backoff first as its own keepalive POST (attempt n + 1) — never one waiting on a server `Retry-After` —
   * then the head ≤ 60 KiB of the queue, each next batch only after the previous resolved. A failed keepalive
   * leaves its events for the worker (they become the worker's current batch, attempt count kept). Never rejects.
   */
  async hideSend(): Promise<void> {
    if (this.closed || this.detached || this.hiding) return;
    this.hiding = true;
    try {
      let b = this.current;
      if (b?.inflight !== undefined) {
        this.cfg.debug('hide-send: a fetch is in flight; starting after it resolves');
        await b.decided.promise;
      }
      b = this.current;
      if (b?.sleep !== undefined) {
        if (b.sleep.reason === 'retry-after') {
          this.cfg.debug(`hide-send: batch of ${b.events.length} is waiting on Retry-After; not re-sent`);
          return;
        }
        b.sleep.wake('hide');
        await b.decided.promise;
      }
      while (!this.closed && this.current === undefined && this.queue.size > 0) {
        const batch = this.dequeue();
        if (batch === undefined) break;
        this.run(batch, 'hide');
        await batch.decided.promise;
      }
    } catch {
      // Best-effort: nothing on the hide path may throw into a visibilitychange listener.
    } finally {
      this.hiding = false;
    }
  }

  /**
   * `close`'s transport half (contract §4.3 *`close`*): abort the in-flight attempt, cancel any sleep, empty the
   * queue and go silent — no `fetch`, no `onError` after this returns. Returns the number of events dropped.
   */
  close(): number {
    if (this.closed) return 0;
    this.closed = true;
    this.clearWaitTimer();
    let count = this.queue.drain().length;
    const b = this.current;
    if (b !== undefined) {
      count += b.events.length;
      b.inflight?.controller.abort();
      b.sleep?.wake('closed');
    }
    this.settleIdle(true);
    return count;
  }

  /**
   * A second `init`'s drain (contract §4.2): the not-yet-sent queue leaves with the caller; the batch in flight
   * or backing off stays and finishes here, under this transport's options and `onError`.
   */
  detach(): QueuedEvent[] {
    this.detached = true;
    this.clearWaitTimer();
    const events = this.queue.drain();
    this.settleIdle(true);
    return events;
  }

  // --- the worker ---------------------------------------------------------------------------------------

  private kick(): void {
    if (this.closed || this.detached || this.current !== undefined) return;
    if (this.queue.size === 0) {
      this.clearWaitTimer();
      this.settleIdle();
      return;
    }
    const microtask = this.cfg.batchWaitMs === 0 || this.flushActive > 0;
    if (microtask || this.queue.size >= this.cfg.batchSize) {
      // Never from inside `enqueue`: the caller's loop returns before anything is dequeued (B15). The edge
      // build and a `flush` drain on the next microtask; a full batch elsewhere on a zero timer, so the worker
      // stays on timers as `sdk-go`'s ticker does.
      if (this.drainQueued) return;
      this.drainQueued = true;
      if (microtask) {
        queueMicrotask(() => this.drain());
      } else {
        const t = setTimeout(() => this.drain(), 0);
        unref(t);
      }
      return;
    }
    if (this.waitTimer === undefined) {
      this.waitTimer = setTimeout(() => this.drain(), this.cfg.batchWaitMs);
      unref(this.waitTimer);
    }
  }

  private clearWaitTimer(): void {
    if (this.waitTimer !== undefined) {
      clearTimeout(this.waitTimer);
      this.waitTimer = undefined;
    }
  }

  private drain(): void {
    this.drainQueued = false;
    this.clearWaitTimer();
    if (this.closed || this.detached || this.current !== undefined) return;
    const batch = this.dequeue();
    if (batch === undefined) {
      this.settleIdle();
      return;
    }
    this.run(batch, 'worker');
  }

  /** Pack the head batch and make it current. `buffer_overflow` is reported here, once, with the count (B15). */
  private dequeue(): Batch | undefined {
    const dropped = this.queue.takeDropped();
    if (dropped > 0) {
      this.cfg.report('buffer_overflow', `${dropped} event(s) dropped: the buffer of ${this.cfg.maxBufferSize} overflowed`, { count: dropped });
    }
    const events = this.queue.takeBatch(this.cfg.batchSize, MAX_BATCH_BYTES);
    if (events.length === 0) return undefined;
    const body = `[${events.map((e) => e.json).join(',')}]`;
    const batch: Batch = {
      events,
      body,
      oversize: batchBytes(events) > MAX_BATCH_BYTES,
      attempts: 0,
      inflight: undefined,
      sleep: undefined,
      decided: deferred(),
      flushGen: 0,
    };
    if (batch.oversize) this.cfg.debug(`event ${events[0]?.id} is ${batch.body.length} bytes serialised, over the 60 KiB keepalive cap: sent alone, without keepalive`);
    this.current = batch;
    return batch;
  }

  private run(batch: Batch, first: SendMode): void {
    void this.ladder(batch, first).finally(() => {
      if (this.current === batch) this.current = undefined;
      batch.decided.resolve();
      this.kick();
    });
  }

  /** `sendBatch`'s loop: attempt, classify, sleep, until done, dropped at four attempts, or closed. */
  private async ladder(batch: Batch, first: SendMode): Promise<void> {
    let mode = first;
    for (;;) {
      const outcome = await this.attempt(batch, mode);
      if (outcome.kind !== 'retry') return;
      if (batch.attempts >= MAX_ATTEMPTS) {
        const init: SentinelSdkErrorInit = { attempts: batch.attempts, count: batch.events.length };
        if (outcome.status !== undefined) init.status = outcome.status;
        this.cfg.report('batch_dropped', `dropped ${batch.events.length} event(s) after ${batch.attempts} attempts: ${outcome.message}`, init);
        return;
      }
      let delay = this.ladderDelay(batch.attempts);
      let reason: SleepReason = 'backoff';
      if (outcome.retryAfterMs !== undefined) {
        delay = outcome.retryAfterMs;
        reason = 'retry-after';
      } else if (outcome.ignoredRetryAfter !== undefined) {
        this.cfg.debug(`Retry-After ${JSON.stringify(outcome.ignoredRetryAfter)} ignored (unparseable, ≤ 0 or in the past); the ladder's ${Math.round(delay)} ms applies`);
      }
      if (this.flushActive > 0 && batch.flushGen < this.flushGen) {
        // The caller asked to send now (contract §4.4 *`flush`*): this sleep is skipped, once per flush.
        batch.flushGen = this.flushGen;
        this.cfg.debug(`flush: attempt ${batch.attempts} failed (${outcome.message}); re-sending ${batch.events.length} event(s) now`);
        mode = 'flush';
        continue;
      }
      this.cfg.debug(`attempt ${batch.attempts}/${MAX_ATTEMPTS} failed (${outcome.message}); retrying ${batch.events.length} event(s) in ${Math.round(delay)} ms (${reason})`);
      const sleeping = this.sleep(batch, delay, reason);
      batch.decided.resolve();
      const how = await sleeping;
      if (how === 'closed' || this.closed) return;
      mode = how === 'hide' ? 'hide' : how === 'flush' ? 'flush' : 'worker';
    }
  }

  private ladderDelay(attempts: number): number {
    const base = BACKOFF_BASE_MS * 2 ** Math.max(0, attempts - 1);
    const jitter = 1 + (Math.random() * 2 - 1) * BACKOFF_JITTER;
    return Math.min(BACKOFF_CAP_MS, base * jitter);
  }

  private sleep(batch: Batch, ms: number, reason: SleepReason): Promise<Wake> {
    return new Promise<Wake>((resolve) => {
      const finish = (how: Wake): void => {
        clearTimeout(timer);
        batch.sleep = undefined;
        // The next attempt gets a fresh `decided`, so a waker can await the send it triggered.
        batch.decided = deferred();
        resolve(how);
      };
      const timer = setTimeout(() => finish('timer'), ms);
      unref(timer);
      batch.sleep = { reason, wake: finish };
    });
  }

  /** One POST of the batch's fixed body: `attempts` incremented, the outcome classified per contract §4.4. */
  private async attempt(batch: Batch, mode: SendMode): Promise<Outcome> {
    batch.attempts++;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);
    unref(timer);
    batch.inflight = { controller };
    const keepalive = this.cfg.runtime === 'browser' && !batch.oversize;
    this.cfg.debug(`sending ${batch.events.length} event(s), ${batch.body.length} bytes, attempt ${batch.attempts}/${MAX_ATTEMPTS} (${mode}${keepalive ? ', keepalive' : ''})`);
    try {
      const init: RequestInit = {
        method: 'POST',
        headers: { ...this.cfg.target.headers },
        body: batch.body,
        signal: controller.signal,
      };
      if (this.cfg.runtime === 'browser') {
        init.keepalive = keepalive;
        init.credentials = 'omit';
      }
      const doFetch = this.cfg.fetch ?? globalThis.fetch;
      const res = await doFetch(this.cfg.target.url, init);
      const text = await readCapped(res, MAX_BODY_READ_BYTES);
      if (this.closed) return { kind: 'closed' };
      return this.classify(batch, res, text);
    } catch (cause) {
      if (this.closed) return { kind: 'closed' };
      const why = controller.signal.aborted ? `timeout after ${REQUEST_TIMEOUT_MS} ms` : `network error: ${String(cause)}`;
      return { kind: 'retry', status: undefined, retryAfterMs: undefined, ignoredRetryAfter: undefined, message: why };
    } finally {
      clearTimeout(timer);
      batch.inflight = undefined;
    }
  }

  /** Response reading (contract §4.4, decision 33). Reports `item_rejected` / `batch_dropped` as it decides. */
  private classify(batch: Batch, res: Response, text: string): Outcome {
    const status = res.status;
    const count = batch.events.length;
    if (status >= 200 && status < 300) {
      const result = parseBatchResult(text);
      for (const e of result?.errors ?? []) {
        // The batch landed; the failed item is not retried (decision 33's accepted loss).
        this.cfg.report('item_rejected', `${e.index}: ${e.message}`, { status, count: 1, cause: { index: e.index, event_id: batch.events[e.index]?.id } });
      }
      this.cfg.debug(`batch of ${count} accepted (${status}) on attempt ${batch.attempts}${result !== undefined && result.errors.length > 0 ? `, ${result.errors.length} item(s) rejected` : ''}`);
      return { kind: 'done' };
    }
    if (status === 429 || status >= 500) {
      const header = res.headers.get('retry-after');
      const retryAfterMs = parseRetryAfter(header);
      return {
        kind: 'retry',
        status,
        retryAfterMs,
        ignoredRetryAfter: header !== null && retryAfterMs === undefined ? header : undefined,
        message: `status ${status}`,
      };
    }
    if (status === 400) {
      // Decision 33's split: the ingestor puts a NATS publish failure into the same `errors[]` as a validation
      // failure, and only its `validation failed` prefix tells them apart (main.go: handleBatchIngest).
      const result = parseBatchResult(text);
      const message = result === undefined ? text : result.errors.map((e) => `${e.index}: ${e.message}`).join('\n');
      const permanent = result === undefined || result.errors.some((e) => e.message.startsWith('validation failed'));
      if (!permanent) return { kind: 'retry', status, retryAfterMs: undefined, ignoredRetryAfter: undefined, message: `status 400: ${message}` };
      this.cfg.report('batch_dropped', message, { status, attempts: batch.attempts, count });
      return { kind: 'done' };
    }
    // Every other status: resending the same payload cannot change the outcome.
    this.cfg.report('batch_dropped', text === '' ? `status ${status}` : text, { status, attempts: batch.attempts, count });
    return { kind: 'done' };
  }

  // --- idleness -----------------------------------------------------------------------------------------

  private idle(): Promise<true> {
    if (this.current === undefined && this.queue.size === 0) return Promise.resolve(true);
    return new Promise<true>((resolve) => {
      this.idleWaiters.push(() => resolve(true));
    });
  }

  /** Resolve every `flush` waiting for idleness; `force` after `close` / `detach`, where nothing more will be sent. */
  private settleIdle(force = false): void {
    if (!force && (this.current !== undefined || this.queue.size > 0)) return;
    const waiters = this.idleWaiters;
    this.idleWaiters = [];
    for (const w of waiters) w();
  }
}
