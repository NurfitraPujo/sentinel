// The page-hide send of contract §4.4 *Page hide* (B14, unit, fake timers): best-effort but non-destructive —
// the backing-off batch first as its own keepalive POST, then the head ≤ 60 KiB of the queue, each next batch
// only after the previous resolved; events leave the buffer only on a 2xx / non-retryable 4xx; a batch parked
// on Retry-After is not touched, an in-flight fetch is left alone, a batch at four attempts is never sent a
// fifth time, and the worker is not gated on visibility. Browser runtime against the node:http stub; the fetch
// spy sees `keepalive`. Shuffle-safe: every test starts from a deleted globalThis slot and its own stub.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { captureMessage } from '../../src/core/capture.js';
import { close, flush } from '../../src/core/client.js';
import { MAX_BATCH_BYTES } from '../../src/core/queue.js';
import { resetPipeline } from '../helpers/pipeline.js';
import { ACCEPTED, settle, startStub, type StubServer, until } from '../helpers/stub-server.js';
import { advance, lastAnnouncedDelay, setupTransport, stopTransport, useFakeClock } from '../helpers/transport.js';

let stubs: StubServer[] = [];

vi.setConfig({ testTimeout: 30_000 });

async function stub(...args: Parameters<typeof startStub>): Promise<StubServer> {
  const s = await startStub(...args);
  stubs.push(s);
  return s;
}

const UNAVAILABLE = { status: 503, body: 'Service Unavailable' };

beforeEach(() => {
  resetPipeline();
  useFakeClock();
});

afterEach(async () => {
  stopTransport();
  vi.useRealTimers();
  delete (globalThis as { document?: unknown }).document;
  delete (globalThis as { window?: unknown }).window;
  for (const s of stubs) await s.close();
  stubs = [];
});

/** One batch mid-backoff (its first attempt answered 503) and 25 events ≈ 150 KiB queued behind it. */
async function midBackoffWithQueue(s: StubServer) {
  const h = setupTransport(s.url, {}, 'browser');
  const first = captureMessage('first');
  await advance(1000);
  await s.waitFor(1);
  await until(() => h.transport.stats().phase === 'sleeping', 'the backoff');
  expect(h.transport.stats().sleepReason).toBe('backoff');
  const queued = Array.from({ length: 25 }, (_, i) => captureMessage(`queued ${i}`, { metadata: { blob: 'x'.repeat(6 * 1024) } }));
  expect(h.transport.stats()).toMatchObject({ inFlight: 1, queued: 25 });
  const bodyBytes = queued.length * 6 * 1024;
  expect(bodyBytes).toBeGreaterThan(140 * 1024);
  return { h, first, queued };
}

