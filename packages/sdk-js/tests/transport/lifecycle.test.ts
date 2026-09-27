// `flush`, `close`, the async `beforeSend` forms, a second `init` with a batch in flight, the oversize event and
// the edge drain (contract §4.2, §4.3 *`close`*, §4.4): B13 (unit), B9 (async forms), B22 (transport half), B23
// (close), B24, N2 (unit half). Against the local node:http stub, delays on the fake clock. Shuffle-safe: every
// test starts from a deleted globalThis slot, its own stub and its own client.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { captureException, captureMessage } from '../../src/core/capture.js';
import { close, flush } from '../../src/core/client.js';
import { getClient, initCore } from '../../src/core/init.js';
import { setUser } from '../../src/core/scope.js';
import { getInternalClient } from '../../src/core/state.js';
import type { SentinelEvent, SentinelSdkError } from '../../src/core/types.js';
import { resetPipeline } from '../helpers/pipeline.js';
import { ACCEPTED, refusedUrl, settle, startStub, type StubServer, until } from '../helpers/stub-server.js';
import { advance, setupTransport, stopTransport, useFakeClock } from '../helpers/transport.js';

let stubs: StubServer[] = [];

vi.setConfig({ testTimeout: 30_000 });

async function stub(...args: Parameters<typeof startStub>): Promise<StubServer> {
  const s = await startStub(...args);
  stubs.push(s);
  return s;
}

beforeEach(() => {
  resetPipeline();
  useFakeClock();
});

afterEach(async () => {
  stopTransport();
  vi.useRealTimers();
  for (const s of stubs) await s.close();
  stubs = [];
});

describe('B13 (unit): flush(2000) while one batch is in flight and the next answer is a 429 + Retry-After: 60', () => {
  it('B13: the in-flight batch is awaited, not re-sent; the waiting batch goes at once as its own POST (attempt 2); only then is a fresh batch packed; flush never rejects', async () => {
    const s = await stub((req) => (req.n === 1 ? 'hold' : req.n === 2 ? { status: 429, body: 'Rate limit exceeded', headers: { 'retry-after': '60' } } : ACCEPTED(req)));
    const h = setupTransport(s.url);
    const first = captureMessage('in flight');
    await advance(1000);
    await s.waitFor(1); // held
    const queued = [captureMessage('queued 1'), captureMessage('queued 2')];

    const flushed = flush(2000);
    await settle(50);
    expect(s.requests).toHaveLength(1); // awaited, not duplicated
    s.requests[0]?.answer({ status: 429, body: 'Rate limit exceeded', headers: { 'retry-after': '60' } });

    // The Retry-After wait is interrupted: attempt 2 of the same batch, alone, with no clock advance.
    await s.waitFor(2);
    expect(s.requests[1]?.ids).toEqual([first]);
    expect(h.debugLines.some((l) => l.includes('attempt 2/4 (flush)'))).toBe(true);
    // Its answer is another 429, not interrupted a second time by the same flush; flush then times out.
    await advance(2000);
    expect(await flushed).toBe(false);
    expect(s.requests).toHaveLength(2);
    expect(h.transport.stats()).toMatchObject({ inFlight: 1, queued: 2, attempts: 2, phase: 'sleeping', sleepReason: 'retry-after' });

    // Only after that batch resolves is the fresh batch packed from the queue.
    await advance(60_000);
    await s.waitFor(4);
    expect(s.requests[2]?.ids).toEqual([first]);
    expect(s.requests[3]?.ids).toEqual(queued);
    await until(() => h.transport.stats().phase === 'idle', 'drained');
    expect(h.errors).toEqual([]);
  });

  it('B13: a flush with a batch queued resolves true after the send, and a flush of an empty client resolves true', async () => {
    const s = await stub();
    setupTransport(s.url);
    captureMessage('queued');
    const flushed = flush(2000);
    await s.waitFor(1); // no batchWaitMs wait: flush drains now
    await expect(flushed).resolves.toBe(true);
    await expect(flush(2000)).resolves.toBe(true);
  });

  it('flush(timeoutMs) with a non-finite or ≤ 0 timeout falls back to 2 000 with invalid_option once, and never rejects', async () => {
    const h = setupTransport(await refusedUrl());
    captureMessage('stuck');
    const flushed = flush(Number.NaN);
    await advance(1999);
    expect(h.errors.map((e) => [e.code, e.option])).toEqual([['invalid_option', 'timeoutMs']]);
    await advance(1);
    await expect(flushed).resolves.toBe(false);
  });
});

