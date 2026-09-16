// The retry matrix of contract §4.4 against the local node:http stub (contract §11 *The transport against a local
// HTTP stub*): B15 (overflow, the sequential drain), B16 (429 + Retry-After — the cap, the three degenerate forms,
// every send counting as an attempt), B17 (401), N9 (ingestor down, flush false at its timeout, growing delays,
// unref'ed timers), N14 (202 with errors[]), N17 (decision 33's 400 split). Delays run on vitest's fake clock and
// are asserted only as ≤ 60 s; I/O stays real. Shuffle-safe: every test starts from a deleted globalThis slot,
// its own stub and its own client.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { captureException, captureMessage } from '../../src/core/capture.js';
import { flush } from '../../src/core/client.js';
import { MAX_ATTEMPTS } from '../../src/core/transport/transport.js';
import { resetPipeline } from '../helpers/pipeline.js';
import { refusedUrl, startStub, type StubServer, until } from '../helpers/stub-server.js';
import { advance, announcedDelays, lastAnnouncedDelay, setupTransport, stopTransport, useFakeClock } from '../helpers/transport.js';

let stub: StubServer | undefined;

// Long fake-clock advances still cost real milliseconds per step; a test that times out keeps running and would
// advance the NEXT test's clock, so the budget is generous.
vi.setConfig({ testTimeout: 30_000 });

beforeEach(() => {
  resetPipeline();
  useFakeClock();
});

afterEach(async () => {
  stopTransport();
  vi.useRealTimers();
  await stub?.close();
  stub = undefined;
});

const RATE_LIMITED = (retryAfter: string) => () => ({ status: 429, body: 'Rate limit exceeded', headers: { 'retry-after': retryAfter } });