describe('B14 (unit): visibilitychange → hidden with 25 events ≈ 150 KiB queued and one batch mid-backoff', () => {
  it('B14 (a): the route answers 202 — the backing-off batch first, alone, as a keepalive POST (attempt 2), then ≤ 60 KiB batches one after another; the sent events leave the buffer', async () => {
    const s = await stub((req) => (req.n === 1 ? UNAVAILABLE : ACCEPTED(req)));
    const { h, first, queued } = await midBackoffWithQueue(s);

    await h.transport.hideSend();

    expect(s.requests[1]?.ids).toEqual([first]);
    expect(h.debugLines.some((l) => l.includes('attempt 2/4 (hide, keepalive)'))).toBe(true);
    const rest = s.requests.slice(2);
    expect(rest.flatMap((r) => r.ids)).toEqual(queued);
    expect(rest.length).toBeGreaterThan(1);
    for (const r of rest) expect(Buffer.byteLength(r.body)).toBeLessThanOrEqual(MAX_BATCH_BYTES);
    expect(h.fetches.slice(1).every((f) => f.keepalive === true)).toBe(true);
    expect(h.maxInFlight()).toBe(1);
    expect(h.transport.stats()).toMatchObject({ queued: 0, inFlight: 0, phase: 'idle' });
    expect(h.errors).toEqual([]);
  });

  it('B14 (b): the route never answers — the keepalive times out, every event is still buffered, and the worker sends them all later', async () => {
    const s = await stub((req) => (req.n === 1 ? UNAVAILABLE : 'hold'));
    const { h, first, queued } = await midBackoffWithQueue(s);

    const hiding = h.transport.hideSend();
    await s.waitFor(2);
    expect(s.requests[1]?.ids).toEqual([first]);
    await advance(5000); // the per-attempt timeout
    await hiding;
    expect(h.fetches[1]?.aborted).toBe(true);
    expect(s.requests).toHaveLength(2);
    expect(h.transport.stats()).toMatchObject({ inFlight: 1, queued: 25, phase: 'sleeping', attempts: 2 });

    s.respond(ACCEPTED);
    await advance(lastAnnouncedDelay(h.debugLines));
    await s.waitFor(3);
    for (let i = 0; i < 6 && h.transport.stats().queued + h.transport.stats().inFlight > 0; i++) await advance(1000);
    await until(() => h.transport.stats().phase === 'idle' && h.transport.stats().queued === 0, 'the worker draining everything');
    expect(s.requests[2]?.ids).toEqual([first]);
    expect(s.requests.slice(3).flatMap((r) => r.ids)).toEqual(queued);
    expect(h.errors).toEqual([]);
  });

  it('B14 (c): the connection is refused — every event is still buffered, and the worker sends them all once the route is back', async () => {
    const s = await stub((req) => (req.n === 1 ? UNAVAILABLE : ACCEPTED(req)));
    const port = Number(new URL(s.url).port);
    const { h, first, queued } = await midBackoffWithQueue(s);
    await s.close();

    await h.transport.hideSend();
    expect(h.fetches).toHaveLength(2);
    expect(h.fetches[1]?.outcome).toBe('error');
    expect(h.transport.stats()).toMatchObject({ inFlight: 1, queued: 25, phase: 'sleeping', attempts: 2 });

    const back = await stub(ACCEPTED, port);
    await advance(lastAnnouncedDelay(h.debugLines));
    await back.waitFor(1);
    for (let i = 0; i < 6 && h.transport.stats().queued + h.transport.stats().inFlight > 0; i++) await advance(1000);
    await until(() => h.transport.stats().phase === 'idle' && h.transport.stats().queued === 0, 'the worker draining everything');
    expect(back.requests[0]?.ids).toEqual([first]);
    expect(back.requests.slice(1).flatMap((r) => r.ids)).toEqual(queued);
    expect(h.errors).toEqual([]);
  });
});

describe('B14 (unit): an in-flight fetch and a Retry-After wait', () => {
  it('B14: the in-flight batch is left alone and the hide-send starts after it resolves; a batch then waiting on Retry-After: 30 is not hidden-sent — only flush overrides it', async () => {
    const s = await stub((req) => (req.n === 1 ? 'hold' : ACCEPTED(req)));
    const h = setupTransport(s.url, {}, 'browser');
    const first = captureMessage('first');
    await advance(1000);
    await s.waitFor(1);
    const queued = [captureMessage('queued 1'), captureMessage('queued 2')];

    const hiding = h.transport.hideSend();
    await settle(50);
    expect(s.requests).toHaveLength(1); // nothing while the fetch is in flight
    expect(h.debugLines.some((l) => l.includes('a fetch is in flight; starting after it resolves'))).toBe(true);

    s.requests[0]?.answer({ status: 429, body: 'Rate limit exceeded', headers: { 'retry-after': '30' } });
    await hiding;
    expect(h.transport.stats()).toMatchObject({ inFlight: 1, queued: 2, phase: 'sleeping', sleepReason: 'retry-after', attempts: 1 });
    expect(s.requests).toHaveLength(1);
    expect(h.debugLines.some((l) => l.includes('waiting on Retry-After; not re-sent'))).toBe(true);

    // A second hide while it waits changes nothing; the server's 30 s is honoured.
    await h.transport.hideSend();
    expect(s.requests).toHaveLength(1);
    await advance(29_000);
    expect(s.requests).toHaveLength(1);

    // flush is the one override: attempt 2 at once, then the queue.
    const flushed = flush(2000);
    await s.waitFor(3);
    await expect(flushed).resolves.toBe(true);
    expect(s.requests[1]?.ids).toEqual([first]);
    expect(s.requests[2]?.ids).toEqual(queued);
  });

  it('B14: a hide-send of a mid-backoff batch counts as its next attempt; a batch at 4 attempts is dropped, never hidden-sent a fifth time', async () => {
    const s = await stub(() => UNAVAILABLE);
    const h = setupTransport(s.url, {}, 'browser');
    const id = captureMessage('doomed');
    await advance(1000);
    await s.waitFor(1);
    for (let attempt = 2; attempt <= 4; attempt++) {
      await until(() => h.transport.stats().phase !== 'sending', `attempt ${attempt - 1} classified`);
      expect(h.transport.stats()).toMatchObject({ phase: 'sleeping', attempts: attempt - 1 });
      await h.transport.hideSend();
      expect(s.requests).toHaveLength(attempt);
      expect(s.requests[attempt - 1]?.ids).toEqual([id]);
    }
    await until(() => h.errors.length === 1, 'the drop');
    expect(h.errors[0]).toMatchObject({ code: 'batch_dropped', status: 503, attempts: 4, count: 1 });
    expect(h.transport.stats()).toMatchObject({ phase: 'idle', inFlight: 0 });

    await h.transport.hideSend();
    await advance(120_000);
    expect(s.requests).toHaveLength(4);
    expect(h.fetches.slice(1).map((f) => f.keepalive)).toEqual([true, true, true]);
  });
});

