// @vitest-environment jsdom
//
// The browser global handlers (contract §4.6, decision 37): B26 (a)–(d) — the message-only `ErrorEvent` →
// `captureMessage(ev.message, { level: 'error' })` with `hint.originalException` = the message string, a
// `PromiseRejectionEvent` → the exception path, a resource-load `Event` never captured, an explicit capture with
// no `source` key — plus the registration rules: `addEventListener` without `capture`, never `preventDefault` /
// `stopImmediatePropagation`, `window.onerror` left alone, and only the two browser values of the four.
// Shuffle-safe: every test starts from a deleted slot with the previous client's listeners uninstalled.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { captureException, captureMessage, close, init } from '../../src/browser.js';
import { getInternalClient } from '../../src/core/state.js';
import type { BeforeSendHint, SentinelEvent } from '../../src/core/types.js';
import { mockFetch } from '../helpers/fetch-mock.js';
import { resetPipeline } from '../helpers/pipeline.js';

function queued(): SentinelEvent[] {
  return (getInternalClient()?.queue ?? []).map((json) => JSON.parse(json) as SentinelEvent);
}

const swallow = (): void => undefined;

beforeEach(() => {
  resetPipeline();
  mockFetch();
  window.addEventListener('error', swallow);
});

afterEach(async () => {
  await close(50);
  resetPipeline();
  window.removeEventListener('error', swallow);
});

describe('B26: the global capture shapes', () => {
  it("B26 (a): a cross-origin 'Script error.' (ErrorEvent, error: null) → error_class Message, stacktrace [], metadata.level error, metadata.source window.onerror; hint.originalException is the message string", () => {
    const hints: BeforeSendHint[] = [];
    init({
      beforeSend: (event, hint) => {
        hints.push(hint);
        return event;
      },
    });
    const ev = new ErrorEvent('error', { message: 'Script error.', error: null, filename: '', lineno: 0, colno: 0 });
    window.dispatchEvent(ev);
    const events = queued();
    expect(events).toHaveLength(1);
    expect(events[0]).toMatchObject({ error_class: 'Message', message: 'Script error.', stacktrace: [], metadata: { level: 'error', source: 'window.onerror', runtime: 'browser' } });
    expect(hints).toEqual([{ originalException: 'Script error.' }]);
    expect(ev.defaultPrevented).toBe(false);
  });

  it('B26 (a′): an ErrorEvent WITH error set captures that Error by its class and stack, source window.onerror; a thrown primitive by the exception path', () => {
    init();
    const err = new TypeError('x is not a function');
    window.dispatchEvent(new ErrorEvent('error', { message: err.message, error: err }));
    window.dispatchEvent(new ErrorEvent('error', { message: '42', error: 42 }));
    const events = queued();
    expect(events.map((e) => [e.error_class, e.message, e.metadata.source])).toEqual([
      ['TypeError', 'x is not a function', 'window.onerror'],
      ['NonError', '42', 'window.onerror'],
    ]);
    expect(events[0]?.stacktrace.length).toBeGreaterThan(0);
    expect('level' in (events[0]?.metadata ?? {})).toBe(false);
    // Dispatching the same Error again is deduplicated by identity (contract §4.3).
    window.dispatchEvent(new ErrorEvent('error', { message: err.message, error: err }));
    expect(queued()).toHaveLength(2);
  });

  it("B26 (b): Promise.reject('nope') → error_class Error, message nope, metadata.source unhandledrejection; an Error reason keeps its class; never preventDefault", () => {
    init();
    const rejected = Promise.reject('nope');
    rejected.catch(() => undefined);
    const ev = new PromiseRejectionEvent('unhandledrejection', { promise: rejected, reason: 'nope', cancelable: true });
    window.dispatchEvent(ev);
    const boom = new RangeError('boom');
    const rejected2 = Promise.reject(boom);
    rejected2.catch(() => undefined);
    window.dispatchEvent(new PromiseRejectionEvent('unhandledrejection', { promise: rejected2, reason: boom, cancelable: true }));
    const events = queued();
    expect(events.map((e) => [e.error_class, e.message, e.metadata.source])).toEqual([
      ['Error', 'nope', 'unhandledrejection'],
      ['RangeError', 'boom', 'unhandledrejection'],
    ]);
    expect(ev.defaultPrevented).toBe(false);
  });

  it('B26 (c): a resource-load Event named error (an <img> 404) is never captured — the listener is bubbling, and a non-ErrorEvent arriving anyway is ignored', () => {
    init();
    // What a failed <img> dispatches: a plain, non-bubbling Event at the element; it reaches window only in the
    // capture phase, which the SDK does not listen in.
    const img = document.createElement('img');
    document.body.appendChild(img);
    img.dispatchEvent(new Event('error', { bubbles: false, cancelable: false }));
    // And a plain Event dispatched straight at window (a non-ErrorEvent arriving anyway).
    window.dispatchEvent(new Event('error'));
    expect(queued()).toHaveLength(0);
    document.body.removeChild(img);
  });

  it('B26 (d): an explicit captureException / captureMessage carries no metadata.source key; the browser values are exactly two of the four', () => {
    init();
    captureException(new Error('from error.tsx'), { contexts: { react: { componentStack: '\n    at Page' } } });
    captureMessage('explicit');
    window.dispatchEvent(new ErrorEvent('error', { message: 'global', error: new Error('global') }));
    const events = queued();
    expect(events).toHaveLength(3);
    expect('source' in (events[0]?.metadata ?? {})).toBe(false);
    expect('source' in (events[1]?.metadata ?? {})).toBe(false);
    expect(events[0]?.metadata.react_component_stack).toBe('\n    at Page');
    const sources = events.map((e) => e.metadata.source).filter((s) => s !== undefined);
    expect(sources).toEqual(['window.onerror']);
    for (const s of sources) expect(['window.onerror', 'unhandledrejection']).toContain(s);
    expect(sources).not.toContain('uncaughtException');
    expect(sources).not.toContain('unhandledRejection');
  });

  it('B26: a per-call ctx.metadata.source on an explicit capture is dropped (SDK-owned, decision 38), never a fifth value', () => {
    init();
    captureMessage('spoof', { metadata: { source: 'window.onerror' } });
    expect('source' in (queued()[0]?.metadata ?? {})).toBe(false);
  });
});