describe('B16: the route answers 429 + Retry-After: 60', () => {
  it('B16: retried after ≤ 60 s with the same event_ids, 4 attempts in total, then batch_dropped { status: 429, attempts: 4, count: 1 }', async () => {
    stub = await startStub(RATE_LIMITED('60'));
    const h = setupTransport(stub.url);
    const id = captureMessage('rate me');
    await advance(1000);
    await stub.waitFor(1);
    await until(() => h.transport.stats().phase === 'sleeping', 'the first backoff');
    expect(h.transport.stats()).toMatchObject({ inFlight: 1, attempts: 1, phase: 'sleeping', sleepReason: 'retry-after' });

    for (let attempt = 2; attempt <= MAX_ATTEMPTS; attempt++) {
      await advance(59_000);
      expect(stub.requests).toHaveLength(attempt - 1); // never before the server's 60 s
      await advance(1_000);
      await stub.waitFor(attempt);
      await until(() => h.transport.stats().phase !== 'sending', `attempt ${attempt} classified`);
    }
    expect(stub.requests.map((r) => r.ids)).toEqual([[id], [id], [id], [id]]);
    expect(stub.requests.map((r) => r.url)).toEqual(['/batch', '/batch', '/batch', '/batch']);
    expect(h.errors.map((e) => ({ code: e.code, status: e.status, attempts: e.attempts, count: e.count }))).toEqual([
      { code: 'batch_dropped', status: 429, attempts: 4, count: 1 },
    ]);
    expect(h.transport.stats()).toMatchObject({ queued: 0, inFlight: 0, phase: 'idle' });

    await advance(120_000);
    expect(stub.requests).toHaveLength(4); // never a fifth
  });

  it('B16: a flush() during the second wait sends at once as attempt 3 — the batch is still sent exactly four times', async () => {
    stub = await startStub(RATE_LIMITED('60'));
    const h = setupTransport(stub.url);
    const id = captureMessage('rate me');
    await advance(1000);
    await stub.waitFor(1);
    await until(() => h.transport.stats().phase === 'sleeping', 'the first wait');
    await advance(60_000);
    await stub.waitFor(2);
    await until(() => h.transport.stats().phase === 'sleeping', 'the second wait');

    const flushed = flush(2000);
    await stub.waitFor(3); // no clock advance: the Retry-After wait was interrupted
    expect(stub.requests[2]?.ids).toEqual([id]);
    await until(() => h.transport.stats().phase === 'sleeping', 'the third wait');
    expect(h.transport.stats().attempts).toBe(3);
    await advance(2000);
    expect(await flushed).toBe(false);
    expect(stub.requests).toHaveLength(3);

    await advance(60_000);
    await stub.waitFor(4);
    await until(() => h.transport.stats().phase === 'idle', 'the drop');
    expect(stub.requests.map((r) => r.ids)).toEqual([[id], [id], [id], [id]]);
    expect(h.errors.map((e) => [e.code, e.attempts])).toEqual([['batch_dropped', 4]]);
    await advance(120_000);
    expect(stub.requests).toHaveLength(4);
  });

  it("B16 (unit): Retry-After 'soon' / -5 / a past HTTP-date are ignored — the ladder's delay applies, one debug line each, never an immediate resend", async () => {
    const headers = ['soon', '-5', new Date(Date.now() - 3_600_000).toUTCString()];
    stub = await startStub((req) => RATE_LIMITED(headers[req.n - 1] ?? '60')());
    const h = setupTransport(stub.url);
    captureMessage('degenerate');
    await advance(1000);
    await stub.waitFor(1);
    for (let attempt = 2; attempt <= MAX_ATTEMPTS; attempt++) {
      await until(() => h.transport.stats().phase === 'sleeping', `wait ${attempt - 1}`);
      expect(h.transport.stats().sleepReason).toBe('backoff');
      await advance(0);
      expect(stub.requests).toHaveLength(attempt - 1); // never immediate
      const delays = announcedDelays(h.debugLines);
      const delay = delays[delays.length - 1] as number;
      // The ladder: 1 s × 2^(n-1) ± 20 %, never the header's 60 s.
      const base = 1000 * 2 ** (attempt - 2);
      expect(delay).toBeGreaterThanOrEqual(base * 0.8);
      expect(delay).toBeLessThanOrEqual(base * 1.2);
      await advance(delay);
      await stub.waitFor(attempt);
    }
    expect(h.debugLines.filter((l) => /Retry-After .* ignored/.test(l))).toHaveLength(3);
    expect(stub.requests).toHaveLength(4);
    await until(() => h.errors.length === 1, 'the drop');
    expect(h.errors[0]).toMatchObject({ code: 'batch_dropped', status: 429, attempts: 4 });
  });

  it('B16 (unit): Retry-After 120 and an HTTP-date an hour out are the over-cap case — capped at 60 s', async () => {
    const headers = ['120', new Date(Date.now() + 3_600_000).toUTCString()];
    stub = await startStub((req) => RATE_LIMITED(headers[req.n - 1] ?? '60')());
    const h = setupTransport(stub.url);
    captureMessage('over the cap');
    await advance(1000);
    await stub.waitFor(1);
    for (let attempt = 2; attempt <= 3; attempt++) {
      await until(() => h.transport.stats().phase === 'sleeping', `wait ${attempt - 1}`);
      expect(h.transport.stats().sleepReason).toBe('retry-after');
      await advance(59_000);
      expect(stub.requests, h.debugLines.join('\n')).toHaveLength(attempt - 1);
      await advance(1_000);
      await stub.waitFor(attempt);
    }
    expect(h.debugLines.filter((l) => /Retry-After .* ignored/.test(l))).toHaveLength(0);
    expect(announcedDelays(h.debugLines).slice(0, 2)).toEqual([60_000, 60_000]);
  });
});

describe('B17: the route answers 401', () => {
  it('B17: dropped at the first response — batch_dropped { status: 401, attempts: 1 }, one request, no console.error', async () => {
    stub = await startStub(() => ({ status: 401, body: 'Missing API key' }));
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const h = setupTransport(stub.url);
    captureMessage('bad key');
    await advance(1000);
    await stub.waitFor(1);
    await until(() => h.errors.length === 1, 'the drop');
    expect(h.errors[0]).toMatchObject({ code: 'batch_dropped', status: 401, attempts: 1, count: 1, message: 'Missing API key' });
    await advance(120_000);
    expect(stub.requests).toHaveLength(1);
    expect(consoleError).not.toHaveBeenCalled();
    expect(stub.requests[0]?.headers['x-api-key']).toBe('test-key');
  });
});

