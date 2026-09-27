// The server's `contexts` and request-map bounds, mirrored (spec §3.10, TASK-01, for §4.6): so the ingestor's
// own bounds step (`apps/ingestor-go/validation/bounds.go`) removes nothing from what the SDK sends. Runs on
// every runtime, at `build` and again in `afterHook`, both times before the metadata shrink.

import { pairBytes } from './event.js';
import type { JsonValue } from './serialize.js';
import type { RequestInfo } from './types.js';

export const MAX_CONTEXTS = 32;
export const MAX_CONTEXTS_SIZE = 65_536;
export const MAX_CONTEXT_KEY_CODEPOINTS = 64;
export const MAX_REQUEST_HEADERS = 64;
export const MAX_REQUEST_HEADER_KEY_CODEPOINTS = 256;
export const MAX_REQUEST_HEADER_VALUE_CODEPOINTS = 4096;
export const MAX_REQUEST_ENV_VALUE_CODEPOINTS = 1024;
export const MAX_REQUEST_METHOD_CODEPOINTS = 16;
export const MAX_REQUEST_URL_CODEPOINTS = 2048;
export const MAX_REQUEST_QUERY_STRING_CODEPOINTS = 4096;

/** The six names removed last when `contexts` is over (`bounds.go: wellKnownContexts`); also §4.6 step 3's order. */
export const WELL_KNOWN_CONTEXTS: ReadonlySet<string> = new Set(['device', 'os', 'app', 'runtime', 'browser', 'culture']);

function codePointArray(s: string): string[] {
  return Array.from(s);
}

/**
 * Compares two strings BY CODE POINT, not by UTF-16 unit (Go's `sort.Strings`, byte order on UTF-8, which
 * preserves code-point order): a key in U+E000-U+FFFF and a key above U+FFFF sort the way Go sorts them,
 * which plain JS `<` gets wrong for a supplementary-plane character (its surrogate pair's UTF-16 units are
 * numerically below U+E000).
 */
export function compareCodePoints(a: string, b: string): number {
  const ca = codePointArray(a);
  const cb = codePointArray(b);
  const len = Math.min(ca.length, cb.length);
  for (let i = 0; i < len; i++) {
    const pa = (ca[i] as string).codePointAt(0) as number;
    const pb = (cb[i] as string).codePointAt(0) as number;
    if (pa !== pb) return pa - pb;
  }
  return ca.length - cb.length;
}

/** Every lone surrogate becomes U+FFFD — what Go's JSON decoder stores for it (a `charCodeAt` loop, §5.1). */
export function toWellFormedRepair(s: string): string {
  let out = '';
  let changed = false;
  for (let i = 0; i < s.length; i++) {
    const code = s.charCodeAt(i);
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = s.charCodeAt(i + 1);
      if (next >= 0xdc00 && next <= 0xdfff) {
        out += s.charAt(i) + s.charAt(i + 1);
        i++;
        continue;
      }
      out += '�';
      changed = true;
    } else if (code >= 0xdc00 && code <= 0xdfff) {
      out += '�';
      changed = true;
    } else {
      out += s.charAt(i);
    }
  }
  return changed ? out : s;
}

function cutCodePoints(s: string, max: number): { value: string; cut: boolean } {
  const cps = codePointArray(s);
  return cps.length > max ? { value: cps.slice(0, max).join(''), cut: true } : { value: s, cut: false };
}

interface SizedKey {
  key: string;
  size: number;
  last: boolean;
}

/** `bounds.go: sortVictims` — non-well-known before well-known, largest first, a tie to the smaller code point. */
function sortVictims(entries: SizedKey[]): SizedKey[] {
  return entries.slice().sort((a, b) => {
    if (a.last !== b.last) return a.last ? 1 : -1;
    if (a.size !== b.size) return b.size - a.size;
    return compareCodePoints(a.key, b.key);
  });
}

function objectSize(body: number, n: number): number {
  return n > 1 ? body + 2 + (n - 1) : body + 2;
}

