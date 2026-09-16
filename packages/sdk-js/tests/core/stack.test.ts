// B2 (stack parsing per engine format, innermost first) and B4 (the `in_app` rule table and the `inApp`
// override) of flows.md, against contract §4.3's `stacktrace[]` rows. Shuffle-safe: every test starts from a
// deleted `globalThis` slot.

import { beforeEach, describe, expect, it } from 'vitest';
import { captureException } from '../../src/core/capture.js';
import { defaultInApp, parseStack } from '../../src/core/stack.js';
import { resetPipeline, setup, TURBOPACK_CHUNK } from '../helpers/pipeline.js';

beforeEach(resetPipeline);

function errorWithStack(stack: string, message = 'boom'): Error {
  const err = new Error(message);
  err.stack = stack;
  return err;
}

const V8_STACK = [
  'TypeError: Cannot read properties of undefined',
  '    at render (https://app/_next/static/chunks/app/page-9b0d4e.js:12:34)',
  '    at https://app/_next/static/chunks/8f3a1c2b9d4e7f60.js:1:2',
  '    at Module._compile (node:internal/modules/cjs/loader:1256:14)',
  '    at fn (native)',
  '    at fn2 (<anonymous>)',
].join('\n');

const SPIDERMONKEY_STACK = [
  'render@https://app/_next/static/chunks/app/page-9b0d4e.js:12:34',
  '@https://app/_next/static/chunks/8f3a1c2b9d4e7f60.js:1:2',
  'promiseReactionJob@[native code]',
].join('\n');

const JSC_STACK = ['render@https://app/static/app.js:12:34', 'global code@https://app/static/app.js:1:1', 'fn@[native code]'].join('\n');

describe('B2: error.stack parsed per engine format, innermost first', () => {
  it('B2: a V8 stack — `at fn (file:line:col)`, `at file:line:col`, native and <anonymous> locations', () => {
    expect(parseStack(V8_STACK)).toEqual([
      { file: 'https://app/_next/static/chunks/app/page-9b0d4e.js', line: 12, column: 34, function: 'render' },
      { file: TURBOPACK_CHUNK, line: 1, column: 2, function: '<anonymous>' },
      { file: 'node:internal/modules/cjs/loader', line: 1256, column: 14, function: 'Module._compile' },
      { file: '', function: 'fn' },
      { file: '', function: 'fn2' },
    ]);
  });

  it('B2: a SpiderMonkey stack — `fn@file:line:col`, anonymous `@file`, `[native code]`', () => {
    expect(parseStack(SPIDERMONKEY_STACK)).toEqual([
      { file: 'https://app/_next/static/chunks/app/page-9b0d4e.js', line: 12, column: 34, function: 'render' },
      { file: TURBOPACK_CHUNK, line: 1, column: 2, function: '<anonymous>' },
      { file: '', function: 'promiseReactionJob' },
    ]);
  });

  it('B2: a JavaScriptCore stack — `global code@file:line:col`, `fn@[native code]`', () => {
    expect(parseStack(JSC_STACK)).toEqual([
      { file: 'https://app/static/app.js', line: 12, column: 34, function: 'render' },
      { file: 'https://app/static/app.js', line: 1, column: 1, function: 'global code' },
      { file: '', function: 'fn' },
    ]);
  });

  it('B2: an unknown column is omitted, never sent as 0; a non-string stack is []', () => {
    expect(parseStack('    at fn (https://app/x.js:7)')).toEqual([{ file: 'https://app/x.js', line: 7, function: 'fn' }]);
    expect(parseStack(undefined)).toEqual([]);
    expect(parseStack({ toString: () => 'at x' })).toEqual([]);
    // A subclass whose `stack` getter returns an object → [] (contract §4.3: read as a string, never coerced).
    const objectStack = new Error('x');
    Object.defineProperty(objectStack, 'stack', { get: () => ({}) });
    const p = setup();
    captureException(objectStack);
    expect(p.events()[0]?.stacktrace).toEqual([]);
  });

  it('B2: the five V8 forms — async / new / Object.<anonymous> / [as alias] / eval taking the outer location', () => {
    const stack = [
      'Error: five forms',
      '    at async loadPage (https://app/_next/static/chunks/app/page-9b0d4e.js:5:6)',
      '    at new Widget (https://app/_next/static/chunks/app/page-9b0d4e.js:7:8)',
      '    at Object.<anonymous> (/srv/app/.next/server/app/page.js:9:10)',
      '    at handler [as onClick] (https://app/_next/static/chunks/app/page-9b0d4e.js:11:12)',
      '    at eval (eval at outer (https://app/_next/static/chunks/app/page-9b0d4e.js:13:14), <anonymous>:1:1)',
      '    at async https://app/_next/static/chunks/app/page-9b0d4e.js:15:16',
    ].join('\n');
    expect(parseStack(stack)).toEqual([
      { file: 'https://app/_next/static/chunks/app/page-9b0d4e.js', line: 5, column: 6, function: 'loadPage' },
      { file: 'https://app/_next/static/chunks/app/page-9b0d4e.js', line: 7, column: 8, function: 'Widget' },
      { file: '/srv/app/.next/server/app/page.js', line: 9, column: 10, function: 'Object.<anonymous>' },
      { file: 'https://app/_next/static/chunks/app/page-9b0d4e.js', line: 11, column: 12, function: 'handler' },
      { file: 'https://app/_next/static/chunks/app/page-9b0d4e.js', line: 13, column: 14, function: 'eval' },
      { file: 'https://app/_next/static/chunks/app/page-9b0d4e.js', line: 15, column: 16, function: '<anonymous>' },
    ]);
  });

  it('B2: frames reach the event innermost first with in_app decided, and a stackless Error has []', () => {
    const p = setup();
    captureException(errorWithStack(V8_STACK));
    const [event] = p.events();
    expect(event?.stacktrace.map((f) => f.function)).toEqual(['render', '<anonymous>', 'Module._compile', 'fn', 'fn2']);
    expect(event?.stacktrace.map((f) => f.in_app)).toEqual([true, true, false, false, false]);
    const stackless = new Error('no stack');
    delete stackless.stack;
    captureException(stackless);
    expect(p.events()[1]?.stacktrace).toEqual([]);
  });
});