describe('B15: 150 captures in a tight loop with the route unreachable', () => {
  it('B15: 100 kept; buffer_overflow once with count: 50 on the next dequeue, then 90 queued + 10 in flight and no second batch while the first backs off', async () => {
    const h = setupTransport(await refusedUrl());
    const ids: string[] = [];
    for (let i = 0; i < 150; i++) ids.push(captureException(new Error(`e${i}`)));
    // Synchronous: nothing dequeued, nothing reported inside captureException.
    expect(h.transport.stats()).toMatchObject({ queued: 100, inFlight: 0 });
    expect(h.errors).toEqual([]);

    // The batch is full, so the worker's zero timer dequeues it — after the loop returned, never inside it.
    await advance(0);
    await until(() => h.fetches.length === 1, 'the first send');
    expect(h.errors.map((e) => [e.code, e.count])).toEqual([['buffer_overflow', 50]]);
    expect(h.transport.stats()).toMatchObject({ queued: 90, inFlight: 10 });
    // The batch is the oldest 10 survivors, the 50 oldest having been dropped.
    expect(h.fetches[0]?.ids).toEqual(ids.slice(50, 60));

    // Sequential: through every retry of the first batch, no second batch goes out.
    await until(() => h.transport.stats().phase === 'sleeping', 'the first backoff');
    await advance(10_000);
    await until(() => h.errors.some((e) => e.code === 'batch_dropped'), 'the first batch dropped');
    expect(h.fetches.slice(0, 4).every((f) => f.ids.join() === ids.slice(50, 60).join())).toBe(true);
    expect(h.maxInFlight()).toBe(1);

    // The next dequeue has no new drops: no second buffer_overflow.
    await until(() => h.fetches.length >= 5, 'the second batch');
    expect(h.fetches[4]?.ids).toEqual(ids.slice(60, 70));
    expect(h.errors.filter((e) => e.code === 'buffer_overflow')).toHaveLength(1);
  });
});

describe('N9: ingestor down; 20 captures; await flush(3000)', () => {
  it('N9: flush resolves false at its timeout and never rejects; batch_dropped after 4 attempts; the second batch waits; every delay ≤ 60 s and growing', async () => {
    const h = setupTransport(await refusedUrl());
    const ids: string[] = [];
    for (let i = 0; i < 20; i++) ids.push(captureMessage(`m${i}`));
    const flushed = flush(3000);
    await advance(3000);
    await expect(flushed).resolves.toBe(false);

    // The first batch only, so far — never the second while the first is in flight or backing off.
    expect(h.fetches.every((f) => f.ids.join() === ids.slice(0, 10).join())).toBe(true);
    expect(h.fetches.length).toBeLessThanOrEqual(MAX_ATTEMPTS);
    expect(h.maxInFlight()).toBe(1);

    await advance(10_000);
    await until(() => h.errors.filter((e) => e.code === 'batch_dropped').length >= 1, 'the first drop');
    expect(h.fetches.slice(0, 4).map((f) => f.ids)).toEqual([ids.slice(0, 10), ids.slice(0, 10), ids.slice(0, 10), ids.slice(0, 10)]);
    expect(h.errors[0]).toMatchObject({ code: 'batch_dropped', attempts: 4, count: 10 });
    expect(h.errors[0]?.status).toBeUndefined();

    await advance(10_000);
    await until(() => h.errors.filter((e) => e.code === 'batch_dropped').length === 2, 'the second drop');
    expect(h.fetches.slice(4, 8).every((f) => f.ids.join() === ids.slice(10, 20).join())).toBe(true);
    expect(h.fetches).toHaveLength(8);

    const delays = announcedDelays(h.debugLines);
    expect(delays.every((d) => d <= 60_000)).toBe(true);
    // The second batch's three delays, unflushed: 1 s, 2 s, 4 s ± 20 %, each larger than the last.
    const second = delays.slice(-3);
    expect(second).toHaveLength(3);
    expect(second[0]).toBeGreaterThanOrEqual(800);
    expect(second[1]).toBeGreaterThan(second[0] as number);
    expect(second[2]).toBeGreaterThan(second[1] as number);
    expect(h.transport.stats()).toMatchObject({ queued: 0, inFlight: 0, phase: 'idle' });
  });
});