describe('§4.6: registration rules', () => {
  it('registers through addEventListener without capture, leaves window.onerror / onunhandledrejection alone, and never stops propagation', () => {
    const adds: Array<[string, unknown]> = [];
    const add = window.addEventListener.bind(window);
    vi.spyOn(window, 'addEventListener').mockImplementation(((type: string, listener: EventListener, options?: unknown) => {
      adds.push([type, options]);
      add(type as keyof WindowEventMap, listener, options as AddEventListenerOptions);
    }) as typeof window.addEventListener);
    const onerrorBefore = window.onerror;
    const onrejectionBefore = window.onunhandledrejection;
    init();
    const ours = adds.filter(([t]) => t === 'error' || t === 'unhandledrejection');
    expect(ours.map(([t]) => t)).toEqual(['error', 'unhandledrejection']);
    for (const [, options] of ours) {
      const capture = typeof options === 'boolean' ? options : (options as { capture?: boolean } | undefined)?.capture;
      expect(capture).toBeFalsy();
    }
    expect(window.onerror).toBe(onerrorBefore);
    expect(window.onunhandledrejection).toBe(onrejectionBefore);

    // A listener registered after the SDK's still fires: nothing calls stopImmediatePropagation.
    const later = vi.fn();
    window.addEventListener('error', later);
    const ev = new ErrorEvent('error', { message: 'x', error: new Error('x'), cancelable: true });
    window.dispatchEvent(ev);
    expect(later).toHaveBeenCalledTimes(1);
    expect(ev.defaultPrevented).toBe(false);
    window.removeEventListener('error', later);
  });

  it('installGlobalHandlers: false installs nothing; the browser build under a globalThis with no window reports handler_install_failed once and never throws', () => {
    const add = vi.spyOn(window, 'addEventListener');
    init({ installGlobalHandlers: false });
    expect(add.mock.calls.filter(([t]) => t === 'error' || t === 'unhandledrejection')).toHaveLength(0);
    resetPipeline();
    mockFetch();
    const errors: string[] = [];
    const win = window;
    vi.stubGlobal('window', undefined);
    try {
      expect(() => init({ tunnel: 'http://localhost:3000/api/sentinel', onError: (e) => errors.push(e.code) })).not.toThrow();
    } finally {
      vi.stubGlobal('window', win);
    }
    expect(errors).toEqual(['handler_install_failed']);
  });
});
