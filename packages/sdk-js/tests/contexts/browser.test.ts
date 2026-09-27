// @vitest-environment jsdom
//
// The browser's automatic contexts and request (spec §6.1, TASK-04): each field guarded, no contexts.browser /
// .os (D2), app-set contexts win over automatic ones, key by key.

import { beforeEach, describe, expect, it } from 'vitest';
import { browserAutoContexts } from '../../src/browser/contexts.js';
import { captureException } from '../../src/core/capture.js';
import { setContext } from '../../src/core/scope.js';
import { resetPipeline, setup } from '../helpers/pipeline.js';

beforeEach(resetPipeline);

describe('browserAutoContexts (spec §6.1)', () => {
  it('reads request.url (origin + pathname, no query/fragment), query_string and the User-Agent header', () => {
    // location.search never carries a fragment in a real browser (location.hash is separate).
    Object.defineProperty(window, 'location', {
      value: { origin: 'https://app.example', pathname: '/dashboard', search: '?tab=1' },
      writable: true,
    });
    Object.defineProperty(window.navigator, 'userAgent', { value: 'TestAgent/1.0', configurable: true });
    const auto = browserAutoContexts();
    expect(auto.request?.url).toBe('https://app.example/dashboard');
    expect(auto.request?.query_string).toBe('tab=1');
    expect(auto.request?.headers).toEqual({ 'User-Agent': 'TestAgent/1.0' });
  });

  it('never sends contexts.browser or contexts.os (D2)', () => {
    const auto = browserAutoContexts();
    expect(auto.contexts?.browser).toBeUndefined();
    expect(auto.contexts?.os).toBeUndefined();
  });

  it('contexts.culture: locale and timezone, each only when a non-empty string', () => {
    Object.defineProperty(window.navigator, 'language', { value: 'en-US', configurable: true });
    const auto = browserAutoContexts();
    expect(auto.contexts?.culture).toMatchObject({ locale: 'en-US' });
  });

  it('contexts.device: screen / pixel ratio / memory / cpu, omitted entirely when nothing is exposed', () => {
    Object.defineProperty(window.navigator, 'hardwareConcurrency', { value: 8, configurable: true });
    const auto = browserAutoContexts();
    expect(auto.contexts?.device).toMatchObject({ processor_count: 8 });
  });

  it('never throws even when location/navigator access throws', () => {
    const original = Object.getOwnPropertyDescriptor(window, 'location');
    Object.defineProperty(window, 'location', {
      get() {
        throw new Error('nope');
      },
      configurable: true,
    });
    try {
      expect(() => browserAutoContexts()).not.toThrow();
    } finally {
      if (original) Object.defineProperty(window, 'location', original);
    }
  });
});

describe('wired through captureException (spec §3.6: automatic merged under app contexts)', () => {
  it('an automatic context is present when no app context of that name exists', () => {
    const p = setup({}, 'browser', () => ({ contexts: { device: { screen_width_pixels: 1920 } } }));
    captureException(new Error('x'));
    expect(p.events()[0]?.contexts?.device).toEqual({ screen_width_pixels: 1920 });
  });

  it('app-set fields win over automatic fields in the same context, key by key', () => {
    setContext('device', { screen_width_pixels: 1 });
    const p = setup({}, 'browser', () => ({ contexts: { device: { screen_width_pixels: 1920, screen_height_pixels: 1080 } } }));
    captureException(new Error('x'));
    expect(p.events()[0]?.contexts?.device).toEqual({ screen_width_pixels: 1, screen_height_pixels: 1080 });
  });

  it('a name only the app set is taken as is', () => {
    setContext('ok', { a: 1 });
    const p = setup({}, 'browser', () => ({ contexts: { device: { screen_width_pixels: 1920 } } }));
    captureException(new Error('x'));
    expect(p.events()[0]?.contexts?.ok).toEqual({ a: 1 });
    expect(p.events()[0]?.contexts?.device).toEqual({ screen_width_pixels: 1920 });
  });

  it('the automatic request is attached to the event', () => {
    const p = setup({}, 'browser', () => ({ request: { url: 'https://app.example/x', headers: { 'User-Agent': 'UA' } } }));
    captureException(new Error('x'));
    expect(p.events()[0]?.request).toEqual({ url: 'https://app.example/x', headers: { 'User-Agent': 'UA' } });
  });
});