describe('N14: a 202 whose body carries errors[] and event_ids[] with timestamps', () => {
  it('N14: item_rejected for index 3 with the message; nothing retried; event_ids[].timestamp ignored', async () => {
    stub = await startStub((req) => ({
      status: 202,
      body: JSON.stringify({
        ingested: 9,
        failed: 1,
        errors: [{ index: 3, message: 'validation failed: message exceeds 10000' }],
        event_ids: req.ids.map((event_id, index) => ({ index, event_id, timestamp: '2026-09-16T12:00:00Z' })),
      }),
    }));
    const h = setupTransport(stub.url);
    const ids: string[] = [];
    for (let i = 0; i < 10; i++) ids.push(captureMessage(`m${i}`));
    await advance(1000);
    await stub.waitFor(1);
    await until(() => h.transport.stats().phase === 'idle', 'the batch accepted');
    expect(h.errors).toHaveLength(1);
    expect(h.errors[0]).toMatchObject({ code: 'item_rejected', status: 202, message: '3: validation failed: message exceeds 10000' });
    expect(h.errors[0]?.cause).toEqual({ index: 3, event_id: ids[3] });
    await advance(120_000);
    expect(stub.requests).toHaveLength(1);
    expect(h.errors).toHaveLength(1);
  });
});

describe('N17: the 400 split (decision 33)', () => {
  const VALIDATION = JSON.stringify({
    ingested: 0,
    failed: 2,
    errors: [
      { index: 0, message: 'validation failed: message exceeds 10000' },
      { index: 1, message: 'validation failed: error_class is required' },
    ],
  });
  const NATS = JSON.stringify({
    ingested: 0,
    failed: 2,
    errors: [
      { index: 0, message: 'nats: no responders' },
      { index: 1, message: 'nats: no responders' },
    ],
  });

  it('N17 (a): a validation-failed 400 is dropped on the first response — batch_dropped once { status: 400, attempts: 1, count: 2, message } with both texts, no item_rejected, no retry', async () => {
    stub = await startStub(() => ({ status: 400, body: VALIDATION }));
    const h = setupTransport(stub.url);
    captureMessage('one');
    captureMessage('two');
    await advance(1000);
    await stub.waitFor(1);
    await until(() => h.errors.length === 1, 'the drop');
    expect(h.errors[0]).toMatchObject({
      code: 'batch_dropped',
      status: 400,
      attempts: 1,
      count: 2,
      message: '0: validation failed: message exceeds 10000\n1: validation failed: error_class is required',
    });
    await advance(120_000);
    expect(stub.requests).toHaveLength(1);
    expect(h.errors.filter((e) => e.code === 'item_rejected')).toHaveLength(0);
    expect(h.errors).toHaveLength(1);
  });

  it('N17 (b): a 400 without the prefix (a NATS outage) is retried with the same event_ids, 4 attempts in total, then batch_dropped { status: 400, attempts: 4 }', async () => {
    stub = await startStub(() => ({ status: 400, body: NATS }));
    const h = setupTransport(stub.url);
    const ids = [captureMessage('one'), captureMessage('two')];
    await advance(1000);
    await stub.waitFor(1);
    for (let attempt = 2; attempt <= MAX_ATTEMPTS; attempt++) {
      await until(() => h.transport.stats().phase === 'sleeping', `wait ${attempt - 1}`);
      expect(h.transport.stats().sleepReason).toBe('backoff');
      await advance(lastAnnouncedDelay(h.debugLines));
      await stub.waitFor(attempt);
    }
    await until(() => h.errors.length === 1, 'the drop');
    expect(stub.requests.map((r) => r.ids)).toEqual([ids, ids, ids, ids]);
    expect(h.errors[0]).toMatchObject({ code: 'batch_dropped', status: 400, attempts: 4, count: 2 });
    expect(h.errors[0]?.message).toContain('nats: no responders');
    await advance(120_000);
    expect(stub.requests).toHaveLength(4);
  });

  it("N17 (c): a plain-text 400 ('Invalid JSON payload array') is dropped as (a) with the text as message", async () => {
    stub = await startStub(() => ({ status: 400, body: 'Invalid JSON payload array', headers: { 'content-type': 'text/plain' } }));
    const h = setupTransport(stub.url);
    captureMessage('one');
    captureMessage('two');
    await advance(1000);
    await stub.waitFor(1);
    await until(() => h.errors.length === 1, 'the drop');
    expect(h.errors[0]).toMatchObject({ code: 'batch_dropped', status: 400, attempts: 1, count: 2, message: 'Invalid JSON payload array' });
    await advance(120_000);
    expect(stub.requests).toHaveLength(1);
  });

  it('N17 (d): one validation-failed message among two is enough — dropped as (a)', async () => {
    const MIXED = JSON.stringify({
      ingested: 0,
      failed: 2,
      errors: [
        { index: 0, message: 'nats: no responders' },
        { index: 1, message: 'validation failed: error_class is required' },
      ],
    });
    stub = await startStub(() => ({ status: 400, body: MIXED }));
    const h = setupTransport(stub.url);
    captureMessage('one');
    captureMessage('two');
    await advance(1000);
    await stub.waitFor(1);
    await until(() => h.errors.length === 1, 'the drop');
    expect(h.errors[0]).toMatchObject({
      code: 'batch_dropped',
      status: 400,
      attempts: 1,
      count: 2,
      message: '0: nats: no responders\n1: validation failed: error_class is required',
    });
    await advance(120_000);
    expect(stub.requests).toHaveLength(1);
    expect(h.errors).toHaveLength(1);
  });
});

