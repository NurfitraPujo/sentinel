// `createSentinelRouteHandler` as a function (contract §4.5, §7; flows T1–T17, `code` halves; T18 the no-fallback apiKey rule): `Request` →
// `Response` with `globalThis.fetch` stubbed as the ingestor. The factory returns `{ POST }` only; the nine steps
// run in the contract's order — the rate limit at step 4, before the body is read; the origin rule is `web`'s
// transcribed (port included, whole-string host compare); the body taxonomy is complete; one forward attempt;
// the relay copies status, body, `Content-Type` and `Retry-After` only; and no response ever carries the key or
// the endpoint. The bucket and the bounded store are in bucket.test.ts (T14, T16, T17).
// Shuffle-safe: every test builds its own handler; the env is stubbed per test and restored.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { storeOf } from '../../src/nextjs/rate-limit.js';
import { applyClientIp, canonicalIp, createSentinelRouteHandler, isCrossSiteRequest, normaliseBody } from '../../src/nextjs/route.js';
import { ENDPOINT, envelope, freshRoute, ITEM, KEY, PROJECT_KEY, request, stubUpstream, type Upstream } from './helpers.js';

let upstream: Upstream;

beforeEach(() => {
  upstream = stubUpstream();
  vi.stubEnv('SENTINEL_ENDPOINT', ENDPOINT);
  vi.stubEnv('SENTINEL_API_KEY', KEY);
  vi.stubEnv('SENTINEL_PROJECT_KEY', PROJECT_KEY);
});

