// @vitest-environment jsdom
//
// Trace propagation (spec *Trace propagation*, cross-repo-trace-propagation D4): the page trace, `traceparent` on same-origin
// fetch/XHR only by default, `tracePropagationTargets`, a caller's header kept, a second wrapper before and
// after, a new trace per navigation, and events carrying the page trace.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { captureException, close, init } from '../../src/browser.js';
import { getInternalClient } from '../../src/core/state.js';
import type { SentinelEvent } from '../../src/core/types.js';
import { resetPipeline } from '../helpers/pipeline.js';

const TP = /^00-([0-9a-f]{32})-([0-9a-f]{16})-01$/;

interface Seen {
  url: string;
  headers: Record<string, string>;
}
let seen: Seen[] = [];

function lower(h: unknown): Record<string, string> {
  const out: Record<string, string> = {};
  if (typeof (h as { forEach?: unknown } | undefined)?.forEach === 'function' && !Array.isArray(h)) (h as Headers).forEach((v, k) => (out[k.toLowerCase()] = v));
  else if (Array.isArray(h)) for (const [k, v] of h) out[String(k).toLowerCase()] = String(v);
  else if (h !== undefined && h !== null) for (const [k, v] of Object.entries(h)) out[k.toLowerCase()] = String(v);
  return out;
}

function installBaseFetch(): void {
  (globalThis as { fetch?: unknown }).fetch = (input: unknown, init?: { headers?: unknown }) => {
    const req = input as { url?: string; headers?: unknown };
    seen.push({ url: typeof req.url === 'string' ? req.url : String(input), headers: lower(init?.headers ?? req.headers) });
    return Promise.resolve(new Response('ok', { status: 200 }));
  };
}

class FakeXhr {
  static sent: Array<{ url: string; headers: Record<string, string> }> = [];
  url = '';
  headers: Record<string, string> = {};
  open(_m: string, url: string): void {
    this.url = url;
    this.headers = {};
  }
  setRequestHeader(n: string, v: string): void {
    this.headers[n.toLowerCase()] = v;
  }
  send(): void {
    FakeXhr.sent.push({ url: this.url, headers: { ...this.headers } });
  }
  addEventListener(): void {}
  removeEventListener(): void {}
}

function queued(): SentinelEvent[] {
  return (getInternalClient()?.queue ?? []).map((j) => JSON.parse(j) as SentinelEvent);
}

beforeEach(() => {
  resetPipeline();
  seen = [];
  FakeXhr.sent = [];
  history.replaceState({}, '', '/start');
  installBaseFetch();
  (globalThis as { XMLHttpRequest?: unknown }).XMLHttpRequest = FakeXhr as unknown as typeof XMLHttpRequest;
});

afterEach(async () => {
  await close(10);
  resetPipeline();
});