describe('B9: the async beforeSend forms', () => {
  it('B9: a rejected promise ≡ a throw — the event is sent unmodified and onError fires before_send_threw with the cause', async () => {
    const s = await stub();
    const boom = new Error('hook broke');
    const h = setupTransport(s.url, { beforeSend: () => Promise.reject(boom) });
    const id = captureMessage('kept');
    await settle(20); // the rejection settles on microtasks
    expect(await flush(2000)).toBe(true);
    expect(s.requests[0]?.ids).toEqual([id]);
    expect(s.requests[0]?.events[0]?.message).toBe('kept');
    expect(h.errors.map((e) => [e.code, e.cause])).toEqual([['before_send_threw', boom]]);
  });

  it("B9: a promise resolving undefined / false / 'x' ≡ null — dropped with a debug line, no onError, nothing posted", async () => {
    const s = await stub();
    const results: unknown[] = [undefined, false, 'x'];
    const h = setupTransport(s.url, { beforeSend: () => Promise.resolve(results.shift()) as Promise<SentinelEvent | null> });
    captureMessage('one');
    captureMessage('two');
    captureMessage('three');
    expect(await flush(2000)).toBe(true);
    expect(s.requests).toHaveLength(0);
    expect(h.errors).toEqual([]);
    expect(h.debugLines.filter((l) => /beforeSend returned (undefined|boolean|string).*dropped/.test(l))).toHaveLength(3);
  });

  it('B9: a hook that never settles holds its event; flush(500) resolves false at its timeout and never hangs', async () => {
    const s = await stub();
    setupTransport(s.url, { beforeSend: () => new Promise<SentinelEvent>(() => undefined) });
    captureMessage('held');
    const flushed = flush(500);
    await advance(499);
    await settle(5);
    let resolved: boolean | undefined;
    void flushed.then((v) => (resolved = v));
    await settle(5);
    expect(resolved).toBeUndefined();
    await advance(1);
    await expect(flushed).resolves.toBe(false);
    expect(s.requests).toHaveLength(0);
  });

  it('B9 (unit): a hook that resolves 100 ms after a second init enqueues into the NEW client and is posted under the new transport', async () => {
    const a = await stub();
    const b = await stub();
    setupTransport(a.url, {
      beforeSend: (event) => new Promise<SentinelEvent>((resolve) => setTimeout(() => resolve(event), 100)),
    });
    const id = captureMessage('late');
    await advance(50);
    setupTransport(b.url);
    expect(getInternalClient()?.queue).toEqual([]);
    await advance(50);
    expect(getInternalClient()?.queue.length).toBe(1);
    await advance(1000);
    await b.waitFor(1);
    expect(b.requests[0]?.ids).toEqual([id]);
    expect(a.requests).toHaveLength(0);
  });
});

describe('B22 (transport half): a second init with two events queued and one batch in flight', () => {
  it('B22: the queue drains into the new client with the same event_ids and release_version 2.34.0; the in-flight batch retries and reports under the OLD transport', async () => {
    const a = await stub((req) => (req.n === 1 ? 'hold' : { status: 429, body: 'Rate limit exceeded', headers: { 'retry-after': '60' } }));
    const b = await stub();
    const oldErrors: SentinelSdkError[] = [];
    const ha = setupTransport(a.url, { release: '2.34.0', onError: (e) => oldErrors.push(e) });
    const inFlight = captureMessage('in flight');
    await advance(1000);
    await a.waitFor(1); // held under A
    const queued = [captureMessage('queued 1'), captureMessage('queued 2')];
    expect(ha.transport.stats()).toMatchObject({ inFlight: 1, queued: 2 });

    const newErrors: SentinelSdkError[] = [];
    const hb = setupTransport(b.url, { release: '2.35.0', onError: (e) => newErrors.push(e) });
    expect(ha.transport.stats()).toMatchObject({ inFlight: 1, queued: 0 });
    expect(hb.transport.stats()).toMatchObject({ inFlight: 0, queued: 2 });
    expect(getClient()?.options.release).toBe('2.35.0');

    await advance(1000);
    await b.waitFor(1);
    expect(b.requests[0]?.ids).toEqual(queued);
    expect(b.requests[0]?.events.map((e) => e.release_version)).toEqual(['2.34.0', '2.34.0']);

    // The old transport's batch: 429 after the re-init, retried against A four times, dropped on A's onError.
    a.requests[0]?.answer({ status: 429, body: 'Rate limit exceeded', headers: { 'retry-after': '60' } });
    for (let n = 2; n <= 4; n++) {
      await until(() => ha.transport.stats().phase === 'sleeping', `A's wait ${n - 1}`);
      await advance(60_000);
      await a.waitFor(n);
    }
    await until(() => oldErrors.length === 1, "A's drop");
    expect(a.requests.map((r) => r.ids)).toEqual([[inFlight], [inFlight], [inFlight], [inFlight]]);
    expect(oldErrors[0]).toMatchObject({ code: 'batch_dropped', status: 429, attempts: 4, count: 1 });
    expect(newErrors).toEqual([]);
    expect(b.requests.flatMap((r) => r.ids)).not.toContain(inFlight);
    expect(a.requests.flatMap((r) => r.ids)).not.toContain(queued[0]);
  });
});

