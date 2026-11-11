// The tunnel route's token bucket (contract §4.5 step 4, decision 19; flows T14, T16, T17): 60/min per client by
// default, capacity 60 refilling one per second, `Retry-After: ceil(seconds to the next token)`; the key from
// `clientKey` when given (`null` → the shared bucket, a throw ≡ `null`) else the client address the forward also
// writes — `X-Forwarded-For`'s entry at `len − trustedProxyHops`, the RIGHTMOST by default, never the leftmost the
// browser writes (sentinel-event-enrichment spec §4.2-§4.3); step 4 sits after the origin check so a cross-site flood spends no token; the numeric options fall back
// with one `warn` each and the factory never throws; and the store is bounded — ≤ 10 000 keys, the least
// recently seen evicted, a bucket full for ≥ 60 s swept. Fake timers drive the clock; `fetch` is stubbed.
// Shuffle-safe: every test builds its own handler and restores the clock.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FULL_IDLE_MS, MAX_KEYS, SHARED_KEY, storeOf, TokenBucketStore } from '../../src/nextjs/rate-limit.js';
import { createSentinelRouteHandler } from '../../src/nextjs/route.js';
import { ENDPOINT, envelope, freshRoute, ITEM, KEY, request, stubUpstream, type Upstream } from './helpers.js';

let upstream: Upstream;

beforeEach(() => {
  vi.useFakeTimers();
  upstream = stubUpstream();
  vi.stubEnv('SENTINEL_ENDPOINT', ENDPOINT);
  vi.stubEnv('SENTINEL_API_KEY', KEY);
});

