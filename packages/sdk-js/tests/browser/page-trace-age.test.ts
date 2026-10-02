// @vitest-environment jsdom
//
// pageTraceMaxAgeMs (spec *Trace propagation*): a page trace older than the limit is replaced lazily, at read time,
// by a propagated request and by a captured event alike — the same new id for both.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { captureMessage, close, init } from '../../src/browser.js';
import { getInternalClient } from '../../src/core/state.js';
import type { SentinelEvent } from '../../src/core/types.js';
import { resetPipeline } from '../helpers/pipeline.js';

const TP = /^00-([0-9a-f]{32})-([0-9a-f]{16})-01$/;
const MIN = 60 * 1000;
let sent: string[] = [];

function queued(): SentinelEvent[] {
  return (getInternalClient()?.queue ?? []).map((j) => JSON.parse(j) as SentinelEvent);
}
async function fetchTrace(): Promise<string> {
  await fetch('/api/x');
  return TP.exec(sent[sent.length - 1] ?? '')?.[1] ?? 'none';
}
function eventTrace(): string | undefined {
  captureMessage('m');
  return queued().at(-1)?.trace_id;
}

beforeEach(() => {
  vi.useFakeTimers();
  resetPipeline();
  sent = [];
  history.replaceState({}, '', '/start');
  (globalThis as { fetch?: unknown }).fetch = (input: unknown, o?: { headers?: unknown }) => {
    const h = o?.headers ?? (input as { headers?: unknown }).headers;
    sent.push(h instanceof Headers ? (h.get('traceparent') ?? '') : ((h as Record<string, string> | undefined)?.traceparent ?? ''));
    return Promise.resolve(new Response('ok'));
  };
});
afterEach(async () => {
  await close(10);
  resetPipeline();
  vi.useRealTimers();
});

