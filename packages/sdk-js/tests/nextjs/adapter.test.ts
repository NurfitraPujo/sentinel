// The Next.js adapter (contract §4.7): `captureRequestError` on the server build (N3 — the metadata keys, the
// trace ids with the hex flags, no `metadata.source`, `undefined` returned synchronously, a broken `headers`
// object not throwing), on the edge build (N2's adapter half — the send's promise, awaited by Next, resolving on
// the first attempt's outcome and never rejecting; two same-tick captures in one POST), and on the client build
// (a no-op); the router hook under both names as a no-op on every build (B20); the error-boundary shape
// (`contexts.react.componentStack` → `metadata.react_component_stack`) through the client build; and the export
// shape of the built entries (decision 31, D7). `fetch` is mocked; nothing here touches the network.
// Shuffle-safe: every test starts from a deleted slot with the previous client closed.

import { existsSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getInternalClient } from '../../src/core/state.js';
import type { SentinelEvent } from '../../src/core/types.js';
import * as client from '../../src/nextjs/client.js';
import * as edge from '../../src/nextjs/edge.js';
import * as server from '../../src/nextjs/server.js';
import type { InstrumentationOnRequestError, RequestErrorContext, RequestErrorRequest } from '../../src/nextjs/request-error.js';
import { mockFetch } from '../helpers/fetch-mock.js';
import { resetPipeline } from '../helpers/pipeline.js';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
const ENDPOINT = 'http://ingest.example/ingest';
const TRACE_ID = '4bf92f3577b34da6a3ce929d0e0e4736';
const SPAN_ID = '00f067aa0ba902b7';

const REQUEST: RequestErrorRequest = { path: '/chats', method: 'GET', headers: { traceparent: `00-${TRACE_ID}-${SPAN_ID}-01` } };
const CONTEXT: Readonly<RequestErrorContext> = { routerKind: 'App Router', routePath: '/chats', routeType: 'render', revalidateReason: undefined };

function queued(): SentinelEvent[] {
  return (getInternalClient()?.queue ?? []).map((json) => JSON.parse(json) as SentinelEvent);
}

/** Resolves `'resolved'` when `p` settles before `ms`, else `'pending'` — the promise is never rejected. */
async function settledWithin(p: Promise<unknown>, ms: number): Promise<'resolved' | 'pending'> {
  return Promise.race([p.then(() => 'resolved' as const), new Promise<'pending'>((r) => setTimeout(() => r('pending'), ms))]);
}

beforeEach(() => {
  resetPipeline();
});

afterEach(async () => {
  await server.close(10);
  resetPipeline();
});