describe('B14 (unit, fake timers): the worker is not gated on visibility', () => {
  it('B14: with document.visibilityState stubbed hidden for 90 s while a batch is mid-backoff, the retry timer still fires and the batch is re-sent', async () => {
    const s = await stub((req) => (req.n === 1 ? UNAVAILABLE : ACCEPTED(req)));
    const doc = Object.assign(new EventTarget(), { visibilityState: 'hidden' });
    (globalThis as { document?: unknown }).document = doc;
    const h = setupTransport(s.url, {}, 'browser');
    const id = captureMessage('hidden tab');
    await advance(1000);
    await s.waitFor(1);
    await until(() => h.transport.stats().phase === 'sleeping', 'the backoff');
    expect(doc.visibilityState).toBe('hidden');
    await advance(90_000);
    await s.waitFor(2);
    await until(() => h.transport.stats().phase === 'idle', 'accepted');
    expect(s.requests.map((r) => r.ids)).toEqual([[id], [id]]);
    expect(h.debugLines.some((l) => l.includes('attempt 2/4 (worker'))).toBe(true);
    expect(h.errors).toEqual([]);
  });

  it('flushOnHide: visibilitychange → hidden and pagehide trigger the hide-send through the installed listeners; close removes them', async () => {
    const s = await stub();
    const doc = Object.assign(new EventTarget(), { visibilityState: 'visible' });
    const win = new EventTarget();
    (globalThis as { document?: unknown }).document = doc;
    (globalThis as { window?: unknown }).window = win;
    const h = setupTransport(s.url, {}, 'browser');

    const a = captureMessage('a');
    doc.visibilityState = 'hidden';
    doc.dispatchEvent(new Event('visibilitychange'));
    await s.waitFor(1); // no clock advance: sent at once
    expect(s.requests[0]?.ids).toEqual([a]);
    expect(h.fetches[0]?.keepalive).toBe(true);
    // waitFor(1) returns when the STUB has the request, not when the client has its response: until then the
    // hide-send's loop is still running, and an event captured now is the queue head it sends next (as it
    // should — contract §4.4). Wait for the client to settle the batch, so `b` is captured after the hide-send.
    await until(() => h.transport.stats().phase === 'idle', 'the hide-send resolved');

    doc.visibilityState = 'visible';
    const b = captureMessage('b');
    doc.dispatchEvent(new Event('visibilitychange')); // visible: nothing
    await settle(30);
    expect(s.requests).toHaveLength(1);
    win.dispatchEvent(new Event('pagehide'));
    await s.waitFor(2);
    expect(s.requests[1]?.ids).toEqual([b]);

    await expect(close(1000)).resolves.toBe(true);
    captureMessage('after close'); // dropped
    doc.visibilityState = 'hidden';
    doc.dispatchEvent(new Event('visibilitychange'));
    win.dispatchEvent(new Event('pagehide'));
    await settle(30);
    expect(s.requests).toHaveLength(2);
  });

  it('flushOnHide: false installs no listener', async () => {
    const s = await stub();
    const doc = Object.assign(new EventTarget(), { visibilityState: 'visible' });
    const spy = vi.spyOn(doc, 'addEventListener');
    (globalThis as { document?: unknown }).document = doc;
    setupTransport(s.url, { flushOnHide: false }, 'browser');
    expect(spy).not.toHaveBeenCalled();
  });
});