describe('B23: close()', () => {
  it('B23: close(300) with three events in flight against a stub that never answers resolves false, never rejects; the attempt is aborted, the count is in one debug line, no fetch and no onError after, getClient() undefined', async () => {
    const s = await stub(() => 'hold');
    const h = setupTransport(s.url);
    captureMessage('one');
    captureMessage('two');
    captureMessage('three');
    await advance(1000);
    await s.waitFor(1);
    expect(h.transport.stats()).toMatchObject({ inFlight: 3, phase: 'sending' });

    const closed = close(300);
    await advance(300);
    await expect(closed).resolves.toBe(false);
    expect(getClient()).toBeUndefined();
    expect(h.fetches[0]?.aborted).toBe(true);
    await until(() => s.requests[0]?.aborted === true, 'the stub seeing the abort');
    expect(h.debugLines.filter((l) => /close: 3 event\(s\) dropped/.test(l))).toHaveLength(1);

    const errorsAtClose = h.errors.length;
    const fetchesAtClose = h.fetches.length;
    await advance(180_000);
    expect(h.fetches).toHaveLength(fetchesAtClose);
    expect(s.requests).toHaveLength(1);
    expect(h.errors).toHaveLength(errorsAtClose);
    expect(h.errors.filter((e) => e.code === 'batch_dropped')).toHaveLength(0);
    // Captures after close: a fresh id, nothing queued.
    expect(captureMessage('after')).toMatch(/^[0-9a-f-]{36}$/);
    expect(getClient()).toBeUndefined();
  });

  it('B23: close cancels a backoff sleep and a Retry-After wait — no fetch after it resolves', async () => {
    const s = await stub(() => ({ status: 429, body: 'Rate limit exceeded', headers: { 'retry-after': '60' } }));
    const h = setupTransport(s.url);
    captureMessage('parked');
    await advance(1000);
    await s.waitFor(1);
    await until(() => h.transport.stats().phase === 'sleeping', 'the Retry-After wait');

    // flush inside close interrupts the wait (attempt 2), the second 429 parks it again, close's timeout hits.
    const closed = close(300);
    await s.waitFor(2);
    await until(() => h.transport.stats().phase === 'sleeping', 'the second wait');
    await advance(300);
    await expect(closed).resolves.toBe(false);
    expect(h.debugLines.filter((l) => /close: 1 event\(s\) dropped/.test(l))).toHaveLength(1);
    await advance(180_000);
    expect(s.requests).toHaveLength(2);
    expect(h.errors).toEqual([]);
  });

  it('B23: close resolves true when flush emptied the queue; a later init is a first init that keeps the module-level scope; close before any init resolves true', async () => {
    await expect(close(100)).resolves.toBe(true);
    const s = await stub();
    setUser({ id: 'usr_1' });
    setupTransport(s.url);
    captureMessage('sent');
    const closed = close(2000);
    await s.waitFor(1);
    await expect(closed).resolves.toBe(true);
    expect(getClient()).toBeUndefined();

    const h2 = setupTransport(s.url);
    expect(h2.transport.stats()).toMatchObject({ queued: 0, inFlight: 0 });
    captureMessage('after re-init');
    await advance(1000);
    await s.waitFor(2);
    expect(s.requests[1]?.events[0]?.user?.id).toBe('usr_1');
  });

  it('B23: close counts pending beforeSend hooks and their events never enqueue into a client', async () => {
    const s = await stub();
    const h = setupTransport(s.url, { beforeSend: () => new Promise<SentinelEvent>(() => undefined) });
    captureMessage('held');
    const closed = close(100);
    await advance(100);
    await expect(closed).resolves.toBe(false);
    expect(h.debugLines.filter((l) => /close: 1 event\(s\) dropped/.test(l))).toHaveLength(1);
  });
});