afterEach(() => {
  vi.unstubAllEnvs();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

/** `n` same-origin requests from `hop` (or with the given headers), their statuses in order. */
async function burst(POST: (req: Request) => Promise<Response>, n: number, headers: Record<string, string | null>): Promise<number[]> {
  const out: number[] = [];
  for (let i = 0; i < n; i++) out.push((await POST(request({ headers }))).status);
  return out;
}

describe('T14: the default rateLimit { perMinute: 60 } (decision 19)', () => {
  it('T14: 61 requests from one X-Forwarded-For first hop within a second — the 61st is 429 rate_limited with Retry-After: 1, never 60', async () => {
    const { POST } = createSentinelRouteHandler();
    const statuses = await burst(POST, 61, { 'x-forwarded-for': '5.5.5.5' });
    expect(statuses.slice(0, 60).every((s) => s === 202)).toBe(true);
    expect(statuses[60]).toBe(429);
    const res = await POST(request({ headers: { 'x-forwarded-for': '5.5.5.5' } }));
    expect(res.status).toBe(429);
    expect(res.headers.get('retry-after')).toBe('1');
    const env = await envelope(res);
    expect(env.error.code).toBe('rate_limited');
    expect(upstream.calls).toHaveLength(60);
  });

  it('T14: one second later one more passes (refill one token per second), and the next after it is 429 again', async () => {
    const { POST } = createSentinelRouteHandler();
    await burst(POST, 60, { 'x-forwarded-for': '5.5.5.5' });
    expect((await POST(request({ headers: { 'x-forwarded-for': '5.5.5.5' } }))).status).toBe(429);
    await vi.advanceTimersByTimeAsync(1000);
    expect((await POST(request({ headers: { 'x-forwarded-for': '5.5.5.5' } }))).status).toBe(202);
    expect((await POST(request({ headers: { 'x-forwarded-for': '5.5.5.5' } }))).status).toBe(429);
  });

  it('T14: a half-refilled token waits ceil — 500 ms into the second the answer is still Retry-After: 1', async () => {
    const { POST } = createSentinelRouteHandler();
    await burst(POST, 60, { 'x-forwarded-for': '5.5.5.5' });
    await vi.advanceTimersByTimeAsync(500);
    const res = await POST(request({ headers: { 'x-forwarded-for': '5.5.5.5' } }));
    expect(res.status).toBe(429);
    expect(res.headers.get('retry-after')).toBe('1');
  });

  it('T14: per instance — a second handler has its own bucket for the same hop; a different hop has its own bucket on the same handler', async () => {
    const a = createSentinelRouteHandler();
    const b = createSentinelRouteHandler();
    await burst(a.POST, 60, { 'x-forwarded-for': '5.5.5.5' });
    expect((await a.POST(request({ headers: { 'x-forwarded-for': '5.5.5.5' } }))).status).toBe(429);
    expect((await b.POST(request({ headers: { 'x-forwarded-for': '5.5.5.5' } }))).status).toBe(202);
    expect((await a.POST(request({ headers: { 'x-forwarded-for': '6.6.6.6' } }))).status).toBe(202);
  });

  it.each([
    ['rateLimit: false', { rateLimit: false as const }],
    ['{ perMinute: 0 }', { rateLimit: { perMinute: 0 } }],
    ['{ perMinute: NaN }', { rateLimit: { perMinute: NaN } }],
    ['{ perMinute: -1 }', { rateLimit: { perMinute: -1 } }],
    ['{ perMinute: Infinity }', { rateLimit: { perMinute: Infinity } }],
  ])('T14: %s lets the 61st through (≡ false)', async (_name, options) => {
    const { POST } = createSentinelRouteHandler(options);
    const statuses = await burst(POST, 61, { 'x-forwarded-for': '5.5.5.5' });
    expect(statuses.every((s) => s === 202)).toBe(true);
    expect(storeOf.get(POST)).toBeUndefined();
  });

  it('T14: a custom perMinute sets the capacity and the refill — { perMinute: 6 } refuses the 7th and refills one token per 10 s (Retry-After: 10)', async () => {
    const { POST } = createSentinelRouteHandler({ rateLimit: { perMinute: 6 } });
    const statuses = await burst(POST, 7, { 'x-forwarded-for': '5.5.5.5' });
    expect(statuses).toEqual([202, 202, 202, 202, 202, 202, 429]);
    const res = await POST(request({ headers: { 'x-forwarded-for': '5.5.5.5' } }));
    expect(res.headers.get('retry-after')).toBe('10');
    await vi.advanceTimersByTimeAsync(10_000);
    expect((await POST(request({ headers: { 'x-forwarded-for': '5.5.5.5' } }))).status).toBe(202);
  });

  it('T14: 100 cross-site requests from a hop spend no token — the bucket is step 4, after the origin check — so all 60 same-origin requests from it pass', async () => {
    const { POST } = createSentinelRouteHandler();
    const crossSite = await burst(POST, 100, { 'x-forwarded-for': '5.5.5.5', 'sec-fetch-site': 'cross-site' });
    expect(crossSite.every((s) => s === 403)).toBe(true);
    const sameOrigin = await burst(POST, 60, { 'x-forwarded-for': '5.5.5.5' });
    expect(sameOrigin.every((s) => s === 202)).toBe(true);
    expect((await POST(request({ headers: { 'x-forwarded-for': '5.5.5.5' } }))).status).toBe(429);
    expect(storeOf.get(POST)?.size).toBe(1);
  });

  it('T14: a 405 and a 415 spend no token either', async () => {
    const { POST } = createSentinelRouteHandler({ rateLimit: { perMinute: 1 } });
    expect((await POST(request({ method: 'GET', headers: { 'x-forwarded-for': '5.5.5.5' } }))).status).toBe(405);
    expect((await POST(request({ headers: { 'x-forwarded-for': '5.5.5.5', 'content-type': 'text/plain' } }))).status).toBe(415);
    expect(storeOf.get(POST)?.size).toBe(0);
    expect((await POST(request({ headers: { 'x-forwarded-for': '5.5.5.5' } }))).status).toBe(202);
    expect((await POST(request({ headers: { 'x-forwarded-for': '5.5.5.5' } }))).status).toBe(429);
  });

  it('T14: createSentinelRouteHandler({ maxBodyBytes: NaN, maxItems: 0, forwardTimeoutMs: -1 }) — each falls back with ONE console.warn naming it, the factory does not throw, a 200 KiB body is admitted', async () => {
    const route = await freshRoute();
    const warned = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    let handler: { POST: (req: Request) => Promise<Response> } | undefined;
    expect(() => {
      handler = route.createSentinelRouteHandler({ maxBodyBytes: NaN, maxItems: 0, forwardTimeoutMs: -1 });
    }).not.toThrow();
    expect(warned).toHaveBeenCalledTimes(3);
    const lines = warned.mock.calls.map((c) => String(c[0]));
    expect(lines.some((l) => l.includes('maxBodyBytes'))).toBe(true);
    expect(lines.some((l) => l.includes('maxItems'))).toBe(true);
    expect(lines.some((l) => l.includes('forwardTimeoutMs'))).toBe(true);
    // Once per option per process: a second bad factory adds nothing.
    route.createSentinelRouteHandler({ maxBodyBytes: 'x' as unknown as number, maxItems: -5, forwardTimeoutMs: Number.POSITIVE_INFINITY });
    expect(warned).toHaveBeenCalledTimes(3);
    const big = JSON.stringify([{ ...ITEM, message: 'm'.repeat(200 * 1024) }]);
    expect(big.length).toBeGreaterThan(200 * 1024);
    const res = await handler!.POST(request({ body: big }));
    expect(res.status).toBe(202);
    // maxItems fell back to 50: 50 pass, 51 do not.
    expect((await handler!.POST(request({ body: JSON.stringify(Array.from({ length: 50 }, () => ITEM)) }))).status).toBe(202);
    expect((await handler!.POST(request({ body: JSON.stringify(Array.from({ length: 51 }, () => ITEM)) }))).status).toBe(400);
  });

  it('T14: the default forwardTimeoutMs after a bad value is 5000 — an unanswered forward is aborted at 5 s', async () => {
    const route = await freshRoute();
    vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    upstream.hang();
    const { POST } = route.createSentinelRouteHandler({ forwardTimeoutMs: 0 });
    const pending = POST(request());
    await vi.advanceTimersByTimeAsync(4_999);
    expect(upstream.calls[0]?.signal?.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(2);
    expect((await pending).status).toBe(503);
  });

  it('T14: 61 requests with X-Forwarded-For: ", 1.2.3.4" beside 61 with "1.2.3.4" share one bucket keyed 1.2.3.4 — the 61st and 62nd of the 122 are 429', async () => {
    const { POST } = createSentinelRouteHandler();
    const statuses: number[] = [];
    for (let i = 0; i < 61; i++) {
      statuses.push((await POST(request({ headers: { 'x-forwarded-for': ', 1.2.3.4' } }))).status);
      statuses.push((await POST(request({ headers: { 'x-forwarded-for': '1.2.3.4' } }))).status);
    }
    expect(statuses.slice(0, 60).every((s) => s === 202)).toBe(true);
    expect(statuses[60]).toBe(429);
    expect(statuses[61]).toBe(429);
    expect(statuses.filter((s) => s === 202)).toHaveLength(60);
    const store = storeOf.get(POST);
    expect(store?.size).toBe(1);
    expect(store?.has('1.2.3.4')).toBe(true);
    expect(store?.has('')).toBe(false);
  });

  it('T14: whitespace around a hop is trimmed and an earlier (browser-written) hop is never the key — "  9.9.9.9 , 10.0.0.1 " keys on 10.0.0.1', async () => {
    const { POST } = createSentinelRouteHandler();
    await POST(request({ headers: { 'x-forwarded-for': '  9.9.9.9 , 10.0.0.1 ' } }));
    await POST(request({ headers: { 'x-forwarded-for': '10.0.0.1' } }));
    const store = storeOf.get(POST);
    expect(store?.size).toBe(1);
    expect(store?.has('10.0.0.1')).toBe(true);
    expect(store?.has('9.9.9.9')).toBe(false);
  });

  it('T14: 61 requests whose LEFTMOST hop varies and whose rightmost hop is fixed share one bucket — the 61st is 429 (a browser cannot mint a bucket per request)', async () => {
    const { POST } = createSentinelRouteHandler();
    const statuses: number[] = [];
    for (let i = 0; i < 61; i++) {
      statuses.push((await POST(request({ headers: { 'x-forwarded-for': `6.6.${i >> 8}.${i & 255}, 198.51.100.23` } }))).status);
    }
    expect(statuses.slice(0, 60).every((s) => s === 202)).toBe(true);
    expect(statuses[60]).toBe(429);
    expect(storeOf.get(POST)?.size).toBe(1);
    expect(storeOf.get(POST)?.has('198.51.100.23')).toBe(true);
  });

  it('T14: trustedProxyHops: 2 keys on the entry second from the right; a hop count past the header keys on the shared bucket, never the socket', async () => {
    const { POST } = createSentinelRouteHandler({ trustedProxyHops: 2 });
    await POST(request({ headers: { 'x-forwarded-for': '6.6.6.6, 198.51.100.23, 10.0.0.1' } }));
    expect(storeOf.get(POST)?.has('198.51.100.23')).toBe(true);
    const short = Object.assign(request({ headers: { 'x-forwarded-for': '198.51.100.23' } }), { socket: { remoteAddress: '203.0.113.9' } });
    await POST(short);
    expect(storeOf.get(POST)?.has(SHARED_KEY)).toBe(true);
    expect(storeOf.get(POST)?.has('203.0.113.9')).toBe(false);
  });

  it('T14: a non-IP rightmost entry keys on the shared bucket — the header text itself is never a key', async () => {
    const { POST } = createSentinelRouteHandler();
    await POST(request({ headers: { 'x-forwarded-for': '1.1.1.1, evil.example' } }));
    expect(storeOf.get(POST)?.size).toBe(1);
    expect(storeOf.get(POST)?.has(SHARED_KEY)).toBe(true);
  });

  it('T14: X-Forwarded-For: "," (no non-empty hop) and a whitespace-only header fall through to the shared bucket, like no header at all', async () => {
    const { POST } = createSentinelRouteHandler({ rateLimit: { perMinute: 3 } });
    expect((await POST(request({ headers: { 'x-forwarded-for': ',' } }))).status).toBe(202);
    expect((await POST(request({ headers: { 'x-forwarded-for': ' ' } }))).status).toBe(202);
    expect((await POST(request({ headers: { 'x-forwarded-for': null } }))).status).toBe(202);
    const res = await POST(request({ headers: { 'x-forwarded-for': ', ,' } }));
    expect(res.status).toBe(429);
    const store = storeOf.get(POST);
    expect(store?.size).toBe(1);
    expect(store?.has(SHARED_KEY)).toBe(true);
    // A real hop is untouched by the shared bucket's exhaustion.
    expect((await POST(request({ headers: { 'x-forwarded-for': '7.7.7.7' } }))).status).toBe(202);
  });
});

describe('T16: the key without a header, and the clientKey option', () => {
  it('T16: no X-Forwarded-For and no socket on the Request (the pure function) — one shared bucket keys every caller', async () => {
    const { POST } = createSentinelRouteHandler({ rateLimit: { perMinute: 2 } });
    expect((await POST(request({ headers: { origin: 'http://localhost:3001', 'user-agent': 'a' } }))).status).toBe(202);
    expect((await POST(request({ headers: { origin: 'http://localhost:3001', 'user-agent': 'b' } }))).status).toBe(202);
    const res = await POST(request({ headers: { 'user-agent': 'c' } }));
    expect(res.status).toBe(429);
    expect((await envelope(res)).error.code).toBe('rate_limited');
    expect(storeOf.get(POST)?.size).toBe(1);
    expect(storeOf.get(POST)?.has(SHARED_KEY)).toBe(true);
  });

  it('T16: with clientKey (web’s getClientIp shape) returning 10.0.0.7 for two requests whose first hops differ, the two share ONE bucket — the header is not consulted', async () => {
    const seen: string[] = [];
    const { POST } = createSentinelRouteHandler({
      rateLimit: { perMinute: 1 },
      clientKey: (req) => {
        seen.push(req.headers.get('x-forwarded-for') ?? '');
        return '10.0.0.7';
      },
    });
    expect((await POST(request({ headers: { 'x-forwarded-for': '1.1.1.1, 10.0.0.7' } }))).status).toBe(202);
    expect((await POST(request({ headers: { 'x-forwarded-for': '2.2.2.2, 10.0.0.7' } }))).status).toBe(429);
    expect(seen).toEqual(['1.1.1.1, 10.0.0.7', '2.2.2.2, 10.0.0.7']);
    const store = storeOf.get(POST);
    expect(store?.size).toBe(1);
    expect(store?.has('10.0.0.7')).toBe(true);
    expect(store?.has('1.1.1.1')).toBe(false);
  });

  it('T16: a clientKey returning null lands the request in the shared bucket; an empty string too', async () => {
    let answer: string | null = null;
    const { POST } = createSentinelRouteHandler({ rateLimit: { perMinute: 2 }, clientKey: () => answer });
    expect((await POST(request({ headers: { 'x-forwarded-for': '1.1.1.1' } }))).status).toBe(202);
    answer = '';
    expect((await POST(request({ headers: { 'x-forwarded-for': '2.2.2.2' } }))).status).toBe(202);
    answer = null;
    expect((await POST(request({ headers: { 'x-forwarded-for': '3.3.3.3' } }))).status).toBe(429);
    const store = storeOf.get(POST);
    expect(store?.size).toBe(1);
    expect(store?.has(SHARED_KEY)).toBe(true);
    // A named key is a different bucket.
    answer = '10.0.0.7';
    expect((await POST(request({ headers: { 'x-forwarded-for': '3.3.3.3' } }))).status).toBe(202);
  });

  it('T16: a clientKey that throws lands its request in the shared bucket with one debug line and no exception', async () => {
    const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
    const { POST } = createSentinelRouteHandler({
      rateLimit: { perMinute: 1 },
      clientKey: () => {
        throw new Error('ip parser broke');
      },
    });
    const first = await POST(request({ headers: { 'x-forwarded-for': '1.1.1.1' } }));
    expect(first.status).toBe(202);
    expect(debug).toHaveBeenCalledTimes(1);
    expect(String(debug.mock.calls[0]?.[0])).toContain('clientKey');
    const second = await POST(request({ headers: { 'x-forwarded-for': '2.2.2.2' } }));
    expect(second.status).toBe(429);
    expect(debug).toHaveBeenCalledTimes(2);
    expect(storeOf.get(POST)?.has(SHARED_KEY)).toBe(true);
    expect(storeOf.get(POST)?.size).toBe(1);
  });

  it('T16: a Request that exposes ip / socket.remoteAddress is keyed on it when there is NO header; a header with no usable hop never falls back to the socket', async () => {
    const { POST } = createSentinelRouteHandler({ rateLimit: { perMinute: 1 } });
    const withIp = Object.assign(request(), { ip: '203.0.113.5' });
    expect((await POST(withIp)).status).toBe(202);
    const withSocket = Object.assign(request(), { socket: { remoteAddress: '203.0.113.5' } });
    expect((await POST(withSocket)).status).toBe(429);
    expect(storeOf.get(POST)?.has('203.0.113.5')).toBe(true);
    // Behind a proxy the socket IS the proxy: an unreadable rightmost hop means "no address", not the socket.
    const headerNoHop = Object.assign(request({ headers: { 'x-forwarded-for': ',' } }), { ip: '203.0.113.6' });
    expect((await POST(headerNoHop)).status).toBe(202);
    expect(storeOf.get(POST)?.has(SHARED_KEY)).toBe(true);
    expect(storeOf.get(POST)?.has('203.0.113.6')).toBe(false);
  });

  it('T16: trustedProxyHops: 0 never reads X-Forwarded-For — a plain Request (the Next App Router shape) keys on the shared bucket; a socket, when one exists, is the key', async () => {
    const { POST } = createSentinelRouteHandler({ trustedProxyHops: 0 });
    await POST(request({ headers: { 'x-forwarded-for': '6.6.6.6' } }));
    expect(storeOf.get(POST)?.has(SHARED_KEY)).toBe(true);
    expect(storeOf.get(POST)?.has('6.6.6.6')).toBe(false);
    await POST(Object.assign(request({ headers: { 'x-forwarded-for': '6.6.6.6' } }), { socket: { remoteAddress: '203.0.113.9' } }));
    expect(storeOf.get(POST)?.has('203.0.113.9')).toBe(true);
  });
});

describe('T17: the bounded bucket store (contract §4.5 step 4)', () => {
  it('T17: through the handler — 10 000 distinct fabricated first hops after an exhausted bucket evict it (least recently seen): the store holds ≤ 10 000 buckets and the first address starts a fresh, full bucket', async () => {
    const { POST } = createSentinelRouteHandler();
    const store = storeOf.get(POST);
    expect(store).toBeDefined();
    // The first address spends its bucket …
    await burst(POST, 60, { 'x-forwarded-for': '1.1.1.1' });
    expect((await POST(request({ headers: { 'x-forwarded-for': '1.1.1.1' } }))).status).toBe(429);
    expect(store?.has('1.1.1.1')).toBe(true);
    // … then a spoofed flood: 10 000 distinct hops, one request each — 10 001 keys seen in all. Each is refused
    // at step 5 (a declared length over the cap) so the flood costs no forward, but the token was taken at step 4.
    for (let i = 0; i < MAX_KEYS; i++) {
      const res = await POST(request({ headers: { 'x-forwarded-for': `10.${(i >> 16) & 255}.${(i >> 8) & 255}.${i & 255}`, 'content-length': '999999999' } }));
      expect(res.status).toBe(413);
    }
    expect(store?.size).toBeLessThanOrEqual(MAX_KEYS);
    expect(store?.size).toBe(MAX_KEYS);
    expect(store?.has('1.1.1.1')).toBe(false);
    // The evicted address is a new key: a fresh bucket starts full, so it passes (it would be 429 unevicted —
    // no clock has advanced, nothing refilled).
    expect((await POST(request({ headers: { 'x-forwarded-for': '1.1.1.1' } }))).status).toBe(202);
    expect(store?.size).toBe(MAX_KEYS);
  }, 60_000);

  it('T17: through the handler — a bucket left untouched (full) for 61 s is removed by the sweep on the next insert; at 30 s it is still there', async () => {
    const { POST } = createSentinelRouteHandler();
    const store = storeOf.get(POST);
    await POST(request({ headers: { 'x-forwarded-for': '1.1.1.1' } }));
    expect(store?.has('1.1.1.1')).toBe(true);
    // 1.1.1.1's bucket is full again one second after its one request; 30 s later that is 29 s of being full.
    await vi.advanceTimersByTimeAsync(30_000);
    await POST(request({ headers: { 'x-forwarded-for': '2.2.2.2' } }));
    expect(store?.has('1.1.1.1')).toBe(true);
    expect(store?.size).toBe(2);
    // 62 s after its request 1.1.1.1's bucket has been full for 61 s → swept; 2.2.2.2's for only 31 s → kept.
    await vi.advanceTimersByTimeAsync(32_000);
    await POST(request({ headers: { 'x-forwarded-for': '3.3.3.3' } }));
    expect(store?.has('1.1.1.1')).toBe(false);
    expect(store?.has('2.2.2.2')).toBe(true);
    expect(store?.has('3.3.3.3')).toBe(true);
    expect(store?.size).toBe(2);
  });

  it('T17: the store — 10 001 distinct keys hold ≤ 10 000 buckets, the first (least recently seen) evicted; a touched key is not the one evicted', () => {
    let now = 0;
    const store = new TokenBucketStore(60, () => now);
    for (let i = 0; i <= MAX_KEYS; i++) {
      // Touch key 0 half-way so it is no longer the least recently seen.
      if (i === 5000) store.take('k0');
      store.take(`k${i}`);
    }
    expect(store.size).toBeLessThanOrEqual(MAX_KEYS);
    expect(store.size).toBe(MAX_KEYS);
    expect(store.has('k0')).toBe(true);
    expect(store.has('k1')).toBe(false);
    expect(store.has(`k${MAX_KEYS}`)).toBe(true);
    now += 1;
    store.take('fresh');
    expect(store.size).toBe(MAX_KEYS);
    expect(store.has('k2')).toBe(false);
  });

  it('T17: the store — a bucket full for ≥ 60 s is swept, one that is not full or not full long enough is kept, and the sweep is clock-gated to once per second', () => {
    let now = 0;
    const store = new TokenBucketStore(60, () => now);
    store.take('idle'); // 59 tokens; full again at t = 1 s
    for (let i = 0; i < 60; i++) store.take('busy'); // empty; full again at t = 60 s
    now = FULL_IDLE_MS + 1000; // 61 s: idle has been full for 60 s, busy for 1 s
    store.take('new');
    expect(store.has('idle')).toBe(false);
    expect(store.has('busy')).toBe(true);
    expect(store.has('new')).toBe(true);
    // Gated: an insert in the same second does not sweep again — busy, full since t = 60 s, survives an insert at
    // t = 121 s only because the gate holds; the forced sweep removes it (new, full since t = 62 s, stays).
    now = FULL_IDLE_MS + 1000 + 999;
    store.take('same-second');
    now = FULL_IDLE_MS + 61_000;
    store.sweep(now, true);
    expect(store.has('busy')).toBe(false);
    expect(store.has('new')).toBe(true);
    // At t = 122.5 s new has been full for 60.5 s: swept on the next insert (a new second, so the gate opens);
    // same-second, full since t = 62.999 s, has 59.5 s and stays.
    now = FULL_IDLE_MS + 62_500;
    store.take('later');
    expect(store.has('new')).toBe(false);
    expect([...['same-second', 'later']].every((k) => store.has(k))).toBe(true);
    expect(store.size).toBe(2);
  });

  it('T17: the store — Retry-After is ceil(seconds to the next token) at any refill rate, never below 1', () => {
    let now = 0;
    const store = new TokenBucketStore(60, () => now);
    for (let i = 0; i < 60; i++) expect(store.take('k')).toEqual({ ok: true });
    expect(store.take('k')).toEqual({ ok: false, retryAfterSeconds: 1 });
    now = 999;
    expect(store.take('k')).toEqual({ ok: false, retryAfterSeconds: 1 });
    now = 1000;
    expect(store.take('k')).toEqual({ ok: true });
    const slow = new TokenBucketStore(2, () => now); // one token per 30 s
    expect(slow.take('k')).toEqual({ ok: true });
    expect(slow.take('k')).toEqual({ ok: true });
    expect(slow.take('k')).toEqual({ ok: false, retryAfterSeconds: 30 });
    now += 10_000;
    expect(slow.take('k')).toEqual({ ok: false, retryAfterSeconds: 20 });
  });
});
