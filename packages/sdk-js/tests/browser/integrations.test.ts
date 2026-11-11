// @vitest-environment jsdom
//
// The automatic browser breadcrumb integrations (spec §5, TASK-03): the page test (all four/five kinds on one
// captured error), each integration's off switch, `close()`'s restore/pass-through, and the selector shape.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { captureException, close, init } from '../../src/browser.js';
import { getInternalClient } from '../../src/core/state.js';
import type { SentinelEvent } from '../../src/core/types.js';
import { mockFetch } from '../helpers/fetch-mock.js';
import { resetPipeline } from '../helpers/pipeline.js';

function queued(): SentinelEvent[] {
  return (getInternalClient()?.queue ?? []).map((json) => JSON.parse(json) as SentinelEvent);
}

beforeEach(() => {
  resetPipeline();
  document.body.innerHTML = '';
});

afterEach(async () => {
  await close(10);
  resetPipeline();
});

describe('the page test: console, fetch, xhr, navigation, click all recorded on one captured error', () => {
  it('produces one crumb of each category, filtered, none of the secrets', async () => {
    // The page itself starts on a URL carrying a secret query, so the navigation crumb's `from` (not just
    // `to`) must be exercised too — done via history.pushState before init(), so nothing is recorded yet.
    history.pushState({}, '', '/start?token=start0k');
    document.body.innerHTML = '<form><input id="secret" value="secret-input-value"><button id="save" class="btn primary">Save</button></form>';
    mockFetch();
    (globalThis as { fetch?: unknown }).fetch = () => Promise.resolve(new Response('secret-body', { status: 200 }));
    class FakeXhr {
      static instances: FakeXhr[] = [];
      listeners: Record<string, Array<() => void>> = {};
      status = 200;
      open(..._args: unknown[]): void {
        /* recorded by the wrapper */
      }
      send(..._args: unknown[]): void {
        FakeXhr.instances.push(this);
        queueMicrotask(() => this.listeners.loadend?.forEach((l) => l()));
      }
      addEventListener(type: string, listener: () => void): void {
        (this.listeners[type] ??= []).push(listener);
      }
      removeEventListener(type: string, listener: () => void): void {
        this.listeners[type] = (this.listeners[type] ?? []).filter((l) => l !== listener);
      }
    }
    (globalThis as { XMLHttpRequest?: unknown }).XMLHttpRequest = FakeXhr as unknown as typeof XMLHttpRequest;

    init({ tunnel: '/api/sentinel' });

    console.log('a console line');
    void fetch('/api/x?token=t0k&page=2', { method: 'POST', body: 'secret-body' });
    const xhr = new FakeXhr();
    xhr.open('POST', '/api/y?token=x0k');
    xhr.send('secret-xhr-body');
    await Promise.resolve();
    history.pushState({}, '', '/reset?code=code-secret-marker');
    document.getElementById('secret')?.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    document.getElementById('save')?.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    await new Promise((r) => setTimeout(r, 20));

    captureException(new Error('boom'));
    const [event] = queued();
    const categories = (event?.breadcrumbs ?? []).map((c) => c.category);
    expect(categories).toEqual(expect.arrayContaining(['console', 'fetch', 'xhr', 'navigation', 'ui.click']));

    const serialised = JSON.stringify(event);
    expect(serialised).not.toContain('secret-input-value');
    expect(serialised).not.toContain('secret-body');
    expect(serialised).not.toContain('secret-xhr-body');
    expect(serialised).not.toContain('t0k');
    expect(serialised).not.toContain('x0k');
    expect(serialised).not.toContain('code-secret-marker');
    expect(serialised).not.toContain('start0k');
    expect(serialised).toContain('token=[Filtered]');
    expect(serialised).toContain('code=[Filtered]');

    const xhrCrumb = (event?.breadcrumbs ?? []).find((c) => c.category === 'xhr');
    expect(xhrCrumb?.data?.url).toContain('[Filtered]');
    expect(xhrCrumb?.data).not.toHaveProperty('body');

    const navCrumb = (event?.breadcrumbs ?? []).find((c) => c.category === 'navigation');
    expect(navCrumb?.data?.from).toContain('[Filtered]');
    expect(navCrumb?.data?.to).toContain('[Filtered]');

    const clicks = (event?.breadcrumbs ?? []).filter((c) => c.category === 'ui.click');
    const saveClick = clicks.find((c) => c.message === 'form > button#save.btn.primary');
    expect(saveClick).toBeDefined();
    const secretClick = clicks.find((c) => c.message?.includes('#secret'));
    expect(secretClick).toBeDefined();
    for (const crumb of clicks) {
      expect(JSON.stringify(crumb)).not.toContain('secret-input-value');
    }

    const target = getInternalClient()?.options.tunnel;
    void target;
    for (const crumb of event?.breadcrumbs ?? []) {
      expect(JSON.stringify(crumb)).not.toContain('/api/sentinel');
    }
  });
});

describe('per-integration switches', () => {
  it('breadcrumbs: { console: false } records none of that kind while the other four still record', () => {
    mockFetch();
    init({ tunnel: '/api/sentinel', breadcrumbs: { console: false } });
    console.log('should not be recorded');
    history.pushState({}, '', '/x');
    captureException(new Error('x'));
    const categories = (queued()[0]?.breadcrumbs ?? []).map((c) => c.category);
    expect(categories).not.toContain('console');
    expect(categories).toContain('navigation');
  });

  it('breadcrumbs: false records none', () => {
    mockFetch();
    init({ tunnel: '/api/sentinel', breadcrumbs: false });
    console.log('should not be recorded');
    history.pushState({}, '', '/x');
    captureException(new Error('x'));
    expect(queued()[0]?.breadcrumbs).toBeUndefined();
  });
});

describe('close() restore / pass-through', () => {
  it('restores console when the SDK still owns the wrapper, and leaves a later wrapper working otherwise', async () => {
    mockFetch();
    const originalLog = console.log;
    init({ tunnel: '/api/sentinel' });
    expect(console.log).not.toBe(originalLog);
    await close(10);
    expect(console.log).toBe(originalLog);

    // A later wrapper (a stand-in for Sentry) survives close() as a pass-through, not stacked or replaced.
    mockFetch();
    init({ tunnel: '/api/sentinel' });
    const sdkWrapped = console.log;
    let calls = 0;
    console.log = ((...args: unknown[]) => {
      calls++;
      return sdkWrapped(...args);
    }) as typeof console.log;
    await close(10);
    console.log('after close, through the later wrapper');
    expect(calls).toBe(1);
  });
});

describe('console: filter then cut', () => {
  it('a 518-byte argument whose cut would otherwise end inside a nested %XX is filtered first', () => {
    mockFetch();
    init({ tunnel: '/api/sentinel' });
    const arg = '/login?next=%2Freset%3Freset_token%3DS3CR3T%26pad%3D' + 'a'.repeat(458) + '%26b%3D1';
    console.log(arg);
    captureException(new Error('x'));
    const crumb = (queued()[0]?.breadcrumbs ?? []).find((c) => c.category === 'console');
    expect(crumb?.message).not.toContain('S3CR3T');
    expect(crumb?.message).not.toContain('%252');
    expect(new TextEncoder().encode(crumb?.message ?? '').byteLength).toBeLessThanOrEqual(512);
  });
});
