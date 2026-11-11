// @vitest-environment jsdom
//
// fetch keeps unhandledrejection working (spec §5.3, §5 *No wrapper marks a page promise handled*): jsdom never
// fires `unhandledrejection` on its own, so this bridges Node's own `unhandledRejection` (the real signal an
// uncaught rejected promise produces) onto `window` the way a browser host does, and proves the wrapper's
// chained promise still lets it fire exactly once for an uncaught failed fetch.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { captureException, close, init } from '../../src/browser.js';
import { getInternalClient } from '../../src/core/state.js';
import type { SentinelEvent } from '../../src/core/types.js';
import { mockFetch } from '../helpers/fetch-mock.js';
import { resetPipeline } from '../helpers/pipeline.js';

function queued(): SentinelEvent[] {
  return (getInternalClient()?.queue ?? []).map((json) => JSON.parse(json) as SentinelEvent);
}

const bridgeListeners = new Set<(...args: unknown[]) => void>();
let bridgeCount = 0;

function installBridge(): void {
  bridgeCount = 0;
  const onRejection = (...args: unknown[]): void => {
    const [reason, promise] = args as [unknown, Promise<unknown>];
    bridgeCount++;
    try {
      const Ctor = (globalThis as { PromiseRejectionEvent?: unknown }).PromiseRejectionEvent;
      const ev =
        typeof Ctor === 'function'
          ? new (Ctor as new (type: string, init: unknown) => Event)('unhandledrejection', { promise, reason, cancelable: true })
          : new CustomEvent('unhandledrejection', { detail: { promise, reason } });
      window.dispatchEvent(ev);
    } catch {
      // Best effort — a missing PromiseRejectionEvent constructor is jsdom's, not ours.
    }
  };
  bridgeListeners.add(onRejection);
  process.on('unhandledRejection', onRejection);
}

function uninstallBridge(): void {
  for (const l of bridgeListeners) process.removeListener('unhandledRejection', l);
  bridgeListeners.clear();
}

/** Runs `fn` with every OTHER unhandledRejection listener temporarily removed (tests/handlers/node.test.ts's pattern). */
async function withSoleListener(fn: () => Promise<void> | void): Promise<void> {
  const all = process.listeners('unhandledRejection') as Array<(...args: unknown[]) => void>;
  const bridge = all.filter((l) => bridgeListeners.has(l));
  const others = all.filter((l) => !bridgeListeners.has(l));
  for (const l of others) process.removeListener('unhandledRejection', l);
  try {
    expect(bridge).toHaveLength(1);
    await fn();
  } finally {
    for (const l of others) process.on('unhandledRejection', l);
  }
}

beforeEach(() => {
  resetPipeline();
  installBridge();
});

afterEach(async () => {
  await close(10);
  uninstallBridge();
  resetPipeline();
});

describe('fetch keeps unhandledrejection working (spec §5.3)', () => {
  it('uncaught: exactly one unhandledrejection, one event, one fetch crumb at level error', async () => {
    mockFetch();
    (globalThis as { fetch?: unknown }).fetch = () => Promise.reject(new TypeError('Failed to fetch'));
    init({ tunnel: '/api/sentinel', breadcrumbs: { fetch: true, console: false, xhr: false, navigation: false, click: false } });
    await withSoleListener(async () => {
      fetch('/api/x'); // no handler attached — genuinely uncaught, so Node's own detection fires
      await new Promise((r) => setTimeout(r, 0));
      await new Promise((r) => setTimeout(r, 0));
    });
    expect(bridgeCount).toBe(1);
    captureException(new Error('after'));
    const [event] = queued();
    const fetchCrumbs = (event?.breadcrumbs ?? []).filter((c) => c.category === 'fetch');
    expect(fetchCrumbs).toHaveLength(1);
    expect(fetchCrumbs[0]?.level).toBe('error');
  });

  it('caught: the bridge counts 0, no event captured, the fetch crumb still recorded', async () => {
    mockFetch();
    (globalThis as { fetch?: unknown }).fetch = () => Promise.reject(new TypeError('Failed to fetch'));
    init({ tunnel: '/api/sentinel' });
    await withSoleListener(async () => {
      await fetch('/api/x').catch(() => undefined);
      await new Promise((r) => setTimeout(r, 0));
    });
    expect(bridgeCount).toBe(0);
    captureException(new Error('after'));
    const [event] = queued();
    const fetchCrumbs = (event?.breadcrumbs ?? []).filter((c) => c.category === 'fetch');
    expect(fetchCrumbs).toHaveLength(1);
  });

  it('resolved: the awaited value is the same Response', async () => {
    mockFetch();
    const response = new Response('ok', { status: 200 });
    (globalThis as { fetch?: unknown }).fetch = () => Promise.resolve(response);
    init({ tunnel: '/api/sentinel' });
    const result = await fetch('/api/x');
    expect(result).toBe(response);
  });
});
