// Stack parsing and the `in_app` rule (contract §4.3, `stacktrace[]` rows; decision 27).
//
// `error.stack` is read as a string and parsed as printed — the SDK never assigns `Error.prepareStackTrace`
// (Next's production server owns it) and never calls `Error.captureStackTrace` on the caller's object. Frames
// stay in the order the engine prints them, innermost first, which is what `fingerprint.Compute` reads as "the
// first three in_app frames" and what `sdk-go` sends (D5).

import type { StackFrame } from './types.js';

/** A parsed frame before the `in_app` decision: the location fields only. */
export interface RawFrame {
  file: string;
  line?: number;
  column?: number;
  function: string;
}

const ANONYMOUS = '<anonymous>';

/** `native`, `<anonymous>`, `[native code]` and an empty location all mean "no file" → `""` (contract §4.3). */
function normaliseFile(file: string): string {
  const f = file.trim();
  if (f === '' || f === 'native' || f === ANONYMOUS || f === '[native code]') return '';
  return f;
}

/** `file:line:col` / `file:line` / `file` — line and column only when printed and positive. */
function parseLocation(location: string): { file: string; line?: number; column?: number } {
  let m = /^(.*):(\d+):(\d+)$/.exec(location);
  if (m) {
    const out: { file: string; line?: number; column?: number } = { file: normaliseFile(m[1] as string) };
    const line = Number(m[2]);
    const column = Number(m[3]);
    if (line > 0) out.line = line;
    if (column > 0) out.column = column;
    return out;
  }
  m = /^(.*):(\d+)$/.exec(location);
  if (m) {
    const out: { file: string; line?: number } = { file: normaliseFile(m[1] as string) };
    const line = Number(m[2]);
    if (line > 0) out.line = line;
    return out;
  }
  return { file: normaliseFile(location) };
}

/**
 * An eval frame's location — `eval at outer (file:line:col), <anonymous>:1:1` — takes the OUTER location, the
 * `file:line:col` of the `eval at` clause; nested evals unwrap until a plain location remains (contract §4.3).
 */
function unwrapEval(location: string): string {
  let loc = location;
  for (;;) {
    const m = /^eval at [^(]*\((.*)\), [^,]*$/.exec(loc);
    if (!m) return loc;
    loc = m[1] as string;
  }
}

/** `async ` / `new ` prefixes and the ` [as …]` suffix stripped, nothing else rewritten (contract §4.3). */
function normaliseFunction(name: string): string {
  let fn = name.trim();
  if (fn.startsWith('async ')) fn = fn.slice(6);
  if (fn.startsWith('new ')) fn = fn.slice(4);
  fn = fn.replace(/ \[as [^\]]*\]$/, '');
  fn = fn.trim();
  return fn === '' ? ANONYMOUS : fn;
}

/** One V8 line, already known to start with `at `. Returns `undefined` for a line that is not a frame. */
function parseV8Line(line: string): RawFrame | undefined {
  const body = line.slice(3).trim();
  if (body === '') return undefined;
  // `at fn (location)` — the location is the LAST parenthesised group so `at fn [as alias] (…)` and a
  // function name containing parentheses both resolve; `at fn (eval at outer (…), <anonymous>:1:1)` keeps the
  // whole eval clause because the closing parenthesis matched is the final one.
  const m = /^(.*?) \((.*)\)$/.exec(body);
  if (m) {
    const location = unwrapEval(m[2] as string);
    return { ...parseLocation(location), function: normaliseFunction(m[1] as string) };
  }
  // `at location` — an anonymous frame; `at async location` is an async one.
  let location = body;
  if (location.startsWith('async ')) location = location.slice(6);
  return { ...parseLocation(unwrapEval(location)), function: ANONYMOUS };
}

/** One SpiderMonkey / JavaScriptCore line: `fn@file:line:col`, `@file:line:col`, `fn@[native code]`. */
function parseAtSignLine(line: string): RawFrame | undefined {
  // The FIRST `@`: a function name never contains one, a file URL may (`https://cdn/@scope/pkg.js`).
  const at = line.indexOf('@');
  if (at < 0) return undefined;
  const fn = line.slice(0, at);
  const location = line.slice(at + 1);
  // SpiderMonkey prints eval frames as `fn@file line 2 > eval:1:1` etc.; the location parser reads whatever
  // `file:line:col` tail is printed and leaves the rest as the file.
  return { ...parseLocation(location), function: normaliseFunction(fn) };
}

/**
 * Parse `error.stack` as printed. Returns `[]` for a non-string. V8 stacks (`Name: message` header lines then
 * `    at …`) are recognised by the `at ` lines; anything else is read as the header-less `fn@file:line:col`
 * form SpiderMonkey and JavaScriptCore print.
 */
export function parseStack(stack: unknown): RawFrame[] {
  if (typeof stack !== 'string' || stack === '') return [];
  const lines = stack.split('\n');
  const frames: RawFrame[] = [];
  const isV8 = lines.some((l) => /^\s+at /.test(l));
  for (const raw of lines) {
    const line = raw.trim();
    if (line === '') continue;
    if (isV8) {
      if (!/^\s+at /.test(raw)) continue;
      const frame = parseV8Line(line);
      if (frame) frames.push(frame);
    } else {
      const frame = parseAtSignLine(line);
      if (frame) frames.push(frame);
    }
  }
  return frames;
}

// The webpack-named framework / runtime chunks the default rule can see through (contract §4.3, decision 27).
const WEBPACK_FRAMEWORK_CHUNK = /\/_next\/static\/chunks\/(framework|main|main-app|webpack|polyfills)[-.]/;
const WEBPACK_PAGES_APP_CHUNK = /\/_next\/static\/chunks\/pages\/_app-/;
const WEBPACK_INTERNAL_NODE_MODULES = /^webpack-internal:\/\/\/\([^)]*\)\/\.\/node_modules\//;

/**
 * The default `in_app` rule (contract §4.3): `false` when `file` is `""`, contains `/node_modules/`, starts with
 * `node:`, or names a webpack framework / runtime chunk; `true` otherwise — which on a Turbopack production
 * build is every content-hashed `<16-hex>.js` chunk, because the rule cannot see through the name (decision 27).
 */
export function defaultInApp(file: string): boolean {
  if (file === '') return false;
  if (file.includes('/node_modules/')) return false;
  if (file.startsWith('node:')) return false;
  if (WEBPACK_FRAMEWORK_CHUNK.test(file)) return false;
  if (WEBPACK_PAGES_APP_CHUNK.test(file)) return false;
  if (WEBPACK_INTERNAL_NODE_MODULES.test(file)) return false;
  return true;
}

/**
 * Decide `in_app` for one frame: the default rule, then `init.inApp` called with that default already set
 * (contract §4.2 — the override is a refinement, `frame => frame.in_app && …`); a boolean return overrides,
 * anything else keeps the default, and a throwing `inApp` keeps the default too (the caller prints one `debug`
 * line per capture, no `onError`). A frame whose `file` is `""` is never `in_app` (proto CEL `!in_app || file != ""`).
 */
export function decideInApp(
  frame: RawFrame,
  inApp: ((frame: StackFrame) => boolean | undefined) | undefined,
): { frame: StackFrame; threw: boolean } {
  const withDefault: StackFrame = { ...frame, in_app: defaultInApp(frame.file) };
  let threw = false;
  if (inApp !== undefined) {
    try {
      const decided = inApp({ ...withDefault });
      if (typeof decided === 'boolean') withDefault.in_app = decided;
    } catch {
      threw = true;
    }
  }
  if (withDefault.file === '') withDefault.in_app = false;
  return { frame: withDefault, threw };
}