describe('fetch', () => {
  it('adds traceparent to a same-origin request and not to a cross-origin one; no tracestate/baggage/sentry-trace', async () => {
    init({ tunnel: '/api/sentinel' });
    await fetch('/api/orders');
    await fetch(`${location.origin}/api/abs`);
    await fetch('https://third-party.example/x');
    expect(seen[0]?.headers.traceparent).toMatch(TP);
    expect(seen[1]?.headers.traceparent).toMatch(TP);
    expect(seen[2]?.headers.traceparent).toBeUndefined();
    for (const s of seen) {
      expect(s.headers.tracestate).toBeUndefined();
      expect(s.headers.baggage).toBeUndefined();
      expect(s.headers['sentry-trace']).toBeUndefined();
    }
  });

  it('one trace id per page, a fresh span id per request', async () => {
    init({ tunnel: '/api/sentinel' });
    await fetch('/a');
    await fetch('/b');
    const [a, b] = seen.map((s) => TP.exec(s.headers.traceparent ?? '') as RegExpExecArray);
    expect(a?.[1]).toBe(b?.[1]);
    expect(a?.[2]).not.toBe(b?.[2]);
  });

  it("never overwrites the caller's traceparent (any case, any headers shape, or a Request)", async () => {
    init({ tunnel: '/api/sentinel' });
    await fetch('/a', { headers: { TraceParent: 'mine-1' } });
    await fetch('/b', { headers: new Headers({ traceparent: 'mine-2' }) });
    await fetch('/c', { headers: [['traceparent', 'mine-3']] });
    await fetch(new Request(`${location.origin}/d`, { headers: { traceparent: 'mine-4' } }));
    expect(seen.map((s) => s.headers.traceparent)).toEqual(['mine-1', 'mine-2', 'mine-3', 'mine-4']);
  });

  it("keeps the caller's other headers and a Request's own headers when it adds one", async () => {
    init({ tunnel: '/api/sentinel' });
    await fetch('/a', { headers: { 'X-One': '1' } });
    await fetch(new Request(`${location.origin}/b`, { headers: { 'X-Two': '2' } }));
    expect(seen[0]?.headers['x-one']).toBe('1');
    expect(seen[0]?.headers.traceparent).toMatch(TP);
    expect(seen[1]?.headers['x-two']).toBe('2');
    expect(seen[1]?.headers.traceparent).toMatch(TP);
  });

  it('never decorates the SDK’s own tunnel send', async () => {
    init({ tunnel: '/api/sentinel' });
    await fetch('/api/sentinel', { method: 'POST' });
    expect(seen[0]?.headers.traceparent).toBeUndefined();
  });

  it('propagates regardless of breadcrumbs: false', async () => {
    init({ tunnel: '/api/sentinel', breadcrumbs: false });
    await fetch('/a');
    expect(seen[0]?.headers.traceparent).toMatch(TP);
  });

  it('works with another SDK wrapping fetch BEFORE init (inner) and AFTER init (outer)', async () => {
    const inner: Seen[] = [];
    const base = globalThis.fetch;
    (globalThis as { fetch?: unknown }).fetch = (i: unknown, init?: { headers?: unknown }) => {
      inner.push({ url: String(i), headers: lower(init?.headers) });
      return (base as (...a: unknown[]) => Promise<Response>)(i, init);
    };
    init({ tunnel: '/api/sentinel' });
    const sdkFetch = globalThis.fetch;
    (globalThis as { fetch?: unknown }).fetch = (i: unknown, init?: { headers?: unknown }) => sdkFetch(i as string, { ...init, headers: { ...lower(init?.headers), 'sentry-trace': 'x-y' } } as RequestInit);
    await fetch('/a');
    expect(inner[0]?.headers.traceparent).toMatch(TP);
    expect(seen[0]?.headers.traceparent).toMatch(TP);
    expect(seen[0]?.headers['sentry-trace']).toBe('x-y');
  });

  it('close() restores fetch', async () => {
    const before = globalThis.fetch;
    init({ tunnel: '/api/sentinel' });
    expect(globalThis.fetch).not.toBe(before);
    await close(10);
    expect(globalThis.fetch).toBe(before);
  });
});

