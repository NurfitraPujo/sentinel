// The node process hooks (contract §4.6, decisions 22 and 37), in process: the two listeners installed by `init`
// and removed by a second `init` / `close`, `metadata.source` `uncaughtException` / `unhandledRejection`, the
// sole-listener re-raise on `process.nextTick` versus the host-listener branch, the dedup that keeps the
// re-raised rejection's `source`, and `beforeSend` called synchronously with a promise-returning hook ignored.
// The listeners are INVOKED directly here (emitting `unhandledRejection` would trip vitest's own listener) — the
// exit behaviour itself is tests/node-exit (spawned node, N5–N7, N16). Shuffle-safe: every test starts from a
// deleted slot with the previous client's hooks removed.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getInternalClient } from '../../src/core/state.js';
import type { BeforeSendHint, SentinelEvent } from '../../src/core/types.js';
import { captureException, close, init } from '../../src/node.js';
import { mockFetch } from '../helpers/fetch-mock.js';
import { resetPipeline } from '../helpers/pipeline.js';

const ENDPOINT = 'http://127.0.0.1:1/ingest';
type Listener = (...args: unknown[]) => void;

function queued(): SentinelEvent[] {
  return (getInternalClient()?.queue ?? []).map((json) => JSON.parse(json) as SentinelEvent);
}

/** The listeners `init` added for `event`: the ones present now and not before. */
function listenersOf(event: string): Listener[] {
  // @types/node 20 types `process.listeners` by signal name only; the EventEmitter method takes any event.
  return (process as unknown as { listeners(e: string): Listener[] }).listeners(event);
}

function added(event: 'uncaughtExceptionMonitor' | 'unhandledRejection', before: Listener[]): Listener[] {
  return listenersOf(event).filter((l) => !before.includes(l));
}

/**
 * Run `fn` with every OTHER `unhandledRejection` listener (vitest's) temporarily removed, so the SDK's is the
 * sole one — the branch N6 spawns for. Restored in `finally`, whatever happens.
 */
function withSoleListener(fn: () => void): void {
  const all = listenersOf('unhandledRejection');
  const sdk = all.filter((l) => sdkListeners.has(l));
  const others = all.filter((l) => !sdkListeners.has(l));
  for (const l of others) process.removeListener('unhandledRejection', l);
  try {
    expect(sdk).toHaveLength(1);
    expect(process.listenerCount('unhandledRejection')).toBe(1);
    fn();
  } finally {
    for (const l of others) process.on('unhandledRejection', l);
  }
}

const sdkListeners = new Set<Listener>();

/**
 * Observe the re-raise: `process.nextTick` runs its callback at once inside `fn`'s window and records what it
 * threw (a foreign tick — a stream write's — runs harmlessly early). Restored before returning.
 */
function ticksThrowing(fn: () => void): unknown[] {
  const thrown: unknown[] = [];
  const spy = vi.spyOn(process, 'nextTick').mockImplementation(((cb: (...a: unknown[]) => void, ...args: unknown[]) => {
    try {
      cb(...args);
    } catch (e) {
      thrown.push(e);
    }
  }) as typeof process.nextTick);
  try {
    fn();
  } finally {
    spy.mockRestore();
  }
  return thrown;
}
let baseline: { monitor: Listener[]; rejection: Listener[] } = { monitor: [], rejection: [] };

function installed(): { monitor: Listener[]; rejection: Listener[] } {
  const out = { monitor: added('uncaughtExceptionMonitor', baseline.monitor), rejection: added('unhandledRejection', baseline.rejection) };
  for (const l of out.rejection) sdkListeners.add(l);
  return out;
}

beforeEach(() => {
  resetPipeline();
  mockFetch();
  sdkListeners.clear();
  baseline = { monitor: listenersOf('uncaughtExceptionMonitor'), rejection: listenersOf('unhandledRejection') };
});

afterEach(async () => {
  vi.useRealTimers();
  await close(50);
  resetPipeline();
  expect(installed()).toEqual({ monitor: [], rejection: [] });
});