describe('N3: captureRequestError on the server build (@sentinel/sdk-js/nextjs/server)', () => {
  it('N3: metadata.http_method / http_url / route / router_kind / route_type from Next’s three arguments, no metadata.source, and undefined returned synchronously', () => {
    mockFetch();
    server.init({ endpoint: ENDPOINT, apiKey: 'k', batchWaitMs: 60_000 });
    const returned = server.captureRequestError(new Error('render failed'), REQUEST, CONTEXT);
    expect(returned).toBeUndefined();
    const [event] = queued();
    expect(event).toBeDefined();
    expect(event?.error_class).toBe('Error');
    expect(event?.message).toBe('render failed');
    expect(event?.metadata).toMatchObject({
      http_method: 'GET',
      http_url: '/chats',
      route: '/chats',
      router_kind: 'App Router',
      route_type: 'render',
      runtime: 'node',
    });
    expect(event?.metadata).not.toHaveProperty('source');
    expect(event?.metadata).not.toHaveProperty('revalidate_reason');
    expect(event?.metadata).not.toHaveProperty('render_source');
  });

  it('N3: render_source and revalidate_reason are carried when Next sets them', () => {
    mockFetch();
    server.init({ endpoint: ENDPOINT, apiKey: 'k', batchWaitMs: 60_000 });
    server.captureRequestError(new Error('x'), REQUEST, { routerKind: 'Pages Router', routePath: '/p', routeType: 'route', renderSource: 'server-rendering', revalidateReason: 'stale' });
    const [event] = queued();
    expect(event?.metadata).toMatchObject({ router_kind: 'Pages Router', route: '/p', route_type: 'route', render_source: 'server-rendering', revalidate_reason: 'stale' });
  });

  it('N3: trace ids parsed from headers.traceparent — flags 01 → trace_flags 1 (hex radix, contract §4.3)', () => {
    mockFetch();
    server.init({ endpoint: ENDPOINT, apiKey: 'k', batchWaitMs: 60_000 });
    server.captureRequestError(new Error('a'), REQUEST, CONTEXT);
    const [event] = queued();
    expect(event?.trace_id).toBe(TRACE_ID);
    expect(event?.span_id).toBe(SPAN_ID);
    expect(event?.trace_flags).toBe(1);
  });

  it('N3: flags 10 → trace_flags 16, and an array-valued traceparent header takes its first element', () => {
    mockFetch();
    server.init({ endpoint: ENDPOINT, apiKey: 'k', batchWaitMs: 60_000 });
    server.captureRequestError(new Error('a'), { ...REQUEST, headers: { traceparent: [`00-${TRACE_ID}-${SPAN_ID}-10`, 'ignored'] } }, CONTEXT);
    const [event] = queued();
    expect(event?.trace_id).toBe(TRACE_ID);
    expect(event?.trace_flags).toBe(16);
  });

  it('N3: a broken headers object (a Proxy whose every read throws), a null request and a missing context do not throw — the error is still captured', () => {
    mockFetch();
    server.init({ endpoint: ENDPOINT, apiKey: 'k', batchWaitMs: 60_000 });
    const broken = new Proxy({}, { get: () => { throw new Error('boom'); }, has: () => { throw new Error('boom'); }, ownKeys: () => { throw new Error('boom'); } });
    expect(() => server.captureRequestError(new Error('one'), { path: '/a', method: 'POST', headers: broken as RequestErrorRequest['headers'] }, CONTEXT)).not.toThrow();
    expect(() => server.captureRequestError(new Error('two'), broken as unknown as RequestErrorRequest, broken as unknown as RequestErrorContext)).not.toThrow();
    expect(() => server.captureRequestError(new Error('three'), null as unknown as RequestErrorRequest, undefined as unknown as RequestErrorContext)).not.toThrow();
    const events = queued();
    expect(events.map((e) => e.message)).toEqual(['one', 'two', 'three']);
    expect(events[0]?.metadata).toMatchObject({ http_method: 'POST', http_url: '/a', route: '/chats' });
    expect(events[0]?.trace_id).toBeUndefined();
    expect(events[1]?.metadata).not.toHaveProperty('http_method');
  });

  it('N3: before init the call returns undefined and throws nothing (the event vanishes)', () => {
    expect(() => server.captureRequestError(new Error('early'), REQUEST, CONTEXT)).not.toThrow();
    expect(server.captureRequestError(new Error('early'), REQUEST, CONTEXT)).toBeUndefined();
    expect(getInternalClient()).toBeUndefined();
  });

  it('N3 / decision 31: the three builds’ captureRequestError all satisfy Next’s InstrumentationOnRequestError signature (a type-level assertion that compiles)', () => {
    const onServer: InstrumentationOnRequestError = server.captureRequestError;
    const onEdge: InstrumentationOnRequestError = edge.captureRequestError;
    const onClient: InstrumentationOnRequestError = client.captureRequestError;
    expect([onServer, onEdge, onClient].every((f) => typeof f === 'function')).toBe(true);
  });
});