describe('the rest of the retry matrix (contract §11): 5xx, 413, timeout', () => {
  it('500 then 503 then 202: retried under the ladder with the same event_ids and accepted on the third attempt', async () => {
    const statuses = [500, 503, 202];
    stub = await startStub((req) => {
      const status = statuses[req.n - 1] ?? 202;
      return status === 202 ? { status, body: JSON.stringify({ ingested: req.events.length, failed: 0 }) } : { status, body: 'Failed to ingest error' };
    });
    const h = setupTransport(stub.url);
    const id = captureMessage('flaky');
    await advance(1000);
    await stub.waitFor(1);
    await until(() => h.transport.stats().phase === 'sleeping', 'wait 1');
    await advance(lastAnnouncedDelay(h.debugLines));
    await stub.waitFor(2);
    await until(() => h.transport.stats().phase === 'sleeping', 'wait 2');
    await advance(lastAnnouncedDelay(h.debugLines));
    await stub.waitFor(3);
    await until(() => h.transport.stats().phase === 'idle', 'accepted');
    expect(stub.requests.map((r) => r.ids)).toEqual([[id], [id], [id]]);
    expect(h.errors).toEqual([]);
  });

  it('413 is dropped on the first response like every other 4xx', async () => {
    stub = await startStub(() => ({ status: 413, body: 'Request body too large' }));
    const h = setupTransport(stub.url);
    captureMessage('big');
    await advance(1000);
    await stub.waitFor(1);
    await until(() => h.errors.length === 1, 'the drop');
    expect(h.errors[0]).toMatchObject({ code: 'batch_dropped', status: 413, attempts: 1 });
    await advance(120_000);
    expect(stub.requests).toHaveLength(1);
  });

  it('a request that is never answered is aborted after the per-attempt timeout and retried with the same event_ids', async () => {
    stub = await startStub((req) => (req.n === 1 ? 'hold' : { status: 202, body: '{"ingested":1,"failed":0}' }));
    const h = setupTransport(stub.url);
    const id = captureMessage('slow');
    await advance(1000);
    await stub.waitFor(1);
    await advance(5000);
    await until(() => stub?.requests[0]?.aborted === true, 'the stub seeing the abort');
    expect(h.fetches[0]?.aborted).toBe(true);
    await until(() => h.transport.stats().phase === 'sleeping', 'the backoff');
    await advance(lastAnnouncedDelay(h.debugLines));
    await stub.waitFor(2);
    await until(() => h.transport.stats().phase === 'idle', 'accepted');
    expect(stub.requests.map((r) => r.ids)).toEqual([[id], [id]]);
    expect(h.errors).toEqual([]);
  });
});