describe('§4.6 node: installation and removal', () => {
  it('init adds one uncaughtExceptionMonitor and one unhandledRejection listener; a second init removes them before adding new ones; close removes them', async () => {
    const order: string[] = [];
    const on = process.on.bind(process);
    const off = process.removeListener.bind(process);
    vi.spyOn(process, 'on').mockImplementation(((event: string, listener: Listener) => {
      if (event === 'uncaughtExceptionMonitor' || event === 'unhandledRejection') order.push(`add:${event}`);
      return on(event, listener);
    }) as typeof process.on);
    vi.spyOn(process, 'removeListener').mockImplementation(((event: string, listener: Listener) => {
      if (event === 'uncaughtExceptionMonitor' || event === 'unhandledRejection') order.push(`remove:${event}`);
      return off(event, listener);
    }) as typeof process.removeListener);

    init({ endpoint: ENDPOINT, apiKey: 'k' });
    const first = installed();
    expect(first.monitor).toHaveLength(1);
    expect(first.rejection).toHaveLength(1);
    init({ endpoint: ENDPOINT, apiKey: 'k' });
    const second = installed();
    expect(second.monitor).toHaveLength(1);
    expect(second.rejection).toHaveLength(1);
    expect(second.monitor[0]).not.toBe(first.monitor[0]);
    expect(order).toEqual([
      'add:uncaughtExceptionMonitor',
      'add:unhandledRejection',
      'remove:uncaughtExceptionMonitor',
      'remove:unhandledRejection',
      'add:uncaughtExceptionMonitor',
      'add:unhandledRejection',
    ]);
    // Never `process.on('uncaughtException')` — the monitor does not stop the process from exiting.
    expect(order.some((o) => o === 'add:uncaughtException')).toBe(false);
    await close(50);
    expect(installed()).toEqual({ monitor: [], rejection: [] });
  });

  it('installGlobalHandlers: false installs nothing; the no-op client (not_configured) installs nothing; the node build under a globalThis with no process reports handler_install_failed once and never throws', () => {
    init({ endpoint: ENDPOINT, apiKey: 'k', installGlobalHandlers: false });
    expect(installed()).toEqual({ monitor: [], rejection: [] });
    resetPipeline();
    mockFetch();
    const saved = { e: process.env.SENTINEL_ENDPOINT, k: process.env.SENTINEL_API_KEY };
    delete process.env.SENTINEL_ENDPOINT;
    delete process.env.SENTINEL_API_KEY;
    try {
      const codes: string[] = [];
      init({ onError: (e) => codes.push(e.code) });
      expect(codes).toEqual(['not_configured']);
      expect(installed()).toEqual({ monitor: [], rejection: [] });
    } finally {
      if (saved.e !== undefined) process.env.SENTINEL_ENDPOINT = saved.e;
      if (saved.k !== undefined) process.env.SENTINEL_API_KEY = saved.k;
    }
    resetPipeline();
    mockFetch();
    const codes: string[] = [];
    const proc = process;
    vi.stubGlobal('process', undefined);
    try {
      expect(() => init({ endpoint: ENDPOINT, apiKey: 'k', onError: (e) => codes.push(e.code) })).not.toThrow();
    } finally {
      vi.stubGlobal('process', proc);
    }
    expect(codes).toEqual(['handler_install_failed']);
  });
});