/**
 * `boundContexts` (spec §3.10): after each value has passed §3.6's `sanitize` + `isJsonObject`. A U+0000 name
 * or one not 1-64 code points is removed; then, while over 32 names or 65 536 Go-ruler bytes, the largest
 * non-well-known context goes first, well-known ones last, a size tie to the smaller code point. Returns
 * whether anything was removed (the caller adds `'contexts'` to `metadata.truncated`) and the names removed,
 * for the debug line.
 */
export function boundContexts(contexts: Record<string, Record<string, unknown>>): { contexts: Record<string, Record<string, unknown>> | undefined; removed: string[] } {
  const removed: string[] = [];
  const kept: Record<string, Record<string, unknown>> = {};
  for (const name of Object.keys(contexts)) {
    const n = codePointArray(name).length;
    if (name.includes('\u0000') || n < 1 || n > MAX_CONTEXT_KEY_CODEPOINTS) {
      removed.push(name);
      continue;
    }
    kept[name] = contexts[name] as Record<string, unknown>;
  }

  let body = 0;
  const candidates: SizedKey[] = [];
  for (const key of Object.keys(kept)) {
    const size = pairBytes(key, kept[key] as unknown as JsonValue);
    candidates.push({ key, size, last: WELL_KNOWN_CONTEXTS.has(key) });
    body += size;
  }
  const victims = sortVictims(candidates);
  let count = candidates.length;
  for (const victim of victims) {
    if (count <= MAX_CONTEXTS && objectSize(body, count) <= MAX_CONTEXTS_SIZE) break;
    delete kept[victim.key];
    body -= victim.size;
    count -= 1;
    removed.push(victim.key);
  }

  if (Object.keys(kept).length === 0) return { contexts: undefined, removed };
  return { contexts: kept, removed };
}

/**
 * `boundStringMap` (spec §3.10): well-formed first (lone surrogates → U+FFFD), a U+0000 or non-1..maxKey-
 * code-point key dropped, values NUL-stripped and cut to `maxValue` code points, then past `maxEntries` the
 * lexically (by code point) LARGEST keys are dropped. Returns `undefined` when the result is empty.
 */
export function boundStringMap(map: Record<string, string>, maxEntries: number, maxKey: number, maxValue: number): { map: Record<string, string> | undefined; changed: boolean } {
  let changed = false;
  const out: Record<string, string> = {};
  for (const rawKey of Object.keys(map)) {
    const key = toWellFormedRepair(rawKey);
    if (key !== rawKey) changed = true;
    const n = codePointArray(key).length;
    if (key.includes('\u0000') || n < 1 || n > maxKey) {
      changed = true;
      continue;
    }
    let value = toWellFormedRepair(String(map[rawKey]));
    if (value.includes('\u0000')) {
      value = value.replace(/\u0000/g, '');
      changed = true;
    }
    const cut = cutCodePoints(value, maxValue);
    if (cut.cut) changed = true;
    out[key] = cut.value;
  }
  const keys = Object.keys(out).sort(compareCodePoints);
  if (keys.length > maxEntries) {
    changed = true;
    for (const key of keys.slice(maxEntries)) delete out[key];
  }
  if (Object.keys(out).length === 0) return { map: undefined, changed };
  return { map: out, changed };
}

/**
 * `boundRequestStrings` (spec §3.10): U+0000 removed from `method`/`url`/`query_string`, then each cut to the
 * server's bound in code points. A value left empty deletes its key. Mutates `request` in place; returns
 * whether anything changed (for the debug line — no `metadata.truncated` marker, as for headers/env).
 */
export function boundRequestStrings(request: RequestInfo): boolean {
  let changed = false;
  const strip = (value: string): string => {
    if (!value.includes('\u0000')) return value;
    changed = true;
    return value.replace(/\u0000/g, '');
  };
  const apply = (key: 'method' | 'url' | 'query_string', max: number): void => {
    const raw = request[key];
    if (typeof raw !== 'string') return;
    const stripped = strip(raw);
    const cut = cutCodePoints(stripped, max);
    if (cut.cut) changed = true;
    if (cut.value === '') delete request[key];
    else request[key] = cut.value;
  };
  apply('method', MAX_REQUEST_METHOD_CODEPOINTS);
  apply('url', MAX_REQUEST_URL_CODEPOINTS);
  apply('query_string', MAX_REQUEST_QUERY_STRING_CODEPOINTS);
  return changed;
}