describe('B24: one event whose serialised size is ≈ 120 KiB', () => {
  it('B24: sent as its own batch without keepalive, the small events around it with keepalive; never dropped for size', async () => {
    const s = await stub();
    const h = setupTransport(s.url, {}, 'browser');
    const before = captureMessage('before');
    const err = new Error('m'.repeat(12_000));
    const path = 'https://app.example/_next/static/chunks/' + 'a'.repeat(480) + '.js';
    err.stack = `Error: ${err.message}\n` + Array.from({ length: 100 }, (_, i) => `    at fn${i} (${path}:${i + 1}:1)`).join('\n');
    const huge = captureException(err, { metadata: { blob: 'x'.repeat(60 * 1024) } });
    const after = captureMessage('after');
    // Three events under batchSize: each batch goes on the batchWaitMs cadence, one per tick.
    for (let n = 1; n <= 3; n++) {
      await advance(1000);
      await s.waitFor(n);
    }
    expect(s.requests.map((r) => r.ids)).toEqual([[before], [huge], [after]]);
    const hugeBytes = Buffer.byteLength(s.requests[1]?.body ?? '');
    expect(hugeBytes).toBeGreaterThan(100 * 1024);
    expect(h.fetches.map((f) => f.keepalive)).toEqual([true, false, true]);
    expect(h.fetches.map((f) => f.credentials)).toEqual(['omit', 'omit', 'omit']);
    expect(h.errors).toEqual([]);
    expect(h.debugLines.some((l) => l.includes('over the 60 KiB keepalive cap: sent alone, without keepalive'))).toBe(true);
  });
});

describe('N2 (unit half): the edge build drains on the next microtask', () => {
  it('N2: two captures in the same tick go out in one two-element POST, a third after an await in a second POST; batchWaitMs is ignored with a debug note', async () => {
    const s = await stub();
    const h = setupTransport(s.url, { batchWaitMs: 5000 }, 'edge');
    expect(getClient()?.options.batchWaitMs).toBe(0);
    expect(h.debugLines.some((l) => l.includes('batchWaitMs is ignored on the edge build'))).toBe(true);
    const a = captureMessage('a');
    const b = captureMessage('b');
    await s.waitFor(1); // no clock advance: the microtask drain
    const c = captureMessage('c');
    await s.waitFor(2);
    expect(s.requests.map((r) => r.ids)).toEqual([[a, b], [c]]);
    expect(h.fetches.map((f) => f.keepalive)).toEqual([undefined, undefined]);
  });

  it('N2: batchSize is honoured on the edge — 12 same-tick captures are a POST of 10 then one of 2', async () => {
    const s = await stub();
    setupTransport(s.url, {}, 'edge');
    const ids = Array.from({ length: 12 }, (_, i) => captureMessage(`m${i}`));
    await s.waitFor(2);
    expect(s.requests.map((r) => r.ids)).toEqual([ids.slice(0, 10), ids.slice(10)]);
  });
});

describe('the second-init drain of a client without a transport, and getClient()', () => {
  it('a client that resolved no target keeps a plain queue; a second init with a target sends what it held with the same event_ids', async () => {
    const s = await stub();
    initCore({ debug: false }, 'node');
    const id = captureMessage('held in a plain queue');
    expect(getInternalClient()?.transport).toBeUndefined();
    expect(getInternalClient()?.queue).toHaveLength(1);
    setupTransport(s.url);
    await advance(1000);
    await s.waitFor(1);
    expect(s.requests[0]?.ids).toEqual([id]);
    expect(getClient()?.options).toMatchObject({ maxBufferSize: 100, batchSize: 10, batchWaitMs: 1000, endpoint: s.url });
    expect(getClient()?.options).not.toHaveProperty('apiKey');
  });
});