describe('§4.6 node: the capture shapes (decision 37) and the re-raise', () => {
  it('N5 (unit): the monitor captures with metadata.source uncaughtException; the capture enqueues and returns — no fetch inside the monitor', () => {
    const fetchSpy = vi.mocked(globalThis.fetch);
    init({ endpoint: ENDPOINT, apiKey: 'k' });
    const { monitor } = installed();
    const err = new Error('x');
    monitor[0]?.(err, 'uncaughtException');
    const events = queued();
    expect(events).toHaveLength(1);
    expect(events[0]).toMatchObject({ error_class: 'Error', message: 'x', metadata: { source: 'uncaughtException', runtime: 'node' } });
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('N6 (unit): as the sole listener, the rejection listener captures with source unhandledRejection, then re-raises the reason on process.nextTick; the re-raise reaching the monitor is deduplicated — one event, source unhandledRejection', () => {
    init({ endpoint: ENDPOINT, apiKey: 'k' });
    const { monitor, rejection } = installed();
    const reason = new Error('y');
    const thrown = ticksThrowing(() => {
      withSoleListener(() => {
        rejection[0]?.(reason, Promise.resolve());
      });
    });
    expect(thrown).toEqual([reason]);
    expect(thrown[0]).toBe(reason);
    // What node does next: the throw reaches uncaughtExceptionMonitor with the same object.
    monitor[0]?.(reason, 'uncaughtException');
    const events = queued();
    expect(events).toHaveLength(1);
    expect(events[0]?.metadata.source).toBe('unhandledRejection');
    expect(events[0]?.message).toBe('y');
  });

  it('N6 (unit): a non-Error reason is re-raised as is', () => {
    init({ endpoint: ENDPOINT, apiKey: 'k' });
    const { rejection } = installed();
    const thrown = ticksThrowing(() => {
      withSoleListener(() => {
        rejection[0]?.('nope', Promise.resolve());
      });
    });
    expect(thrown).toEqual(['nope']);
    expect(queued()[0]).toMatchObject({ error_class: 'Error', message: 'nope', metadata: { source: 'unhandledRejection' } });
  });

  it('N7 (unit): with a host listener present, the rejection is captured and NOT re-raised — no nextTick', () => {
    init({ endpoint: ENDPOINT, apiKey: 'k' });
    const { rejection } = installed();
    const host = vi.fn();
    process.on('unhandledRejection', host);
    let thrown: unknown[] = [];
    try {
      expect(process.listenerCount('unhandledRejection')).toBeGreaterThan(1);
      thrown = ticksThrowing(() => {
        rejection[0]?.(new Error('z'), Promise.resolve());
      });
    } finally {
      process.removeListener('unhandledRejection', host);
    }
    expect(thrown).toEqual([]);
    expect(queued()[0]).toMatchObject({ message: 'z', metadata: { source: 'unhandledRejection' } });
  });

  it('decision 22: beforeSend is called synchronously on both node captures; a promise-returning hook is ignored — the event is sent as built, with one debug line', () => {
    const calls: Array<[string, BeforeSendHint]> = [];
    const lines: string[] = [];
    vi.spyOn(process.stderr, 'write').mockImplementation(((chunk: unknown) => {
      lines.push(String(chunk));
      return true;
    }) as typeof process.stderr.write);
    init({
      endpoint: ENDPOINT,
      apiKey: 'k',
      debug: true,
      beforeSend: (event, hint) => {
        calls.push([String(event.metadata.source), hint]);
        event.metadata.hooked = 'sync';
        return Promise.resolve(null); // would drop the event anywhere else — ignored here
      },
    });
    const { monitor, rejection } = installed();
    const a = new Error('a');
    const b = new Error('b');
    monitor[0]?.(a, 'uncaughtException');
    const host = vi.fn();
    process.on('unhandledRejection', host);
    try {
      rejection[0]?.(b, Promise.resolve());
    } finally {
      process.removeListener('unhandledRejection', host);
    }
    // Called synchronously — both hints recorded before any await.
    expect(calls).toEqual([
      ['uncaughtException', { originalException: a }],
      ['unhandledRejection', { originalException: b }],
    ]);
    const events = queued();
    expect(events.map((e) => e.message)).toEqual(['a', 'b']);
    // "As built": the pre-hook event, not the hook's synchronous mutation and not its promised null.
    expect(events.map((e) => e.metadata.hooked)).toEqual([undefined, undefined]);
    expect(lines.filter((l) => l.includes('decision 22'))).toHaveLength(2);
  });

  it('decision 22 (contrast): an explicit captureException still honours the async form — the promised null drops the event', async () => {
    init({ endpoint: ENDPOINT, apiKey: 'k', beforeSend: () => Promise.resolve(null) });
    captureException(new Error('explicit'));
    await new Promise((res) => setTimeout(res, 10));
    expect(queued()).toHaveLength(0);
  });
});