describe('pageTraceMaxAgeMs', () => {
  it('older than the limit: the next fetch and the next event carry the same NEW trace id', async () => {
    init({ tunnel: '/api/sentinel', pageTraceMaxAgeMs: 30 * MIN });
    const first = await fetchTrace();
    vi.advanceTimersByTime(30 * MIN);
    const second = await fetchTrace();
    expect(second).not.toBe(first);
    expect(eventTrace()).toBe(second);
  });

  it('an event is the first reader after expiry; a following fetch agrees with it', async () => {
    init({ tunnel: '/api/sentinel', pageTraceMaxAgeMs: 30 * MIN });
    const first = await fetchTrace();
    vi.advanceTimersByTime(31 * MIN);
    const ev = eventTrace();
    expect(ev).not.toBe(first);
    expect(await fetchTrace()).toBe(ev);
  });

  it('younger than the limit: same id; default is 30 minutes', async () => {
    init({ tunnel: '/api/sentinel' });
    const first = await fetchTrace();
    vi.advanceTimersByTime(30 * MIN - 1);
    expect(await fetchTrace()).toBe(first);
    expect(eventTrace()).toBe(first);
    vi.advanceTimersByTime(1);
    expect(await fetchTrace()).not.toBe(first);
  });

  it('age is measured from the renewal, not the page load', async () => {
    init({ tunnel: '/api/sentinel', pageTraceMaxAgeMs: 10 * MIN });
    const first = await fetchTrace();
    vi.advanceTimersByTime(11 * MIN);
    const second = await fetchTrace();
    expect(second).not.toBe(first);
    vi.advanceTimersByTime(9 * MIN);
    expect(await fetchTrace()).toBe(second);
  });

  it('0 never renews by age', async () => {
    const errors: string[] = [];
    init({ tunnel: '/api/sentinel', pageTraceMaxAgeMs: 0, onError: (e) => errors.push(e.code) });
    const first = await fetchTrace();
    vi.advanceTimersByTime(48 * 60 * MIN);
    expect(await fetchTrace()).toBe(first);
    expect(eventTrace()).toBe(first);
    expect(errors).toEqual([]);
  });

  it('Infinity never renews by age, without a report', async () => {
    const errors: string[] = [];
    init({ tunnel: '/api/sentinel', pageTraceMaxAgeMs: Infinity, onError: (e) => errors.push(e.code) });
    const first = await fetchTrace();
    vi.advanceTimersByTime(48 * 60 * MIN);
    expect(await fetchTrace()).toBe(first);
    expect(errors).toEqual([]);
  });

  it('a wall clock stepped backwards renews instead of freezing', async () => {
    init({ tunnel: '/api/sentinel', pageTraceMaxAgeMs: 30 * MIN });
    const first = await fetchTrace();
    vi.setSystemTime(Date.now() - 2 * 60 * MIN);
    const second = await fetchTrace();
    expect(second).not.toBe(first);
    expect(eventTrace()).toBe(second);
    vi.advanceTimersByTime(29 * MIN);
    expect(await fetchTrace()).toBe(second);
  });

  it('a throwing clock at init does not stop the trace installing; it just never ages', async () => {
    const spy = vi.spyOn(Date, 'now').mockImplementation(() => {
      throw new Error('no clock');
    });
    try {
      init({ tunnel: '/api/sentinel' });
    } finally {
      spy.mockRestore();
    }
    const first = await fetchTrace();
    expect(first).toMatch(/^[0-9a-f]{32}$/);
    vi.advanceTimersByTime(48 * 60 * MIN);
    expect(await fetchTrace()).toBe(first);
    expect(eventTrace()).toBe(first);
  });

  it('renews through XHR as well', () => {
    const sentX: string[] = [];
    class X {
      h: Record<string, string> = {};
      open(): void {}
      setRequestHeader(n: string, v: string): void {
        this.h[n.toLowerCase()] = v;
      }
      send(): void {
        sentX.push(this.h.traceparent ?? '');
      }
      addEventListener(): void {}
      removeEventListener(): void {}
    }
    (globalThis as { XMLHttpRequest?: unknown }).XMLHttpRequest = X as unknown as typeof XMLHttpRequest;
    init({ tunnel: '/api/sentinel', pageTraceMaxAgeMs: 30 * MIN });
    const call = (): string => {
      const x = new XMLHttpRequest();
      x.open('GET', '/api/x');
      x.send();
      return TP.exec(sentX[sentX.length - 1] ?? '')?.[1] ?? 'none';
    };
    const first = call();
    expect(call()).toBe(first);
    vi.advanceTimersByTime(30 * MIN);
    const second = call();
    expect(second).not.toBe(first);
    expect(eventTrace()).toBe(second);
  });

  it('a pathname change renews and resets the age', async () => {
    init({ tunnel: '/api/sentinel', pageTraceMaxAgeMs: 30 * MIN });
    const first = await fetchTrace();
    vi.advanceTimersByTime(25 * MIN);
    history.pushState({}, '', '/other');
    const second = await fetchTrace();
    expect(second).not.toBe(first);
    vi.advanceTimersByTime(25 * MIN);
    expect(await fetchTrace()).toBe(second);
  });

  it.each([-1, Number.NaN, -Infinity, '30' as never, null as never])('rejects %s with invalid_option and uses the default', async (bad) => {
    const errors: string[] = [];
    init({ tunnel: '/api/sentinel', pageTraceMaxAgeMs: bad, onError: (e) => errors.push(`${e.code}:${e.option ?? ''}`) });
    expect(errors).toEqual(['invalid_option:pageTraceMaxAgeMs']);
    const first = await fetchTrace();
    vi.advanceTimersByTime(29 * MIN);
    expect(await fetchTrace()).toBe(first);
    vi.advanceTimersByTime(2 * MIN);
    expect(await fetchTrace()).not.toBe(first);
  });

  it('a positive value below 60 000 is clamped to 60 000', async () => {
    const errors: string[] = [];
    init({ tunnel: '/api/sentinel', pageTraceMaxAgeMs: 5, onError: (e) => errors.push(e.code) });
    expect(errors).toEqual(['invalid_option']);
    const first = await fetchTrace();
    vi.advanceTimersByTime(MIN - 1);
    expect(await fetchTrace()).toBe(first);
    vi.advanceTimersByTime(1);
    expect(await fetchTrace()).not.toBe(first);
  });
});