describe('B4: the in_app rule table (decision 27) and the inApp override', () => {
  // ≥ 15 cases: every webpack framework-chunk pattern, node:, node_modules, empty, and the Turbopack chunk.
  const table: [string, boolean][] = [
    ['', false],
    ['node:internal/modules/cjs/loader', false],
    ['node:fs', false],
    ['/srv/app/node_modules/react-dom/cjs/react-dom.production.js', false],
    ['https://app/_next/static/chunks/node_modules/next/dist/client/app-index.js', false],
    ['https://app/_next/static/chunks/framework-2f1e6d9c3a4b5e7f.js', false],
    ['https://app/_next/static/chunks/main-1a2b3c4d5e6f7a8b.js', false],
    ['https://app/_next/static/chunks/main-app-9f8e7d6c5b4a3210.js', false],
    ['https://app/_next/static/chunks/webpack-0a1b2c3d4e5f6a7b.js', false],
    ['https://app/_next/static/chunks/polyfills-42.js', false],
    ['https://app/_next/static/chunks/framework.js', false],
    ['https://app/_next/static/chunks/pages/_app-abcdef0123456789.js', false],
    ['webpack-internal:///(app-pages-browser)/./node_modules/react/index.js', false],
    ['webpack-internal:///(app-pages-browser)/./src/app/page.tsx', true],
    [TURBOPACK_CHUNK, true],
    ['https://app/_next/static/chunks/app/page-9b0d4e.js', true],
    ['https://app/_next/static/chunks/mainframe-1234.js', true],
    ['/srv/app/.next/server/app/page.js', true],
    ['https://app/src/framework-utils.js', true],
  ];

  it.each(table)('B4: file %j → in_app %s', (file, expected) => {
    expect(defaultInApp(file)).toBe(expected);
  });

  it('B4: a Turbopack production chunk <16-hex>.js is in_app: true on the wire', () => {
    const p = setup();
    captureException(errorWithStack(`Error: x\n    at render (${TURBOPACK_CHUNK}:1:2)`));
    expect(p.events()[0]?.stacktrace).toEqual([{ file: TURBOPACK_CHUNK, line: 1, column: 2, function: 'render', in_app: true }]);
  });

  it('B4: inApp receives the frame with its default in_app already set and refines it', () => {
    const seen: boolean[] = [];
    const p = setup({
      inApp: (frame) => {
        seen.push(frame.in_app);
        return frame.in_app && !frame.file.includes('/vendor/');
      },
    });
    captureException(
      errorWithStack(
        ['Error: x', '    at a (https://app/vendor/lib.js:1:1)', '    at b (https://app/app.js:2:2)', '    at c (node:fs:3:3)'].join('\n'),
      ),
    );
    expect(seen).toEqual([true, true, false]);
    expect(p.events()[0]?.stacktrace.map((f) => f.in_app)).toEqual([false, true, false]);
  });

  it('B4: an inApp returning undefined keeps the default; returning true for an empty file is still never in_app', () => {
    const p = setup({ inApp: () => undefined });
    captureException(errorWithStack('Error: x\n    at a (https://app/app.js:1:1)\n    at b (node:fs:3:3)'));
    expect(p.events()[0]?.stacktrace.map((f) => f.in_app)).toEqual([true, false]);
    resetPipeline();
    const q = setup({ inApp: () => true });
    captureException(errorWithStack('Error: x\n    at fn (native)'));
    expect(q.events()[0]?.stacktrace).toEqual([{ file: '', function: 'fn', in_app: false }]);
  });

  it('B4: a throwing inApp leaves the default, prints one debug line per capture and fires no onError', () => {
    const p = setup({
      inApp: () => {
        throw new Error('inApp broke');
      },
    });
    const id = captureException(errorWithStack('Error: x\n    at a (https://app/app.js:1:1)\n    at b (node:fs:3:3)'));
    expect(id).toMatch(/^[0-9a-f-]{36}$/);
    expect(p.events()[0]?.stacktrace.map((f) => f.in_app)).toEqual([true, false]);
    expect(p.errors).toEqual([]);
    expect(p.debugLines.filter((l) => l.includes('inApp threw'))).toHaveLength(1);
  });
});