afterEach(() => {
  vi.unstubAllEnvs();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('the factory (contract §4.5)', () => {
  it('returns an object whose only key is POST — a (req: Request) => Promise<Response> function', () => {
    const handler = createSentinelRouteHandler();
    expect(Reflect.ownKeys(handler)).toEqual(['POST']);
    expect(typeof handler.POST).toBe('function');
    expect(Reflect.ownKeys(createSentinelRouteHandler({ rateLimit: false }))).toEqual(['POST']);
  });

  it('never throws on a bad options value — null, a string, a function', () => {
    expect(() => createSentinelRouteHandler(null as unknown as undefined)).not.toThrow();
    expect(() => createSentinelRouteHandler('x' as unknown as undefined)).not.toThrow();
    expect(() => createSentinelRouteHandler({ isCrossSite: 'no' as unknown as () => boolean, clientKey: 3 as unknown as () => null })).not.toThrow();
  });
});

describe('T1 (code half): same-origin POST, JSON array, env set', () => {
  it('T1: 202 with the ingestor’s batch body verbatim and Content-Type copied; POST {endpoint}/batch with X-API-Key and X-Project-Key, the body re-serialised', async () => {
    const { POST } = createSentinelRouteHandler();
    const items = [ITEM, { ...ITEM, event_id: '22222222-2222-4222-8222-222222222222' }, { ...ITEM, event_id: '33333333-3333-4333-8333-333333333333' }];
    const res = await POST(request({ body: JSON.stringify(items) }));
    expect(res.status).toBe(202);
    expect(res.headers.get('content-type')).toBe('application/json');
    expect(await res.json()).toEqual({ ingested: 3, failed: 0, errors: [], event_ids: [] });
    expect(upstream.calls).toHaveLength(1);
    const call = upstream.calls[0];
    expect(call?.url).toBe(`${ENDPOINT}/batch`);
    expect(call?.method).toBe('POST');
    expect(call?.headers['x-api-key']).toBe(KEY);
    expect(call?.headers['x-project-key']).toBe(PROJECT_KEY);
    expect(call?.headers['content-type']).toBe('application/json');
    expect(call?.items).toEqual(items);
  });

  it('T1: the endpoint’s trailing slash is dropped before /batch; the endpoint / apiKey / projectKey options beat the env', async () => {
    const { POST } = createSentinelRouteHandler({ endpoint: 'http://other.internal/ingest/', apiKey: 'opt-key', projectKey: 'opt-project' });
    const res = await POST(request());
    expect(res.status).toBe(202);
    expect(upstream.calls[0]?.url).toBe('http://other.internal/ingest/batch');
    expect(upstream.calls[0]?.headers['x-api-key']).toBe('opt-key');
    expect(upstream.calls[0]?.headers['x-project-key']).toBe('opt-project');
  });

  it('T1: without a project key nothing is sent as X-Project-Key', async () => {
    vi.stubEnv('SENTINEL_PROJECT_KEY', '');
    const { POST } = createSentinelRouteHandler();
    await POST(request());
    expect(upstream.calls[0]?.headers).not.toHaveProperty('x-project-key');
  });
});

describe('T2: GET — unit-only, the handler called as a function', () => {
  it('T2: 405 method_not_allowed with Allow: POST (under Next the { POST } export means Next answers first)', async () => {
    const { POST } = createSentinelRouteHandler();
    for (const method of ['GET', 'PUT', 'DELETE', 'OPTIONS']) {
      const res = await POST(request({ method }));
      expect(res.status, method).toBe(405);
      expect(res.headers.get('allow')).toBe('POST');
      expect(res.headers.get('content-type')).toBe('application/json');
      const body = await envelope(res);
      expect(body.error.code).toBe('method_not_allowed');
      expect(body.error.details).toEqual({});
    }
    expect(upstream.calls).toHaveLength(0);
  });
});

describe('T3–T6: the origin rule, web’s isCrossSiteRequest transcribed (contract §4.5 step 2)', () => {
  it('T3: Sec-Fetch-Site: cross-site → 403 cross_site_request, nothing forwarded', async () => {
    const { POST } = createSentinelRouteHandler();
    const res = await POST(request({ headers: { 'sec-fetch-site': 'cross-site' } }));
    expect(res.status).toBe(403);
    expect((await envelope(res)).error.code).toBe('cross_site_request');
    expect(upstream.calls).toHaveLength(0);
  });

  it('T3: Sec-Fetch-Site present with any value but same-origin / same-site is cross-site; same-site passes', async () => {
    const { POST } = createSentinelRouteHandler();
    expect((await POST(request({ headers: { 'sec-fetch-site': 'none' } }))).status).toBe(403);
    expect((await POST(request({ headers: { 'sec-fetch-site': 'SAME-ORIGIN' } }))).status).toBe(403);
    expect((await POST(request({ headers: { 'sec-fetch-site': 'same-site', origin: 'https://evil.example' } }))).status).toBe(202);
  });

  it.each([
    ['Origin: https://evil.example against Host: app.example.com', { origin: 'https://evil.example', host: 'app.example.com' }],
    ['Origin: https://app.example.com against Host: app.example.com:443 (the raw header carries a port the URL’s host drops)', { origin: 'https://app.example.com', host: 'app.example.com:443' }],
    ['Origin: https://app.example.com against X-Forwarded-Host: app.example.com, proxy.internal (no comma splitting)', { origin: 'https://app.example.com', host: 'other.internal', 'x-forwarded-host': 'app.example.com, proxy.internal' }],
    ['Origin: not a url', { origin: 'not a url', host: 'not a url' }],
  ])('T4: no Sec-Fetch-Site; %s → 403 cross_site_request', async (_name, headers) => {
    const { POST } = createSentinelRouteHandler();
    const res = await POST(request({ headers: { 'sec-fetch-site': null, ...headers } }));
    expect(res.status).toBe(403);
    expect((await envelope(res)).error.code).toBe('cross_site_request');
    expect(upstream.calls).toHaveLength(0);
  });

  it('T4: hostname-only and port-stripped compares are NOT accepted — Origin: http://localhost:3001 against Host: localhost is cross-site', async () => {
    const { POST } = createSentinelRouteHandler();
    expect((await POST(request({ headers: { 'sec-fetch-site': null, origin: 'http://localhost:3001', host: 'localhost' } }))).status).toBe(403);
    expect((await POST(request({ headers: { 'sec-fetch-site': null, origin: 'http://localhost', host: 'localhost:3001' } }))).status).toBe(403);
  });

  it.each([
    ['Origin: https://app.example.com with X-Forwarded-Host: app.example.com', { origin: 'https://app.example.com', host: 'pod.internal:3000', 'x-forwarded-host': 'app.example.com' }],
    ['Origin: http://localhost:3001 with Host: localhost:3001 (the compose stack; port included on both sides)', { origin: 'http://localhost:3001', host: 'localhost:3001' }],
  ])('T5: no Sec-Fetch-Site; %s → passes step 2 and is forwarded', async (_name, headers) => {
    const { POST } = createSentinelRouteHandler();
    const res = await POST(request({ headers: { 'sec-fetch-site': null, ...headers } }));
    expect(res.status).toBe(202);
    expect(upstream.calls).toHaveLength(1);
  });

  it('T6: no Sec-Fetch-Site and no Origin (a curl) → 403; Origin: null (a sandboxed iframe) → 403', async () => {
    const { POST } = createSentinelRouteHandler();
    const curl = await POST(request({ headers: { 'sec-fetch-site': null, origin: null } }));
    expect(curl.status).toBe(403);
    expect((await envelope(curl)).error.code).toBe('cross_site_request');
    const iframe = await POST(request({ headers: { 'sec-fetch-site': null, origin: 'null' } }));
    expect(iframe.status).toBe(403);
    expect((await envelope(iframe)).error.code).toBe('cross_site_request');
    expect(upstream.calls).toHaveLength(0);
  });

  it('T4–T6: isCrossSiteRequest is exported for the app that wants the same rule; a Request with no host header at all is cross-site', () => {
    expect(isCrossSiteRequest(request({ headers: { 'sec-fetch-site': null, host: null, origin: 'http://localhost:3001' } }))).toBe(true);
    expect(isCrossSiteRequest(request())).toBe(false);
  });
});

describe('T7: the media type (step 3)', () => {
  it('T7: Content-Type: text/plain → 415 unsupported_media_type; a missing Content-Type too; parameters are ignored', async () => {
    const { POST } = createSentinelRouteHandler();
    const plain = await POST(request({ headers: { 'content-type': 'text/plain' } }));
    expect(plain.status).toBe(415);
    expect((await envelope(plain)).error.code).toBe('unsupported_media_type');
    expect((await POST(request({ headers: { 'content-type': null } }))).status).toBe(415);
    expect((await POST(request({ headers: { 'content-type': 'Application/JSON; charset=utf-8' } }))).status).toBe(202);
    expect(upstream.calls).toHaveLength(1);
  });
});

describe('T8: the size cap (step 5)', () => {
  it('T8: a 300 KiB body → 413 payload_too_large, and the stream is not read past maxBodyBytes', async () => {
    const { POST } = createSentinelRouteHandler();
    const chunk = new TextEncoder().encode('x'.repeat(16 * 1024));
    let pulled = 0;
    let cancelled = false;
    const stream = new ReadableStream<Uint8Array>({
      pull(controller) {
        if (pulled >= 300 * 1024) {
          controller.close();
          return;
        }
        pulled += chunk.byteLength;
        controller.enqueue(chunk);
      },
      cancel() {
        cancelled = true;
      },
    });
    const res = await POST(request({ body: stream }));
    expect(res.status).toBe(413);
    expect((await envelope(res)).error.code).toBe('payload_too_large');
    // At most the cap plus the chunk that crossed it was pulled — never the whole 300 KiB.
    expect(pulled).toBeLessThanOrEqual(262_144 + chunk.byteLength);
    expect(pulled).toBeLessThan(300 * 1024);
    expect(cancelled).toBe(true);
    expect(upstream.calls).toHaveLength(0);
  });

  it('T8: a Content-Length over the cap is refused before any byte is read', async () => {
    const { POST } = createSentinelRouteHandler();
    let pulled = 0;
    const stream = new ReadableStream<Uint8Array>({
      pull(controller) {
        pulled++;
        controller.enqueue(new Uint8Array(1024));
      },
    });
    const req = request({ body: stream, headers: { 'content-length': String(300 * 1024) } });
    // A ReadableStream primes itself (one pull up to its high-water mark) on a tick of its own; what matters is
    // that the route pulls nothing more.
    await new Promise((r) => setTimeout(r, 0));
    const primed = pulled;
    const res = await POST(req);
    expect(res.status).toBe(413);
    expect(pulled).toBe(primed);
  });

  it('T8: a lowered maxBodyBytes applies to a string body; a body exactly at the cap is admitted', async () => {
    const { POST } = createSentinelRouteHandler({ maxBodyBytes: 100 });
    const over = JSON.stringify([{ ...ITEM, message: 'y'.repeat(200) }]);
    expect((await POST(request({ body: over }))).status).toBe(413);
    const exact = JSON.stringify([{ m: 'z'.repeat(100 - '[{"m":""}]'.length) }]);
    expect(exact.length).toBe(100);
    expect((await POST(request({ body: exact }))).status).toBe(202);
  });
});

describe('T9: a single foreign object (step 6)', () => {
  it('T9: wrapped into a one-element array; project_key and project_id deleted; platform forced to javascript; unknown keys deleted; forwarded to the key’s own project', async () => {
    const { POST } = createSentinelRouteHandler();
    const foreign = {
      project_key: 'other-project',
      project_id: 'other-id',
      platform: 'golang',
      error_class: 'E',
      environment: 'production',
      timestamp: '2026-09-16T00:00:00Z',
      dsn: 'https://evil',
      tenant: 'x',
      release_version: '1.2.3',
      fingerprint: 'f',
    };
    const res = await POST(request({ body: JSON.stringify(foreign) }));
    expect(res.status).toBe(202);
    expect(upstream.calls).toHaveLength(1);
    expect(upstream.calls[0]?.items).toEqual([{ platform: 'javascript', error_class: 'E', environment: 'production', timestamp: '2026-09-16T00:00:00Z', release_version: '1.2.3', fingerprint: 'f' }]);
    expect(upstream.calls[0]?.body).not.toContain('project_key');
    expect(upstream.calls[0]?.body).not.toContain('other-project');
    expect(upstream.calls[0]?.body).not.toContain('golang');
  });

  it('T9: every item of an array is cleaned the same way, and the item order is kept', async () => {
    const { POST } = createSentinelRouteHandler();
    await POST(request({ body: JSON.stringify([{ ...ITEM, project_key: 'a', junk: 1 }, { ...ITEM, event_id: 'second', project_id: 'b' }]) }));
    const items = upstream.calls[0]?.items as Record<string, unknown>[];
    expect(items.map((i) => i.event_id)).toEqual([ITEM.event_id, 'second']);
    for (const item of items) {
      expect(item).not.toHaveProperty('project_key');
      expect(item).not.toHaveProperty('project_id');
      expect(item).not.toHaveProperty('junk');
      expect(item.platform).toBe('javascript');
    }
  });
});

describe('T10: the body taxonomy, complete (step 6)', () => {
  it.each([
    ['[…51 items]', 'items', JSON.stringify(Array.from({ length: 51 }, () => ITEM))],
    ['"not json"', 'shape', 'not json'],
    ['[1, 2]', 'shape', '[1, 2]'],
    ['[{ …, "metadata": "x" }]', 'metadata', JSON.stringify([{ ...ITEM, metadata: 'x' }])],
    ['[{ …, "metadata": [] }]', 'metadata', JSON.stringify([{ ...ITEM, metadata: [] }])],
    ['[{ …, "metadata": null }]', 'metadata', JSON.stringify([{ ...ITEM, metadata: null }])],
    ['[{ …, "metadata": 1 }]', 'metadata', JSON.stringify([{ ...ITEM, metadata: 1 }])],
    ['[] (zero is outside 1..maxItems)', 'items', '[]'],
    ['null', 'shape', 'null'],
    ['"x"', 'shape', '"x"'],
    ['1', 'shape', '1'],
    ['true', 'shape', 'true'],
    ['[null]', 'shape', '[null]'],
    ['[[]] (an array element)', 'shape', '[[]]'],
    ['[{}, 1] (one non-object element)', 'shape', '[{}, 1]'],
    ['an empty body', 'shape', ''],
  ])('T10: %s → 400 invalid_request with details.reason %s and nothing forwarded', async (_name, reason, body) => {
    const { POST } = createSentinelRouteHandler();
    const res = await POST(request({ body }));
    expect(res.status).toBe(400);
    const env = await envelope(res);
    expect(env.error.code).toBe('invalid_request');
    expect(env.error.details.reason).toBe(reason);
    expect(upstream.calls).toHaveLength(0);
  });

  it('T10: {} is wrapped into [{}] (platform forced) and forwarded — the route validates shape, never item content', async () => {
    const { POST } = createSentinelRouteHandler();
    upstream.answer(400, JSON.stringify({ ingested: 0, failed: 1, errors: [{ index: 0, message: 'validation failed: error_class is required' }] }));
    const res = await POST(request({ body: '{}' }));
    expect(upstream.calls).toHaveLength(1);
    expect(upstream.calls[0]?.items).toEqual([{ platform: 'javascript' }]);
    // The ingestor’s answer for the empty item is relayed as is.
    expect(res.status).toBe(400);
    expect(await res.json()).toMatchObject({ ingested: 0, failed: 1 });
  });

  it('T10: exactly maxItems items pass; a lowered maxItems applies', async () => {
    const { POST } = createSentinelRouteHandler();
    expect((await POST(request({ body: JSON.stringify(Array.from({ length: 50 }, () => ITEM)) }))).status).toBe(202);
    const small = createSentinelRouteHandler({ maxItems: 2 });
    expect((await small.POST(request({ body: JSON.stringify([ITEM, ITEM]) }))).status).toBe(202);
    const res = await small.POST(request({ body: JSON.stringify([ITEM, ITEM, ITEM]) }));
    expect(res.status).toBe(400);
    expect((await envelope(res)).error.details.reason).toBe('items');
  });

  it('T10: normaliseBody keeps every ErrorPayload key the ingestor decodes and nothing else', () => {
    const full = { event_id: 'e', platform: 'p', environment: 'env', message: 'm', error_class: 'c', trace_id: 't', span_id: 's', stacktrace: [], metadata: {}, timestamp: 'ts', trace_flags: 1, release_version: 'r', fingerprint: 'f', fingerprint_override: false, level: 'error', sdk: {}, user: {}, tags: {}, contexts: {}, request: {}, breadcrumbs: [], project_key: 'pk', project_id: 'pid', extra: 1 };
    const out = normaliseBody(JSON.stringify(full), 50);
    expect(out.ok).toBe(true);
    if (out.ok) {
      expect(Object.keys(out.items[0] ?? {}).sort()).toEqual(['breadcrumbs', 'contexts', 'environment', 'error_class', 'event_id', 'fingerprint', 'fingerprint_override', 'level', 'message', 'metadata', 'platform', 'release_version', 'request', 'sdk', 'span_id', 'stacktrace', 'tags', 'timestamp', 'trace_flags', 'trace_id', 'user']);
      expect(out.items[0]?.platform).toBe('javascript');
    }
  });
});

describe('T11: SENTINEL_API_KEY unset (step 7)', () => {
  it('T11: 503 sentinel_not_configured; one warn per process; the response contains neither the endpoint nor any key; nothing forwarded', async () => {
    vi.stubEnv('SENTINEL_API_KEY', '');
    const route = await freshRoute();
    const warned = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const { POST } = route.createSentinelRouteHandler();
    const first = await POST(request());
    const second = await POST(request());
    const third = await route.createSentinelRouteHandler().POST(request());
    for (const res of [first, second, third]) {
      expect(res.status).toBe(503);
      expect(res.headers.get('content-type')).toBe('application/json');
    }
    const env = await envelope(first);
    expect(env.error.code).toBe('sentinel_not_configured');
    const text = JSON.stringify([env, [...first.headers.entries()]]);
    expect(text).not.toContain(ENDPOINT);
    expect(text).not.toContain('ingest.internal');
    expect(text).not.toContain(KEY);
    expect(text).not.toContain(PROJECT_KEY);
    expect(warned).toHaveBeenCalledTimes(1);
    expect(warned.mock.calls[0]?.join(' ')).not.toContain(KEY);
    expect(upstream.calls).toHaveLength(0);
  });

  it('T11: SENTINEL_ENDPOINT unset → the same 503, and the key never appears in the body or the headers', async () => {
    vi.stubEnv('SENTINEL_ENDPOINT', '');
    vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const { POST } = createSentinelRouteHandler();
    const res = await POST(request());
    expect(res.status).toBe(503);
    const text = (await res.text()) + JSON.stringify([...res.headers.entries()]);
    expect(text).toContain('sentinel_not_configured');
    expect(text).not.toContain(KEY);
    expect(upstream.calls).toHaveLength(0);
  });
});

describe('T18: the apiKey option present but unset never falls back to SENTINEL_API_KEY (step 7; sdk-js decision 41)', () => {
  /** Record every `process.env` property read for the call's duration — the route reads env per request, inside the handler. */
  async function envReadsDuring(run: () => Promise<void>): Promise<string[]> {
    const reads: string[] = [];
    const real = process.env;
    process.env = new Proxy(real, {
      get(target, prop, receiver) {
        if (typeof prop === 'string') reads.push(prop);
        return Reflect.get(target, prop, receiver);
      },
    });
    try {
      await run();
    } finally {
      process.env = real;
    }
    return reads;
  }

  it('(a) { apiKey: undefined } with SENTINEL_API_KEY set → 503 sentinel_not_configured, one warn per process naming the option and the variable, SENTINEL_API_KEY never read, nothing forwarded', async () => {
    const route = await freshRoute();
    const warned = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    // Exactly web's mount with the tunnel variable unset: an own property whose value is `undefined`.
    const { POST } = route.createSentinelRouteHandler({ apiKey: process.env.SENTINEL_TUNNEL_API_KEY });
    let first: Response | undefined;
    let second: Response | undefined;
    const reads = await envReadsDuring(async () => {
      first = await POST(request());
      second = await POST(request());
    });
    const third = await route.createSentinelRouteHandler({ apiKey: undefined }).POST(request());
    for (const res of [first!, second!, third]) {
      expect(res.status).toBe(503);
      expect(res.headers.get('content-type')).toBe('application/json');
      expect((await envelope(res)).error.code).toBe('sentinel_not_configured');
    }
    expect(reads).not.toContain('SENTINEL_API_KEY');
    expect(upstream.calls).toHaveLength(0);
    expect(warned).toHaveBeenCalledTimes(1);
    const line = warned.mock.calls[0]?.join(' ') ?? '';
    expect(line).toContain('apiKey option is unset');
    expect(line).toContain('not falling back to SENTINEL_API_KEY');
    expect(line).not.toContain(KEY);
  });

  it("(a') { apiKey: '' } is the same: present and unset → 503, no fallback, nothing forwarded", async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const { POST } = createSentinelRouteHandler({ apiKey: '' });
    const res = await POST(request());
    expect(res.status).toBe(503);
    expect((await envelope(res)).error.code).toBe('sentinel_not_configured');
    expect(upstream.calls).toHaveLength(0);
  });

  it('(b) the option absent → SENTINEL_API_KEY is the documented default and the batch is forwarded with it', async () => {
    const { POST } = createSentinelRouteHandler({});
    const res = await POST(request());
    expect(res.status).toBe(202);
    expect(upstream.calls).toHaveLength(1);
    expect(upstream.calls[0]?.headers['x-api-key']).toBe(KEY);
  });

  it('(c) the option set → it is used, and SENTINEL_API_KEY (set to a different value) is not', async () => {
    const tunnelKey = 'sk_live_tunnel_0123456789abcdef';
    const { POST } = createSentinelRouteHandler({ apiKey: tunnelKey });
    const res = await POST(request());
    expect(res.status).toBe(202);
    expect(upstream.calls).toHaveLength(1);
    expect(upstream.calls[0]?.headers['x-api-key']).toBe(tunnelKey);
    expect(upstream.calls[0]?.headers['x-api-key']).not.toBe(KEY);
  });
});

describe('T12: the ingestor unreachable or slower than forwardTimeoutMs (steps 8–9)', () => {
  it('T12: a network failure → 503 upstream_unavailable with Retry-After on the first failure — exactly one upstream request, never a retry', async () => {
    upstream.fail();
    const { POST } = createSentinelRouteHandler();
    const res = await POST(request());
    expect(res.status).toBe(503);
    expect(res.headers.get('retry-after')).toBe('5');
    expect((await envelope(res)).error.code).toBe('upstream_unavailable');
    expect(upstream.calls).toHaveLength(1);
  });

  it('T12: a forward slower than forwardTimeoutMs is aborted → 503 upstream_unavailable with Retry-After; one upstream request', async () => {
    vi.useFakeTimers();
    upstream.hang();
    const { POST } = createSentinelRouteHandler({ forwardTimeoutMs: 200 });
    const pending = POST(request());
    await vi.advanceTimersByTimeAsync(199);
    expect(upstream.calls).toHaveLength(1);
    expect(upstream.calls[0]?.signal?.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(2);
    const res = await pending;
    expect(res.status).toBe(503);
    expect(res.headers.get('retry-after')).toBe('5');
    expect((await envelope(res)).error.code).toBe('upstream_unavailable');
    expect(upstream.calls).toHaveLength(1);
    expect(upstream.calls[0]?.signal?.aborted).toBe(true);
  });

  it('T12: the default forwardTimeoutMs is 5000 — the attempt is aborted at 5 s, not before', async () => {
    vi.useFakeTimers();
    upstream.hang();
    const { POST } = createSentinelRouteHandler();
    const pending = POST(request());
    await vi.advanceTimersByTimeAsync(4_999);
    expect(upstream.calls[0]?.signal?.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(2);
    expect((await pending).status).toBe(503);
    expect(upstream.calls[0]?.signal?.aborted).toBe(true);
  });
});

describe('T13: the ingestor answers 429 + Retry-After: 60 (step 9)', () => {
  it('T13: relayed as 429 with Retry-After: 60 and the plain-text body; Content-Type copied; no other upstream header relayed', async () => {
    upstream.answer(429, 'Rate limit exceeded\n', { 'content-type': 'text/plain; charset=utf-8', 'retry-after': '60', 'x-request-id': 'abc', 'set-cookie': 'a=b', server: 'ingestor' });
    const { POST } = createSentinelRouteHandler();
    const res = await POST(request());
    expect(res.status).toBe(429);
    expect(res.headers.get('retry-after')).toBe('60');
    expect(res.headers.get('content-type')).toBe('text/plain; charset=utf-8');
    expect(await res.text()).toBe('Rate limit exceeded\n');
    expect(res.headers.get('x-request-id')).toBeNull();
    expect(res.headers.get('set-cookie')).toBeNull();
    expect(res.headers.get('server')).toBeNull();
  });

  it('T13: every other ingestor status is relayed verbatim too — 401 "Missing API key", 400 with the batch JSON, 500', async () => {
    const { POST } = createSentinelRouteHandler();
    upstream.answer(401, 'Missing API key\n', { 'content-type': 'text/plain' });
    let res = await POST(request());
    expect([res.status, await res.text()]).toEqual([401, 'Missing API key\n']);
    upstream.answer(400, JSON.stringify({ ingested: 0, failed: 1, errors: [{ index: 0, message: 'validation failed: x' }] }));
    res = await POST(request());
    expect(res.status).toBe(400);
    expect((await res.json()) as unknown).toMatchObject({ failed: 1 });
    upstream.answer(500, 'Failed to ingest error\n', { 'content-type': 'text/plain' });
    res = await POST(request());
    expect([res.status, await res.text()]).toEqual([500, 'Failed to ingest error\n']);
    expect(res.headers.get('retry-after')).toBeNull();
  });
});

describe('T15: the isCrossSite option (step 2)', () => {
  it('T15: a supplied isCrossSite decides step 2 — the default is not consulted in either direction', async () => {
    const admit = createSentinelRouteHandler({ isCrossSite: () => false });
    expect((await admit.POST(request({ headers: { 'sec-fetch-site': 'cross-site', origin: 'https://evil.example' } }))).status).toBe(202);
    const seen: string[] = [];
    const refuse = createSentinelRouteHandler({
      isCrossSite: (req) => {
        seen.push(req.headers.get('origin') ?? '');
        return true;
      },
    });
    const res = await refuse.POST(request());
    expect(res.status).toBe(403);
    expect((await envelope(res)).error.code).toBe('cross_site_request');
    expect(seen).toEqual(['http://localhost:3001']);
    expect(upstream.calls).toHaveLength(1);
  });

  it('T15: an isCrossSite that throws is read as cross-site — 403 cross_site_request, one debug line, nothing thrown at Next', async () => {
    const debug = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
    const { POST } = createSentinelRouteHandler({
      isCrossSite: () => {
        throw new Error('origin check broke');
      },
    });
    const res = await POST(request());
    expect(res.status).toBe(403);
    expect((await envelope(res)).error.code).toBe('cross_site_request');
    expect(debug).toHaveBeenCalledTimes(1);
    expect(String(debug.mock.calls[0]?.[0])).toContain('isCrossSite');
    expect(upstream.calls).toHaveLength(0);
  });
});

describe('the step order (contract §4.5): each step answers before the next is consulted', () => {
  it('a GET that is also cross-site and text/plain is a 405; a cross-site request with a bad media type is a 403; a same-origin text/plain 415 — and none reads the body or spends a token', async () => {
    let read = 0;
    const counted = () =>
      new ReadableStream<Uint8Array>({
        pull(controller) {
          read++;
          controller.enqueue(new TextEncoder().encode('['));
        },
      });
    const { POST } = createSentinelRouteHandler({ rateLimit: { perMinute: 1 } });
    const rows = [
      request({ method: 'GET', headers: { 'sec-fetch-site': 'cross-site', 'content-type': 'text/plain' } }),
      request({ headers: { 'sec-fetch-site': 'cross-site', 'content-type': 'text/plain' }, body: counted() }),
      request({ headers: { 'content-type': 'text/plain' }, body: counted() }),
    ];
    // A ReadableStream primes itself (one pull up to its high-water mark) on a tick of its own; the three refused
    // rows must pull nothing more.
    await new Promise((r) => setTimeout(r, 0));
    const primed = read;
    expect((await POST(rows[0] as Request)).status).toBe(405);
    expect((await POST(rows[1] as Request)).status).toBe(403);
    expect((await POST(rows[2] as Request)).status).toBe(415);
    expect(read).toBe(primed);
    // The one token is still there: the first request to reach step 4 passes it (and reads the body), the next is the 429.
    expect((await POST(request({ body: '[1]' }))).status).toBe(400);
    expect((await POST(request({ body: '[1]' }))).status).toBe(429);
  });
});

describe('what the route never does (contract §4.5)', () => {
  it('no client header but the User-Agent reaches the upstream in any forwarded row — not Cookie, Authorization, X-Forwarded-For, traceparent, Host, Origin or an inbound x-sentinel-derive-ip; the marker is always off', async () => {
    const { POST } = createSentinelRouteHandler();
    const client = {
      'x-real-ip': '6.6.6.6',
      forwarded: 'for=6.6.6.6',
      'x-client-ip': '6.6.6.6',
      'x-sentinel-derive-ip': 'on',
      cookie: 'session=abc',
      authorization: 'Bearer user-token',
      'x-forwarded-for': '203.0.113.9, 10.0.0.1',
      traceparent: '00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01',
      'x-forwarded-host': 'localhost:3001',
      'user-agent': 'browser/1',
    };
    const rows = [
      request({ headers: client }),
      request({ headers: { ...client, 'sec-fetch-site': null, origin: 'https://app.example.com', 'x-forwarded-host': 'app.example.com' } }),
      request({ headers: { ...client, 'sec-fetch-site': null, origin: 'http://localhost:3001', host: 'localhost:3001' } }),
      request({ headers: client, body: JSON.stringify({ project_key: 'other', platform: 'golang', error_class: 'E' }) }),
      request({ headers: client, body: '{}' }),
    ];
    for (const row of rows) await POST(row);
    expect(upstream.calls).toHaveLength(rows.length);
    for (const call of upstream.calls) {
      expect(Object.keys(call.headers).sort()).toEqual(['content-type', 'user-agent', 'x-api-key', 'x-project-key', 'x-sentinel-derive-ip']);
      expect(call.headers['user-agent']).toBe('browser/1');
      expect(call.headers['x-sentinel-derive-ip']).toBe('off');
      expect(call.headers).not.toHaveProperty('x-forwarded-for');
    }
  });

  it('T1–T17: no response body or header of any row carries the configured key, the project key or the endpoint', async () => {
    const debugSilenced = vi.spyOn(console, 'debug').mockImplementation(() => undefined);
    vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const bodies: string[] = [];
    const record = async (res: Response): Promise<void> => {
      bodies.push(`${res.status} ${JSON.stringify([...res.headers.entries()])} ${await res.text()}`);
    };
    const { POST } = createSentinelRouteHandler({ rateLimit: { perMinute: 4 } });
    await record(await POST(request())); // T1
    await record(await POST(request({ method: 'GET' }))); // T2
    await record(await POST(request({ headers: { 'sec-fetch-site': 'cross-site' } }))); // T3
    await record(await POST(request({ headers: { 'sec-fetch-site': null, origin: 'https://evil.example', host: 'app.example.com' } }))); // T4
    await record(await POST(request({ headers: { 'sec-fetch-site': null, origin: null } }))); // T6
    await record(await POST(request({ headers: { 'content-type': 'text/plain' } }))); // T7
    await record(await POST(request({ headers: { 'content-length': '999999' } }))); // T8
    await record(await POST(request({ body: '[1, 2]' }))); // T10
    await record(await POST(request({ body: '[]' }))); // T10
    await record(await POST(request())); // T14: the four tokens are spent (T1, T8, T10, T10) → 429
    const unconfigured = createSentinelRouteHandler({ apiKey: '' });
    vi.stubEnv('SENTINEL_API_KEY', '');
    await record(await unconfigured.POST(request())); // T11
    vi.stubEnv('SENTINEL_API_KEY', KEY);
    const relay = createSentinelRouteHandler();
    upstream.answer(429, 'Rate limit exceeded', { 'content-type': 'text/plain', 'retry-after': '60' });
    await record(await relay.POST(request())); // T13
    upstream.fail();
    await record(await relay.POST(request())); // T12
    upstream.answer(202);
    const throwing = createSentinelRouteHandler({
      isCrossSite: () => {
        throw new Error(`leak ${KEY}`);
      },
      clientKey: () => {
        throw new Error('leak');
      },
    });
    await record(await throwing.POST(request())); // T15 / T16
    await record(await createSentinelRouteHandler({ maxBodyBytes: NaN }).POST(request({ body: JSON.stringify([ITEM]) }))); // T14
    expect(bodies.length).toBe(15);
    expect(new Set(bodies.map((b) => b.slice(0, 3)))).toEqual(new Set(['202', '405', '403', '415', '413', '400', '429', '503']));
    for (const body of bodies) {
      expect(body).not.toContain(KEY);
      expect(body).not.toContain(PROJECT_KEY);
      expect(body).not.toContain(ENDPOINT);
      expect(body).not.toContain('ingest.internal');
    }
    // The route’s own console lines carry neither, either.
    for (const call of debugSilenced.mock.calls) expect(String(call[0])).not.toContain(KEY);
  });

  it('never forwards to a host other than the configured endpoint, whatever the body or the headers say', async () => {
    const { POST } = createSentinelRouteHandler();
    await POST(request({ headers: { host: 'localhost:3001', 'x-forwarded-host': 'localhost:3001' }, body: JSON.stringify([{ ...ITEM, endpoint: 'http://evil.example' }]) }));
    expect(upstream.calls.map((c) => c.url)).toEqual([`${ENDPOINT}/batch`]);
  });
});

// ---- sentinel-event-enrichment TASK-06 (spec §4.2-§4.5): the browser's IP, derived from the rightmost trusted
// X-Forwarded-For hop, is written into every forwarded item's user.ip_address; `user` is rebuilt from an exact-key
// allow-list; the forward carries the browser's user-agent and x-sentinel-derive-ip: off, and never X-Forwarded-For.

const IP = '198.51.100.23';

async function forwardedItems(xff: string | null, opts: Parameters<typeof createSentinelRouteHandler>[0] = {}, body?: unknown, extra: Record<string, unknown> = {}): Promise<Record<string, unknown>[]> {
  const { POST } = createSentinelRouteHandler({ rateLimit: false, ...opts });
  const req = Object.assign(request({ headers: { 'x-forwarded-for': xff }, ...(body === undefined ? {} : { body: JSON.stringify(body) }) }), extra);
  const res = await POST(req);
  expect(res.status).toBe(202);
  const call = upstream.calls.at(-1);
  return call?.items as Record<string, unknown>[];
}

describe('TASK-06: the client IP derivation, observed in the forwarded body (spec §4.2, §4.5)', () => {
  it('x-forwarded-for: 6.6.6.6, 198.51.100.23 → every item carries user.ip_address 198.51.100.23 — never 6.6.6.6, never the inbound string', async () => {
    const items = await forwardedItems('6.6.6.6, 198.51.100.23', {}, [ITEM, { ...ITEM, event_id: 'b' }, { ...ITEM, event_id: 'c' }]);
    expect(items).toHaveLength(3);
    for (const item of items) expect(item.user).toEqual({ ip_address: IP });
    expect(upstream.calls[0]?.body).not.toContain('6.6.6.6');
  });

  it('trustedProxyHops: 2 over "6.6.6.6, 198.51.100.23, 10.0.0.1" → 198.51.100.23; ", 1.2.3.4" → 1.2.3.4; an out-of-range hop count → no user', async () => {
    expect((await forwardedItems('6.6.6.6, 198.51.100.23, 10.0.0.1', { trustedProxyHops: 2 }))[0]?.user).toEqual({ ip_address: IP });
    expect((await forwardedItems(', 1.2.3.4'))[0]?.user).toEqual({ ip_address: '1.2.3.4' });
    expect((await forwardedItems(IP, { trustedProxyHops: 3 }))[0]).not.toHaveProperty('user');
  });

  it('canonical form: a trailing space is trimmed; ::FFFF:C633:6417 and ::ffff:198.51.100.23 unmap to 198.51.100.23; 2001:DB8::1 → 2001:db8::1', async () => {
    expect((await forwardedItems(`${IP} `))[0]?.user).toEqual({ ip_address: IP });
    expect((await forwardedItems('::FFFF:C633:6417'))[0]?.user).toEqual({ ip_address: IP });
    expect((await forwardedItems('::ffff:198.51.100.23'))[0]?.user).toEqual({ ip_address: IP });
    expect((await forwardedItems('2001:DB8::1'))[0]?.user).toEqual({ ip_address: '2001:db8::1' });
  });

  it('non-IP hops give no user: a hostname, a zone, brackets, a port, and ::1]@6.6.6.6:80/?[ (which must never yield 6.6.6.6)', async () => {
    for (const hop of ['evil.example', 'fe80::1%eth0', '[2001:db8::1]', '2001:db8::1]:80', '::1]@6.6.6.6:80/?[', '198.51.100.23:443', '01.2.3.4', '256.1.1.1']) {
      const items = await forwardedItems(`6.6.6.6, ${hop}`);
      expect(items[0], hop).not.toHaveProperty('user');
    }
    expect(upstream.calls.map((c) => c.body).join('')).not.toContain('6.6.6.6');
  });

  it('trustedProxyHops: 0 with a plain Request (no ip, no socket: the Next App Router shape) → no user on any item, and the shared bucket', async () => {
    const items = await forwardedItems('6.6.6.6', { trustedProxyHops: 0 }, [ITEM, { ...ITEM, event_id: 'b' }]);
    for (const item of items) expect(item).not.toHaveProperty('user');
  });

  it('trustedProxyHops: 0 on a runtime that exposes the socket (NOT the Next path) → the socket address, and the header is still ignored', async () => {
    const items = await forwardedItems('6.6.6.6', { trustedProxyHops: 0 }, undefined, { socket: { remoteAddress: '203.0.113.9' } });
    expect(items[0]?.user).toEqual({ ip_address: '203.0.113.9' });
  });

  it('no header and no socket → no user; a bad trustedProxyHops falls back to 1 with one warn', async () => {
    expect((await forwardedItems(null))[0]).not.toHaveProperty('user');
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const route = await freshRoute();
    for (const bad of [-1, 1.5, NaN, '2' as unknown as number]) route.createSentinelRouteHandler({ trustedProxyHops: bad });
    expect(warn).toHaveBeenCalledTimes(1);
    expect(String(warn.mock.calls[0]?.[0])).toContain('trustedProxyHops');
    const { POST } = route.createSentinelRouteHandler({ trustedProxyHops: -1, rateLimit: false });
    await POST(request({ headers: { 'x-forwarded-for': `6.6.6.6, ${IP}` } }));
    expect((upstream.calls.at(-1)?.items as Record<string, unknown>[])[0]?.user).toEqual({ ip_address: IP });
  });
});

describe('TASK-06: canonicalIp, its IPv6 character guard proven on its own (enr-code-canonicalip-guards-individually-unproven)', () => {
  it('the character pre-filter rejects an entry the WHATWG parser would repair: an embedded tab, LF or CR', () => {
    // new URL() deletes ASCII tab and newline anywhere in its input, so without the filter each of these
    // canonicalises to a clean address its author never wrote.
    for (const hop of ['2001:db8::\t1', '::\n1', '::ffff:198.51.\r100.23']) {
      expect(canonicalIp(hop), JSON.stringify(hop)).toBeNull();
    }
  });

  it('the character pre-filter alone stops ::1]@6.6.6.6:80/?[ from parsing as host 6.6.6.6', () => {
    expect(canonicalIp('::1]@6.6.6.6:80/?[')).toBeNull();
    expect(canonicalIp('::1]@[2001:db8::66]')).toBeNull();
  });

  it('a real IPv6 entry still canonicalises: lower case, and an IPv4-mapped one unmapped', () => {
    expect(canonicalIp(' 2001:DB8:0::1 ')).toBe('2001:db8::1');
    expect(canonicalIp('::FFFF:C633:6417')).toBe('198.51.100.23');
    expect(canonicalIp('::ffff:198.51.100.23')).toBe('198.51.100.23');
  });
});

describe('TASK-06: applyClientIp rebuilds user from the exact-key allow-list (spec §4.4, B1-B5)', () => {
  const run = (user: unknown, ip: string | null): Record<string, unknown> => {
    const item: Record<string, unknown> = { event_id: 'e', platform: 'javascript' };
    if (user !== undefined) item.user = user;
    applyClientIp([item], ip);
    return item;
  };
  const base = { event_id: 'e', platform: 'javascript' };

  it('B2: a browser-sent ip_address is overwritten; id is carried; {{auto}} is replaced', () => {
    expect(run({ ip_address: '6.6.6.6', id: 'u1' }, IP)).toEqual({ ...base, user: { id: 'u1', ip_address: IP } });
    expect(run({ ip_address: '{{auto}}' }, IP)).toEqual({ ...base, user: { ip_address: IP } });
  });

  it('B2: keys Go folds to ip_address never survive — IP_ADDRESS beside ip_address (either filled), Ip_Address, ip_addreſſ', () => {
    expect(run({ ip_address: '6.6.6.6', IP_ADDRESS: '6.6.6.6' }, IP)).toEqual({ ...base, user: { ip_address: IP } });
    expect(run({ ip_address: '', IP_ADDRESS: '6.6.6.6' }, IP)).toEqual({ ...base, user: { ip_address: IP } });
    expect(run({ Ip_Address: '6.6.6.6' }, IP)).toEqual({ ...base, user: { ip_address: IP } });
    expect(run({ 'ip_addreſſ': '6.6.6.6' }, IP)).toEqual({ ...base, user: { ip_address: IP } });
  });

  it('B2: a browser-sent geo is dropped in every spelling', () => {
    expect(run({ id: 'u1', geo: { country_code: 'XX', city: 'Fake' } }, IP)).toEqual({ ...base, user: { id: 'u1', ip_address: IP } });
    expect(run({ id: 'u1', GEO: { country_code: 'XX' } }, IP)).toEqual({ ...base, user: { id: 'u1', ip_address: IP } });
  });

  it('B2: only exact allow-listed names with string values are copied', () => {
    expect(run({ id: 'u1', ID: 'u2', Username: 'n', email: 5, extra: 'x' }, IP)).toEqual({ ...base, user: { id: 'u1', ip_address: IP } });
    expect(JSON.parse('{"__proto__":{"ip_address":"6.6.6.6"},"id":"u1"}')).toHaveProperty('__proto__');
    expect(run(JSON.parse('{"__proto__":{"ip_address":"6.6.6.6"},"id":"u1"}'), IP)).toEqual({ ...base, user: { id: 'u1', ip_address: IP } });
  });

  it('B2: the serialised user keys are in the order id, username, email, ip_address', () => {
    const item = run({ ip_address: '6.6.6.6', email: 'a@b.c', username: 'n', id: 'u1' }, IP);
    expect(JSON.stringify(item.user)).toBe(`{"id":"u1","username":"n","email":"a@b.c","ip_address":"${IP}"}`);
  });

  it('B1: no user, or user: null → { ip_address }; B3: a non-object user is left as it is', () => {
    expect(run(undefined, IP)).toEqual({ ...base, user: { ip_address: IP } });
    expect(run(null, IP)).toEqual({ ...base, user: { ip_address: IP } });
    expect(run('x', IP)).toEqual({ ...base, user: 'x' });
    expect(run(['x'], IP)).toEqual({ ...base, user: ['x'] });
  });

  it('B4: with no IP a plain-object user is rebuilt with no ip_address in any spelling, and deleted entirely when empty; B5: no user stays absent', () => {
    expect(run({ ip_address: '6.6.6.6', id: 'u1' }, null)).toEqual({ ...base, user: { id: 'u1' } });
    expect(run({ Ip_Address: '6.6.6.6' }, null)).toEqual(base);
    expect(run({ 'ip_addreſſ': '6.6.6.6' }, null)).toEqual(base);
    expect(run({ IP_ADDRESS: '6.6.6.6', GEO: {} }, null)).toEqual(base);
    expect(run(undefined, null)).toEqual(base);
    expect(run(null, null)).toEqual({ ...base, user: null });
  });
});

describe('TASK-06: through the route — overwrite, the top-level filter, clientKey (spec §4.5)', () => {
  it('an inbound user.ip_address is never forwarded: with a derivable IP the item carries the derived one; with none, no user at all', async () => {
    // ITEM_KEYS admits `user` (spec §7.1); applyClientIp rebuilds it from USER_KEYS only, so the inbound
    // ip_address never survives, and B4 deletes the empty rebuild when no IP was derived.
    expect((await forwardedItems(IP, {}, [{ ...ITEM, user: { ip_address: '6.6.6.6' } }]))[0]?.user).toEqual({ ip_address: IP });
    expect((await forwardedItems(null, {}, [{ ...ITEM, user: { ip_address: '6.6.6.6' } }]))[0]).not.toHaveProperty('user');
  });

  it('USER (or any key only case-folding to an ITEM_KEYS name) never reaches the forward', async () => {
    const items = await forwardedItems(IP, {}, [{ ...ITEM, user: { ip_address: '6.6.6.6' }, USER: { ip_address: '6.6.6.6' }, Platform: 'x' }]);
    expect(Object.keys(items[0] as object).sort()).toEqual([...Object.keys(ITEM), 'user'].sort());
    expect(upstream.calls.at(-1)?.body).not.toContain('6.6.6.6');
  });

  it('clientKey returning 10.0.0.7 writes 10.0.0.7; one returning a non-IP writes no IP but still keys the bucket on it; a throwing one writes no IP', async () => {
    expect((await forwardedItems('6.6.6.6', { clientKey: () => '10.0.0.7' }))[0]?.user).toEqual({ ip_address: '10.0.0.7' });
    const { POST } = createSentinelRouteHandler({ clientKey: () => 'bucket-a' });
    await POST(request({ headers: { 'x-forwarded-for': IP } }));
    expect((upstream.calls.at(-1)?.items as Record<string, unknown>[])[0]).not.toHaveProperty('user');
    expect(storeOf.get(POST)?.has('bucket-a')).toBe(true);
    vi.spyOn(console, 'debug').mockImplementation(() => undefined);
    expect(
      (
        await forwardedItems(IP, {
          clientKey: () => {
            throw new Error('x');
          },
        })
      )[0],
    ).not.toHaveProperty('user');
  });
});

describe('C1: ITEM_KEYS admits the typed fields (spec §7.1, §7.2)', () => {
  const V2_EXTRAS = {
    level: 'warning',
    sdk: { name: 'sentinel.javascript.browser', version: '0.2.0' },
    tags: { tenant_id: 't1', plan: 'pro' },
    contexts: { app: { version: '1.0' } },
    request: { url: 'https://app.example/x' },
    breadcrumbs: [{ type: 'default', category: 'ui.click', timestamp: '2026-09-16T00:00:00.000Z' }],
  };

  it('each of the six non-user typed keys survives a forward unchanged, deep-equal', async () => {
    const items = await forwardedItems(null, {}, [{ ...ITEM, ...V2_EXTRAS }]);
    const item = items[0] as unknown as typeof V2_EXTRAS;
    expect(item.level).toEqual(V2_EXTRAS.level);
    expect(item.sdk).toEqual(V2_EXTRAS.sdk);
    expect(item.tags).toEqual(V2_EXTRAS.tags);
    expect(item.contexts).toEqual(V2_EXTRAS.contexts);
    expect(item.request).toEqual(V2_EXTRAS.request);
    expect(item.breadcrumbs).toEqual(V2_EXTRAS.breadcrumbs);
  });

  it('not-admitted keys never reach the forward: transaction, server_name, exception, logger, dist, source, project_key, project_id, received_at', async () => {
    const notAdmitted = {
      transaction: 'x',
      server_name: 'x',
      exception: 'x',
      logger: 'x',
      dist: 'x',
      source: 'x',
      project_key: 'pk',
      project_id: 'pid',
      received_at: 'x',
    };
    const items = await forwardedItems(null, {}, [{ ...ITEM, ...notAdmitted }]);
    for (const key of Object.keys(notAdmitted)) expect(items[0]).not.toHaveProperty(key);
  });

  it('case-folded spellings of the new keys are dropped: USER, Tags, Breadcrumbs, uſer', async () => {
    const items = await forwardedItems(null, {}, [
      { ...ITEM, USER: { id: 'x' }, Tags: { a: 'b' }, Breadcrumbs: [{ type: 'x' }], 'uſer': { id: 'y' } },
    ]);
    expect(items[0]).not.toHaveProperty('USER');
    expect(items[0]).not.toHaveProperty('Tags');
    expect(items[0]).not.toHaveProperty('Breadcrumbs');
    expect(items[0]).not.toHaveProperty('uſer');
    expect(items[0]).not.toHaveProperty('user');
    expect(items[0]).not.toHaveProperty('tags');
    expect(items[0]).not.toHaveProperty('breadcrumbs');
  });

  it('a v2 item’s user with {{auto}}, IP_ADDRESS, geo and an extra key forwards as exactly { id, username, email, ip_address: <derived> }', async () => {
    const items = await forwardedItems(IP, {}, [
      { ...ITEM, user: { id: 'u1', username: 'name', email: 'a@b.c', ip_address: '{{auto}}', IP_ADDRESS: '6.6.6.6', geo: { country_code: 'XX' }, extra: 'z' } },
    ]);
    expect(items[0]?.user).toEqual({ id: 'u1', username: 'name', email: 'a@b.c', ip_address: IP });
  });
});

describe('TASK-06: the forward’s header set (spec §4.4)', () => {
  it('headers ⊆ {content-type, x-api-key, x-project-key, user-agent, x-sentinel-derive-ip}; x-forwarded-for never, derived IP or not; the marker off on every forward', async () => {
    const { POST } = createSentinelRouteHandler({ rateLimit: false });
    await POST(request({ headers: { 'x-forwarded-for': `6.6.6.6, ${IP}`, 'user-agent': 'Mozilla/5.0 test' } }));
    await POST(request({ headers: { 'x-forwarded-for': null, 'user-agent': 'Mozilla/5.0 test', 'x-sentinel-derive-ip': 'on' } }));
    await POST(request({ headers: { 'x-forwarded-for': 'evil.example', 'user-agent': null } }));
    const allowed = ['content-type', 'user-agent', 'x-api-key', 'x-project-key', 'x-sentinel-derive-ip'];
    for (const call of upstream.calls) {
      for (const name of Object.keys(call.headers)) expect(allowed).toContain(name);
      expect(call.headers).not.toHaveProperty('x-forwarded-for');
      expect(call.headers['x-sentinel-derive-ip']).toBe('off');
    }
    expect(upstream.calls[0]?.headers['user-agent']).toBe('Mozilla/5.0 test');
    expect(upstream.calls[1]?.headers['user-agent']).toBe('Mozilla/5.0 test');
    expect(upstream.calls[2]?.headers).not.toHaveProperty('user-agent');
  });

  it('a 1025-byte user-agent is not forwarded; 1024 bytes is', async () => {
    const { POST } = createSentinelRouteHandler({ rateLimit: false });
    await POST(request({ headers: { 'user-agent': 'a'.repeat(1025) } }));
    await POST(request({ headers: { 'user-agent': 'a'.repeat(1024) } }));
    expect(upstream.calls[0]?.headers).not.toHaveProperty('user-agent');
    expect(upstream.calls[1]?.headers['user-agent']).toBe('a'.repeat(1024));
  });
});