describe('tracePropagationTargets', () => {
  it('a string is a prefix of the absolute URL; "/x" means same-origin /x…; the option replaces the default', async () => {
    init({ tunnel: '/api/sentinel', tracePropagationTargets: ['https://api.example.com/v1', '/graphql'] });
    await fetch('https://api.example.com/v1/orders');
    await fetch('https://api.example.com/v2/orders');
    await fetch('/graphql?x=1');
    await fetch('/other');
    expect(seen.map((s) => s.headers.traceparent !== undefined)).toEqual([true, false, true, false]);
  });

  it('a RegExp is tested against the absolute URL, repeatedly (a /g flag does not skip)', async () => {
    init({ tunnel: '/api/sentinel', tracePropagationTargets: [/^https:\/\/api\.example\.com\//g] });
    await fetch('https://api.example.com/a');
    await fetch('https://api.example.com/b');
    await fetch('/c');
    expect(seen.map((s) => s.headers.traceparent !== undefined)).toEqual([true, true, false]);
  });

  it('[] sends nowhere; a non-array reports invalid_option and keeps the default', async () => {
    const errors: string[] = [];
    init({ tunnel: '/api/sentinel', tracePropagationTargets: [], onError: (e) => errors.push(e.code) });
    await fetch('/a');
    expect(seen[0]?.headers.traceparent).toBeUndefined();
    await close(10);
    resetPipeline();
    init({ tunnel: '/api/sentinel', tracePropagationTargets: 'nope' as never, onError: (e) => errors.push(e.code) });
    await fetch('/b');
    expect(seen[1]?.headers.traceparent).toMatch(TP);
    expect(errors).toContain('invalid_option');
  });
});

describe('XHR', () => {
  function xhr(url: string, pre?: (x: XMLHttpRequest) => void): Record<string, string> {
    const x = new XMLHttpRequest();
    x.open('GET', url);
    pre?.(x);
    x.send();
    return FakeXhr.sent[FakeXhr.sent.length - 1]?.headers ?? {};
  }

  it('same-origin gets traceparent (same trace id as fetch), cross-origin does not, caller header kept', () => {
    init({ tunnel: '/api/sentinel' });
    const same = xhr('/api/x');
    const cross = xhr('https://third-party.example/x');
    const own = xhr('/api/y', (x) => x.setRequestHeader('TraceParent', 'mine'));
    expect(same.traceparent).toMatch(TP);
    expect(cross.traceparent).toBeUndefined();
    expect(own.traceparent).toBe('mine');
    for (const h of [same, cross, own]) {
      expect(h.tracestate).toBeUndefined();
      expect(h.baggage).toBeUndefined();
      expect(h['sentry-trace']).toBeUndefined();
    }
  });

  it('works under a second XHR wrapper installed before and after init', () => {
    const proto = XMLHttpRequest.prototype as unknown as Record<string, (...a: unknown[]) => unknown>;
    const calls: string[] = [];
    const wrap = (name: string): void => {
      const orig = proto[name] as (...a: unknown[]) => unknown;
      proto[name] = function (this: unknown, ...a: unknown[]) {
        calls.push(name);
        return orig.apply(this, a);
      };
    };
    wrap('send'); // before init (inner)
    init({ tunnel: '/api/sentinel' });
    wrap('send'); // after init (outer)
    const h = xhr('/api/x');
    expect(h.traceparent).toMatch(TP);
    expect(calls).toEqual(['send', 'send']);
  });
});

describe('the page trace', () => {
  it('an event without an explicit traceparent carries the page trace id, a span id and flags 1', () => {
    init({ tunnel: '/api/sentinel' });
    captureException(new Error('boom'));
    const e = queued()[0] as SentinelEvent;
    expect(e.trace_id).toMatch(/^[0-9a-f]{32}$/);
    expect(e.span_id).toMatch(/^[0-9a-f]{16}$/);
    expect(e.trace_flags).toBe(1);
  });

  it('the event trace id equals the id the request carried', async () => {
    init({ tunnel: '/api/sentinel' });
    await fetch('/api/x');
    captureException(new Error('boom'));
    expect(TP.exec(seen[0]?.headers.traceparent ?? '')?.[1]).toBe(queued()[0]?.trace_id);
  });

  it('an explicit ctx.traceparent wins', () => {
    init({ tunnel: '/api/sentinel' });
    captureException(new Error('boom'), { traceparent: '00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01' });
    expect(queued()[0]?.trace_id).toBe('4bf92f3577b34da6a3ce929d0e0e4736');
  });

  it('a new trace on pushState / replaceState to a new pathname and on popstate; not for query/hash-only changes', async () => {
    init({ tunnel: '/api/sentinel' });
    const id = (): string => {
      captureException(new Error('x'));
      const q = queued();
      return q[q.length - 1]?.trace_id as string;
    };
    const t0 = id();
    history.pushState({}, '', '/start?tab=2');
    history.replaceState({}, '', '/start#frag');
    expect(id()).toBe(t0);
    history.pushState({}, '', '/next');
    const t1 = id();
    expect(t1).not.toBe(t0);
    history.replaceState({}, '', '/replaced');
    const t2 = id();
    expect(t2).not.toBe(t1);
    history.pushState({}, '', '/third');
    id();
    history.replaceState({}, '', '/third'); // jsdom: go back to a different pathname, then popstate
    history.back();
    await new Promise((r) => setTimeout(r, 50));
    const t4 = id();
    expect(new Set([t0, t1, t2, t4]).size).toBe(4);
  });

  it('a request after navigation carries the new trace id', async () => {
    init({ tunnel: '/api/sentinel' });
    await fetch('/a');
    history.pushState({}, '', '/next');
    await fetch('/b');
    expect(TP.exec(seen[0]?.headers.traceparent ?? '')?.[1]).not.toBe(TP.exec(seen[1]?.headers.traceparent ?? '')?.[1]);
  });

  it('close() leaves no page trace on a later event', async () => {
    init({ tunnel: '/api/sentinel' });
    const client = getInternalClient();
    await close(10);
    expect(client?.pageTrace).toBeUndefined();
  });
});

// Every call below goes through a REAL Request (Node/undici), the way the platform would send it, so what the
// wrapper does to method / body / credentials / referrerPolicy / headers is observed, not assumed.
interface Sent {
  url: string;
  method: string;
  body: string;
  credentials: string;
  referrerPolicy: string;
  headers: Record<string, string>;
}
let sent: Sent[] = [];

function installRealRequestFetch(): void {
  (globalThis as { fetch?: unknown }).fetch = async (input: unknown, init?: RequestInit) => {
    const base = typeof document === 'undefined' ? location.href : document.baseURI;
    const resolved = typeof input === 'string' ? new URL(input, base).href : input;
    // A Request with no init is read as it is (undici's own `new Request(req)` drops referrerPolicy, which would
    // hide exactly what these tests look for); anything else is built the way `fetch` builds it.
    const req = input instanceof Request && init === undefined ? input : new Request(resolved as RequestInfo, init);
    sent.push({
      url: req.url,
      method: req.method,
      body: await req.clone().text(),
      credentials: req.credentials,
      referrerPolicy: req.referrerPolicy,
      headers: lower(req.headers),
    });
    return new Response('ok');
  };
}

describe('through a real Request', () => {
  beforeEach(() => {
    sent = [];
    installRealRequestFetch();
  });

  it('a Request input keeps method, body, credentials and referrerPolicy, and gains traceparent', async () => {
    init({ tunnel: '/api/sentinel' });
    const r = new Request(`${location.origin}/api/proxy/x`, { method: 'POST', body: 'hello', credentials: 'include', referrerPolicy: 'no-referrer', headers: { 'X-Keep': '1' } });
    await fetch(r);
    expect(sent[0]).toMatchObject({ method: 'POST', body: 'hello', credentials: 'include', referrerPolicy: 'no-referrer' });
    expect(sent[0]?.headers['x-keep']).toBe('1');
    expect(sent[0]?.headers.traceparent).toMatch(TP);
  });

  it("does not mutate the caller's Request", async () => {
    init({ tunnel: '/api/sentinel' });
    const r = new Request(`${location.origin}/api/x`, { method: 'POST', body: 'b' });
    await fetch(r);
    expect(r.headers.has('traceparent')).toBe(false);
  });

  it('an init that is not a plain object (a Request as init) goes out untouched — method and body intact', async () => {
    init({ tunnel: '/api/sentinel' });
    // jsdom and undici disagree on AbortSignal, so the stub records what it was handed instead of re-wrapping it.
    let received: unknown;
    const platform = (_i: unknown, i?: unknown): Promise<Response> => {
      received = i;
      return Promise.resolve(new Response('ok'));
    };
    (globalThis as { fetch?: unknown }).fetch = platform;
    await close(10);
    init({ tunnel: '/api/sentinel' });
    const asInit = new Request(`${location.origin}/other`, { method: 'POST', body: 'payload' });
    await fetch('/z', asInit as unknown as RequestInit);
    expect(received).toBe(asInit); // the very object, not a spread copy that would drop method and body
  });

  it('a plain init keeps its method, body and credentials and gains traceparent', async () => {
    init({ tunnel: '/api/sentinel' });
    await fetch('/z', { method: 'PUT', body: 'p', credentials: 'include', referrerPolicy: 'no-referrer', headers: { 'X-A': '1' } });
    expect(sent[0]).toMatchObject({ method: 'PUT', body: 'p', credentials: 'include', referrerPolicy: 'no-referrer' });
    expect(sent[0]?.headers['x-a']).toBe('1');
    expect(sent[0]?.headers.traceparent).toMatch(TP);
  });

  it("init.headers REPLACES a Request's headers: the Request's own traceparent does not block injection, but init's does", async () => {
    init({ tunnel: '/api/sentinel' });
    const withTp = (): Request => new Request(`${location.origin}/a`, { headers: { traceparent: 'req-own' } });
    await fetch(withTp(), { headers: { 'x-b': '2' } });
    expect(sent[0]?.headers.traceparent).toMatch(TP);
    expect(sent[0]?.headers['x-b']).toBe('2');
    await fetch(withTp(), { headers: { traceparent: 'init-own' } });
    expect(sent[1]?.headers.traceparent).toBe('init-own');
    await fetch(withTp());
    expect(sent[2]?.headers.traceparent).toBe('req-own');
  });
});

describe('string targets match origin exactly, then path segments', () => {
  const run = async (targets: unknown[], urls: string[]): Promise<boolean[]> => {
    seen = [];
    init({ tunnel: '/api/sentinel', tracePropagationTargets: targets as never });
    for (const u of urls) await fetch(u);
    const out = seen.map((s) => s.headers.traceparent !== undefined);
    await close(10);
    resetPipeline();
    installBaseFetch();
    return out;
  };

  it('an absolute target does not match a longer host or userinfo trick', async () => {
    expect(
      await run(
        ['https://api.example.com'],
        ['https://api.example.com/x', 'https://api.example.com.evil.com/x', 'https://api.example.com@evil.com/x', 'http://api.example.com/x', 'https://api.example.com:8443/x'],
      ),
    ).toEqual([true, false, false, false, false]);
  });

  it('a path target is segment-aware and on the page origin: /api matches /api and /api/x, not /apiary', async () => {
    expect(await run(['/api'], ['/api', '/api/x', '/apiary', `${location.origin}/api/y`, 'https://evil.example/api/x'])).toEqual([true, true, false, true, false]);
    expect(await run(['https://api.example.com/v1/'], ['https://api.example.com/v1/a', 'https://api.example.com/v1', 'https://api.example.com/v10'])).toEqual([true, false, false]);
  });

  it("a relative target without a leading '/' is rejected at init with invalid_option", async () => {
    const errors: string[] = [];
    init({ tunnel: '/api/sentinel', tracePropagationTargets: ['api/'], onError: (e) => errors.push(e.code) });
    await fetch('/api/x');
    expect(errors).toContain('invalid_option');
    expect(seen[0]?.headers.traceparent).toBeUndefined();
  });
});

describe('an absent traceparent', () => {
  const cases: Array<[string, unknown]> = [['undefined', undefined], ['null', null], ["''", '']];
  for (const [name, value] of cases) {
    it(`${name} is absent: the event carries the page trace`, () => {
      init({ tunnel: '/api/sentinel' });
      captureException(new Error('x'), { traceparent: value as never });
      expect(queued()[0]?.trace_id).toMatch(/^[0-9a-f]{32}$/);
    });
  }

  it('a non-empty malformed traceparent yields no trace', () => {
    init({ tunnel: '/api/sentinel' });
    captureException(new Error('x'), { traceparent: 'nonsense' });
    expect(queued()[0]?.trace_id).toBeUndefined();
  });
});

describe('<base href>', () => {
  afterEach(() => {
    document.head.querySelectorAll('base').forEach((b) => b.remove());
  });

  it('relative URLs resolve against document.baseURI; same-origin is still the PAGE origin', async () => {
    const base = document.createElement('base');
    base.href = 'https://cdn.other.example/';
    document.head.appendChild(base);
    installRealRequestFetch();
    sent = [];
    init({ tunnel: '/api/sentinel' });
    await fetch('/a'); // goes to cdn.other.example — cross-origin for the page
    expect(sent[0]?.url).toContain('cdn.other.example');
    expect(sent[0]?.headers.traceparent).toBeUndefined();
    await fetch(`${location.origin}/b`);
    expect(sent[1]?.headers.traceparent).toMatch(TP);
  });
});

// sentinel-sdkjs-request-clone-unread BUG-01: adding traceparent must consume the caller's Request body exactly as a
// plain fetch(req) would, not leave it unread behind a clone.
describe('a Request with a body is consumed, not cloned', () => {
  beforeEach(() => {
    sent = [];
    installRealRequestFetch();
  });

  it('bodyUsed is true after the SDK fetch, and a second fetch rejects', async () => {
    init({ tunnel: '/api/sentinel' });
    const r = new Request(`${location.origin}/api/x`, { method: 'POST', body: 'b' });
    await fetch(r);
    expect(sent[0]?.body).toBe('b');
    expect(sent[0]?.headers.traceparent).toMatch(TP);
    expect(r.bodyUsed).toBe(true);
    await expect(fetch(r)).rejects.toThrow(TypeError);
  });

  it('a streaming body is sent with traceparent and the original stream is not left unread', async () => {
    init({ tunnel: '/api/sentinel' });
    const stream = new ReadableStream<Uint8Array>({
      start(c) {
        c.enqueue(new TextEncoder().encode('streamed'));
        c.close();
      },
    });
    const r = new Request(`${location.origin}/api/up`, { method: 'POST', body: stream, duplex: 'half' } as RequestInit);
    await fetch(r);
    expect(sent[0]?.body).toBe('streamed');
    expect(sent[0]?.headers.traceparent).toMatch(TP);
    expect(r.bodyUsed).toBe(true);
    // Read to the end by the sent request (a tee'd, abandoned original would still be locked by the tee):
    // the stream is free again and has nothing left in it.
    expect(r.body?.locked).toBe(false);
    expect((await r.body?.getReader().read())?.done).toBe(true);
  });
});
