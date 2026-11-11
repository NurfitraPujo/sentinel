// @vitest-environment jsdom
//
// The browser entry's `init` (contract §4.2): the `tunnel` same-origin rule (B19), a second `init`'s handler half
// — the old `error` / `unhandledrejection` listeners removed BEFORE the new ones are added, scope surviving,
// the two-module-instance case through dist/ (B22) — and `close`'s handler half — listeners gone whatever
// `flush` resolved, an `init` after `close` a first init keeping scope, the opaque `getClient()` (B23). jsdom's
// origin is http://localhost:3000; `fetch` is mocked, so "nothing posted" is "the spy was never called".
// Shuffle-safe: every test starts from a deleted slot with the previous client's listeners uninstalled.

import { existsSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { captureException, captureMessage, close, flush, getClient, init, setTag, setUser } from '../../src/browser.js';
import { getInternalClient } from '../../src/core/state.js';
import type { SentinelEvent } from '../../src/core/types.js';
import { errorRecorder, mockFetch } from '../helpers/fetch-mock.js';
import { resetPipeline } from '../helpers/pipeline.js';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const GLOBAL_TYPES = new Set(['error', 'unhandledrejection']);

interface ListenerLog {
  /** `add:error`, `remove:unhandledrejection`, … in call order, for the two global-handler types only. */
  calls: string[];
  /** Listeners currently registered for the two types, by identity. */
  live(): Set<unknown>;
}

/** Spy on `window.addEventListener` / `removeEventListener` and keep the live set of the SDK's two listeners. */
function watchListeners(): ListenerLog {
  const calls: string[] = [];
  const live = new Set<unknown>();
  const add = window.addEventListener.bind(window);
  const remove = window.removeEventListener.bind(window);
  vi.spyOn(window, 'addEventListener').mockImplementation(((type: string, listener: unknown, options?: unknown) => {
    if (GLOBAL_TYPES.has(type)) {
      calls.push(`add:${type}`);
      live.add(listener);
      // Contract §4.6: registered without `capture: true`.
      const capture = typeof options === 'boolean' ? options : (options as { capture?: boolean } | undefined)?.capture;
      expect(capture, `${type} listener registered with capture`).toBeFalsy();
    }
    add(type as keyof WindowEventMap, listener as EventListener, options as AddEventListenerOptions);
  }) as typeof window.addEventListener);
  vi.spyOn(window, 'removeEventListener').mockImplementation(((type: string, listener: unknown, options?: unknown) => {
    if (GLOBAL_TYPES.has(type)) {
      calls.push(`remove:${type}`);
      live.delete(listener);
    }
    remove(type as keyof WindowEventMap, listener as EventListener, options as EventListenerOptions);
  }) as typeof window.removeEventListener);
  return { calls, live: () => live };
}

function queued(): SentinelEvent[] {
  return (getInternalClient()?.queue ?? []).map((json) => JSON.parse(json) as SentinelEvent);
}

function throwPrimitive(value: unknown): void {
  window.dispatchEvent(new ErrorEvent('error', { message: String(value), error: value }));
}

// vitest's jsdom environment re-throws a window `error` event as an uncaught exception when no user listener is
// registered — exactly the no-listener states these tests dispatch into. A test-owned listener (added before the
// spies, so it is not in the SDK's count) keeps the environment quiet; it never calls preventDefault.
const swallow = (): void => undefined;

beforeEach(() => {
  resetPipeline();
  window.addEventListener('error', swallow);
});

afterEach(async () => {
  await close(50);
  resetPipeline();
  window.removeEventListener('error', swallow);
});

describe('B19: the tunnel same-origin rule', () => {
  it.each(['https://evil.example/collect', '//evil.example/collect'])('B19: init({ tunnel: %j }) → the no-op client, invalid_option tunnel once, no listeners, captures return ids, nothing posted', async (tunnel) => {
    const f = mockFetch();
    const log = watchListeners();
    const r = errorRecorder();
    expect(() => init({ tunnel, onError: r.onError })).not.toThrow();
    expect(getClient()).toMatchObject({ runtime: 'browser' });
    expect(r.codes()).toEqual(['invalid_option:tunnel']);
    expect(r.errors[0]?.message).toContain(tunnel);
    expect(log.calls).toEqual([]);
    expect(captureException(new Error('dropped'))).toMatch(UUID);
    throwPrimitive('dropped too');
    await expect(flush(100)).resolves.toBe(true);
    expect(f.posts).toHaveLength(0);
    expect(r.codes()).toEqual(['invalid_option:tunnel']);
  });

  it.each([
    ['/api/sentinel', 'http://localhost:3000/api/sentinel'],
    ['http://localhost:3000/api/sentinel', 'http://localhost:3000/api/sentinel'],
  ])('B19: init({ tunnel: %j }) is accepted and posts to %s', async (tunnel, resolved) => {
    const f = mockFetch();
    const r = errorRecorder();
    init({ tunnel, onError: r.onError });
    const id = captureMessage('sent');
    await expect(flush(2000)).resolves.toBe(true);
    expect(f.posts.map((p) => p.url)).toEqual([resolved]);
    expect(f.posts[0]?.events.map((e) => e.event_id)).toEqual([id]);
    expect(r.errors).toEqual([]);
  });

  it('B19: the default tunnel is /api/sentinel on the page origin; a same-origin absolute URL on another port is refused', () => {
    mockFetch();
    init();
    expect(getClient()?.options.tunnel).toBeUndefined();
    captureMessage('x');
    expect(queued()).toHaveLength(1);
    resetPipeline();
    mockFetch();
    const r = errorRecorder();
    init({ tunnel: 'http://localhost:3001/api/sentinel', onError: r.onError });
    expect(r.codes()).toEqual(['invalid_option:tunnel']);
  });
});

describe('B22 (handler half): a second init', () => {
  it('B22: the old error / unhandledrejection listeners are removed before the new ones are added; a thrown primitive after re-init is captured once, with user_id set before the first init and the tag set between the two', async () => {
    const f = mockFetch();
    const log = watchListeners();
    setUser({ id: 'usr_1' });
    init({ tunnel: '/api/sentinel' });
    expect(log.calls).toEqual(['add:error', 'add:unhandledrejection']);
    setTag('feature', 'chats');
    init({ tunnel: '/api/sentinel' });
    expect(log.calls).toEqual(['add:error', 'add:unhandledrejection', 'remove:error', 'remove:unhandledrejection', 'add:error', 'add:unhandledrejection']);
    expect(log.live().size).toBe(2);

    throwPrimitive('boom');
    const events = queued();
    expect(events).toHaveLength(1);
    expect(events[0]).toMatchObject({ error_class: 'Error', message: 'boom', user: { id: 'usr_1' }, tags: { feature: 'chats' }, metadata: { source: 'window.onerror' } });
    await expect(flush(2000)).resolves.toBe(true);
    expect(f.posts).toHaveLength(1);
    expect(f.posts[0]?.events).toHaveLength(1);
  });

  it('B22: a re-init with installGlobalHandlers: false uninstalls the old listeners and installs none', () => {
    mockFetch();
    const log = watchListeners();
    init();
    init({ installGlobalHandlers: false });
    expect(log.live().size).toBe(0);
    throwPrimitive('unseen');
    expect(queued()).toHaveLength(0);
    expect(getClient()?.options.installGlobalHandlers).toBe(false);
  });

  it('B22 (unit): init from dist/browser.js beside setUser / captureMessage from dist/core/index.js — two module instances, one slot: the event carries user_id and is posted by the first instance’s client', async () => {
    const browserDist = join(root, 'dist/browser.js');
    const coreDist = join(root, 'dist/core/index.js');
    expect(existsSync(browserDist) && existsSync(coreDist), 'dist/ is missing: run pnpm build first').toBe(true);
    const f = mockFetch();
    const browser = (await import(pathToFileURL(browserDist).href)) as typeof import('../../src/browser.js');
    const core = (await import(pathToFileURL(coreDist).href)) as typeof import('../../src/core/index.js');
    expect(browser.init).not.toBe(undefined);
    // Two instances: the functions are different objects …
    expect(browser.captureMessage).not.toBe(core.captureMessage);
    core.setUser({ id: 'usr_2' });
    const r = errorRecorder();
    browser.init({ tunnel: '/api/sentinel', onError: r.onError });
    // … but one SDK: the second instance sees the first's client.
    expect(core.getClient()).toMatchObject({ runtime: 'browser' });
    const id = core.captureMessage('through the second instance');
    await expect(core.flush(2000)).resolves.toBe(true);
    expect(f.posts).toHaveLength(1);
    expect(f.posts[0]?.url).toBe('http://localhost:3000/api/sentinel');
    expect(f.posts[0]?.events.map((e) => [e.event_id, e.user?.id])).toEqual([[id, 'usr_2']]);
    expect(r.errors).toEqual([]);
    await browser.close(100);
    expect(core.getClient()).toBeUndefined();
  });
});

describe('B23 (handler half): capture before init / after close, and close itself', () => {
  it('B23: captureException before any init returns a fresh id, queues nothing; getClient() is undefined', () => {
    const f = mockFetch();
    expect(getClient()).toBeUndefined();
    const a = captureException(new Error('early'));
    const b = captureException(new Error('early 2'));
    expect(a).toMatch(UUID);
    expect(b).toMatch(UUID);
    expect(a).not.toBe(b);
    expect(getInternalClient()).toBeUndefined();
    expect(f.posts).toHaveLength(0);
  });

  it.each([
    ['false — the mock never answers', true],
    ['true — the queue emptied', false],
  ])('B23: after close(300) resolving %s, the listeners are gone regardless; a capture is dropped; getClient() undefined; an init after it is a first init keeping scope and getClient() is the opaque { options, runtime }', async (_label, hold) => {
    const f = mockFetch();
    if (hold) f.hold();
    const log = watchListeners();
    const debugLines: string[] = [];
    vi.spyOn(console, 'debug').mockImplementation((line: unknown) => {
      debugLines.push(String(line));
    });
    setUser({ id: 'usr_1' });
    const r = errorRecorder();
    init({ tunnel: '/api/sentinel', debug: true, onError: r.onError });
    expect(log.live().size).toBe(2);
    captureMessage('one');
    captureMessage('two');
    captureMessage('three');
    const closed = close(300);
    await expect(closed).resolves.toBe(!hold);
    expect(log.live().size).toBe(0);
    expect(log.calls.filter((c) => c.startsWith('remove:'))).toEqual(['remove:error', 'remove:unhandledrejection']);
    expect(getClient()).toBeUndefined();
    if (hold) {
      expect(f.posts[0]?.aborted).toBe(true);
      expect(debugLines.filter((l) => /close: 3 event\(s\) dropped/.test(l))).toHaveLength(1);
    }
    const postsAtClose = f.posts.length;
    const errorsAtClose = r.errors.length;

    // After close: a capture returns an id and drops; a window error reaches no listener.
    expect(captureException(new Error('late'))).toMatch(UUID);
    throwPrimitive('late primitive');
    await new Promise((res) => setTimeout(res, 20));
    expect(f.posts).toHaveLength(postsAtClose);
    expect(r.errors).toHaveLength(errorsAtClose);

    // An init after close is a first init: nothing drained, the module-level scope kept, the listeners back.
    init({ tunnel: '/api/sentinel', onError: r.onError });
    expect(getInternalClient()?.transport?.stats()).toMatchObject({ queued: 0, inFlight: 0 });
    expect(log.live().size).toBe(2);
    throwPrimitive('after re-init');
    expect(queued().map((e) => e.user?.id)).toEqual(['usr_1']);

    const client = getClient();
    expect(client).toBeDefined();
    expect(Object.keys(client ?? {})).toEqual(['options', 'runtime']);
    expect(client?.runtime).toBe('browser');
    expect(Object.values(client ?? {}).some((v) => typeof v === 'function')).toBe(false);
    expect('apiKey' in (client?.options ?? {})).toBe(false);
    expect('endpoint' in (client?.options ?? {})).toBe(false);
    expect(client?.options).toMatchObject({ environment: 'production', sampleRate: 1, batchSize: 10, installGlobalHandlers: true, flushOnHide: true });
    expect(Object.isFrozen(client)).toBe(true);
    expect(Object.isFrozen(client?.options)).toBe(true);
  });
});