describe('N2 (adapter half): captureRequestError on the edge build (@sentinel/sdk-js/nextjs/edge) returns the send’s promise', () => {
  it('N2: returns a Promise that Next can await — resolved once the POST carrying the event has been answered, with metadata.runtime edge, route_type middleware and no metadata.source', async () => {
    const fetched = mockFetch();
    edge.init({ endpoint: ENDPOINT, apiKey: 'k' });
    // The ingestor answers on a timer, so "resolved" can only mean "after the response arrived".
    let answer: (() => void) | undefined;
    vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
      fetched.posts.push({ n: 1, url: String(input), method: init?.method ?? 'GET', headers: Object.fromEntries(Object.entries((init?.headers as Record<string, string>) ?? {}).map(([k, v]) => [k.toLowerCase(), v])), body: String(init?.body), events: JSON.parse(String(init?.body)) as SentinelEvent[], aborted: false });
      return new Promise<Response>((resolve) => {
        answer = () => resolve(new Response('{"ingested":1,"failed":0}', { status: 202 }));
      });
    });
    const returned = edge.captureRequestError(new Error('middleware failed'), { path: '/x', method: 'GET', headers: {} }, { ...CONTEXT, routeType: 'middleware', routePath: '/x' });
    expect(returned).toBeInstanceOf(Promise);
    // Nothing has been posted synchronously: the edge drains on the next microtask (contract §4.4 *Edge*).
    expect(fetched.posts).toHaveLength(0);
    expect(await settledWithin(returned, 30)).toBe('pending');
    expect(fetched.posts).toHaveLength(1);
    expect(answer).toBeDefined();
    answer?.();
    expect(await settledWithin(returned, 200)).toBe('resolved');
    const [event] = fetched.posts[0]?.events ?? [];
    expect(event?.metadata).toMatchObject({ runtime: 'edge', route_type: 'middleware', route: '/x', http_method: 'GET', http_url: '/x' });
    expect(event?.metadata).not.toHaveProperty('source');
    expect(fetched.posts[0]?.headers['x-api-key']).toBe('k');
  });

  it('N2: two captureRequestError calls in one tick go out in ONE POST with a two-element array, and both promises resolve on it', async () => {
    const fetched = mockFetch();
    edge.init({ endpoint: ENDPOINT, apiKey: 'k' });
    const a = edge.captureRequestError(new Error('a'), REQUEST, CONTEXT);
    const b = edge.captureRequestError(new Error('b'), REQUEST, CONTEXT);
    await Promise.all([a, b]);
    expect(fetched.posts).toHaveLength(1);
    expect(fetched.posts[0]?.events.map((e) => e.message)).toEqual(['a', 'b']);
  });

  it('N2: the promise never rejects — a failed POST (network error) resolves it after the first attempt, the retry left to a timer', async () => {
    const fetched = mockFetch();
    vi.spyOn(globalThis, 'fetch').mockImplementation(() => Promise.reject(new TypeError('fetch failed')));
    edge.init({ endpoint: ENDPOINT, apiKey: 'k' });
    const returned = edge.captureRequestError(new Error('down'), REQUEST, CONTEXT);
    await expect(returned).resolves.toBeUndefined();
    expect(fetched.posts).toHaveLength(0);
    const transport = getInternalClient()?.transport;
    expect(transport?.stats().phase).toBe('sleeping');
    expect(transport?.stats().attempts).toBe(1);
  });

  it('N2: the promise is held only for the one POST — a fetch that never answers resolves it once the per-attempt timeout aborts the attempt, never before', async () => {
    vi.useFakeTimers();
    try {
      const fetched = mockFetch();
      fetched.hold();
      edge.init({ endpoint: ENDPOINT, apiKey: 'k' });
      let settled = false;
      const returned = edge.captureRequestError(new Error('slow'), REQUEST, CONTEXT).then(() => {
        settled = true;
      });
      await vi.advanceTimersByTimeAsync(4_900);
      expect(fetched.posts).toHaveLength(1);
      expect(settled).toBe(false);
      await vi.advanceTimersByTimeAsync(200);
      await returned;
      expect(settled).toBe(true);
      expect(fetched.posts[0]?.aborted).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  it('N2: with no client (before init) and with the no-op client (no endpoint) the promise resolves at once and nothing is posted', async () => {
    const fetched = mockFetch();
    expect(await settledWithin(edge.captureRequestError(new Error('early'), REQUEST, CONTEXT), 50)).toBe('resolved');
    edge.init({ apiKey: 'k' });
    expect(await settledWithin(edge.captureRequestError(new Error('unconfigured'), REQUEST, CONTEXT), 50)).toBe('resolved');
    expect(fetched.posts).toHaveLength(0);
  });

  it('N2: a broken headers object does not throw on the edge either, and the returned promise still resolves', async () => {
    const fetched = mockFetch();
    edge.init({ endpoint: ENDPOINT, apiKey: 'k' });
    const broken = new Proxy({}, { get: () => { throw new Error('boom'); } });
    let returned: Promise<void> | undefined;
    expect(() => {
      returned = edge.captureRequestError(new Error('x'), broken as unknown as RequestErrorRequest, broken as unknown as RequestErrorContext);
    }).not.toThrow();
    await returned;
    expect(fetched.posts).toHaveLength(1);
  });
});

describe('the client build (@sentinel/sdk-js/nextjs/client)', () => {
  it('captureRequestError exists as a no-op so instrumentation.ts type-checks in every bundle: returns undefined, throws nothing, captures nothing', () => {
    expect(client.captureRequestError(new Error('x'), REQUEST, CONTEXT)).toBeUndefined();
    expect(getInternalClient()).toBeUndefined();
  });

  it('captureException(error, { contexts: { react: { componentStack } } }) → metadata.react_component_stack (the error-boundary helper, §4.7)', () => {
    // Under node the browser init degrades to the no-op client; the pipeline is exercised through the server build’s
    // client so the queue can be read — the capture call is the same core export on every build.
    mockFetch();
    server.init({ endpoint: ENDPOINT, apiKey: 'k', batchWaitMs: 60_000 });
    client.captureException(new Error('boundary'), { contexts: { react: { componentStack: '\n    at Page\n    at Layout' } } });
    const [event] = queued();
    expect(event?.metadata.react_component_stack).toBe('\n    at Page\n    at Layout');
  });
});

describe('B20: onRouterTransitionStart / captureRouterTransitionStart', () => {
  it.each([
    ['client', client],
    ['server', server],
    ['edge', edge],
  ] as const)('B20: on the %s build both names are the same no-op — undefined returned, nothing thrown, nothing captured', (_name, build) => {
    expect(typeof build.onRouterTransitionStart).toBe('function');
    expect(typeof build.captureRouterTransitionStart).toBe('function');
    expect(build.onRouterTransitionStart('/chats', 'push')).toBeUndefined();
    expect(build.captureRouterTransitionStart('/chats', 'replace')).toBeUndefined();
    expect(() => build.onRouterTransitionStart('/chats', 'traverse')).not.toThrow();
    expect(getInternalClient()).toBeUndefined();
  });
});

describe('D7 / decision 31: the built entries’ export shape (dist/nextjs/{client,server,edge}.js)', () => {
  const COMMON = ['captureException', 'captureMessage', 'captureRequestError', 'captureRouterTransitionStart', 'close', 'flush', 'getClient', 'onRouterTransitionStart', 'setTag', 'setTenant', 'setUser'];

  it.each(['dist/nextjs/client.js', 'dist/nextjs/server.js', 'dist/nextjs/edge.js'])('D7: %s exports init, captureRequestError, both router names and §4.3’s API', async (file) => {
    const path = join(root, file);
    expect(existsSync(path), `${file} is missing: run pnpm build first`).toBe(true);
    const mod = (await import(pathToFileURL(path).href)) as Record<string, unknown>;
    for (const name of [...COMMON, 'init']) expect(typeof mod[name], name).toBe('function');
  });

  it('D7: dist/nextjs/route.js exports createSentinelRouteHandler and no init', async () => {
    const path = join(root, 'dist/nextjs/route.js');
    expect(existsSync(path), 'dist/nextjs/route.js is missing: run pnpm build first').toBe(true);
    const mod = (await import(pathToFileURL(path).href)) as Record<string, unknown>;
    expect(typeof mod.createSentinelRouteHandler).toBe('function');
    expect(mod).not.toHaveProperty('init');
  });
});
