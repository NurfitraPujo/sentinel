// @vitest-environment jsdom
//
// Import-time inertness with a `window` present (contract §4.7, §11; flows D7): the browser entry, evaluated
// fresh in a jsdom environment, adds no listener to `window` or `document`, sets no timer and calls no `fetch`
// until `init` — and `init` is what adds the `error` / `unhandledrejection` listeners (and the hide listeners).
// The source entry is imported here (vitest's runner transforms it; the built file's body is tests/inertness/
// dist.test.ts's, under node's own loader) with a cache-busting query so the module body runs inside the spies.

import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetPipeline } from '../helpers/pipeline.js';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
let n = 0;

beforeEach(() => {
  resetPipeline();
});

afterEach(() => {
  resetPipeline();
});

describe('D7 (jsdom): the browser entry with a window present', () => {
  it('registers nothing on window / document, no timer, no fetch at import; init adds the two global listeners and the hide listeners', async () => {
    const winAdd = vi.spyOn(window, 'addEventListener');
    const docAdd = vi.spyOn(document, 'addEventListener');
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(() => Promise.resolve(new Response('{}', { status: 202 })));
    const timer = vi.spyOn(globalThis, 'setTimeout');
    const interval = vi.spyOn(globalThis, 'setInterval');
    const procOn = vi.spyOn(process, 'on');

    const mod = (await import(`${pathToFileURL(join(root, 'dist/browser.js')).href}?inert=${++n}`)) as typeof import('../../src/browser.js');
    expect(typeof mod.init).toBe('function');
    expect(winAdd).not.toHaveBeenCalled();
    expect(docAdd).not.toHaveBeenCalled();
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(timer).not.toHaveBeenCalled();
    expect(interval).not.toHaveBeenCalled();
    expect(procOn).not.toHaveBeenCalled();

    mod.init({ tunnel: '/api/sentinel' });
    expect(winAdd.mock.calls.map((c) => c[0])).toEqual(expect.arrayContaining(['error', 'unhandledrejection', 'pagehide']));
    // 'click' is the click breadcrumb integration's listener on document (spec §5.6), installed alongside
    // the hide listener's 'visibilitychange'.
    expect(docAdd.mock.calls.map((c) => c[0])).toEqual(expect.arrayContaining(['visibilitychange', 'click']));
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(procOn).not.toHaveBeenCalled();
    await mod.close(10);
  });
});
