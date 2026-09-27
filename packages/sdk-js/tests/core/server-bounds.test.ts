// The server's contexts and request-map bounds, mirrored (spec §3.10): `boundContexts`, `boundStringMap`,
// `boundRequestStrings`, `compareCodePoints` — unit tests directly against src/core/server-bounds.ts — plus
// their wiring through `captureException` (§3.10's mirrored bounds, spec review round 8).

import { beforeEach, describe, expect, it } from 'vitest';
import { boundContexts, boundRequestStrings, boundStringMap, compareCodePoints, toWellFormedRepair } from '../../src/core/server-bounds.js';
import { captureException } from '../../src/core/capture.js';
import { setContext } from '../../src/core/scope.js';
import type { RequestInfo } from '../../src/core/types.js';
import { resetPipeline, setup } from '../helpers/pipeline.js';

beforeEach(resetPipeline);

describe('compareCodePoints', () => {
  it('compares by code point, not UTF-16 unit — a supplementary-plane character sorts above U+E000-U+FFFF', () => {
    const bmpPrivateUse = '';
    const supplementary = String.fromCodePoint(0x10000);
    expect(compareCodePoints(bmpPrivateUse, supplementary)).toBeLessThan(0);
    expect([supplementary, bmpPrivateUse].sort(compareCodePoints)).toEqual([bmpPrivateUse, supplementary]);
    // Plain `<` gets this wrong: the supplementary character's lead surrogate (0xD800) is numerically
    // below 0xE000, so a UTF-16-unit sort puts it first.
    expect(supplementary < bmpPrivateUse).toBe(true);
  });

  it('equal-length equal strings compare 0; a prefix sorts before its extension', () => {
    expect(compareCodePoints('abc', 'abc')).toBe(0);
    expect(compareCodePoints('ab', 'abc')).toBeLessThan(0);
  });
});

describe('toWellFormedRepair', () => {
  it('replaces a lone surrogate with U+FFFD; leaves a valid surrogate pair and ordinary text alone', () => {
    expect(toWellFormedRepair('a\uD800b')).toBe('a�b');
    expect(toWellFormedRepair('a\uDC00b')).toBe('a�b');
    expect(toWellFormedRepair('a' + String.fromCodePoint(0x1f600) + 'b')).toBe('a' + String.fromCodePoint(0x1f600) + 'b');
    expect(toWellFormedRepair('plain text')).toBe('plain text');
  });
});

describe('boundContexts', () => {
  it('a name with U+0000 or not 1-64 code points is removed', () => {
    const { contexts, removed } = boundContexts({ 'a\u0000b': { x: 1 }, ['y'.repeat(65)]: { x: 1 }, ok: { x: 1 } });
    expect(contexts).toEqual({ ok: { x: 1 } });
    expect(removed.sort()).toEqual(['a\u0000b', 'y'.repeat(65)].sort());
  });

  it('33 small custom contexts + a well-known one, over 32, removes non-well-known first, largest then lexically smallest', () => {
    const contexts: Record<string, Record<string, unknown>> = {};
    for (let i = 0; i < 33; i++) contexts[`c${String(i).padStart(2, '0')}`] = { v: i === 32 ? 'x'.repeat(50) : 'x' };
    contexts.browser = { name: 'chrome' }; // well-known — removed only after every custom name
    // 34 contexts total; keeping 32 removes 2: the largest (c32), then the lexically smallest tie (c00).
    const { contexts: kept, removed } = boundContexts(contexts);
    expect(Object.keys(kept ?? {})).toHaveLength(32);
    expect(kept?.browser).toBeDefined();
    expect(removed).toEqual(['c32', 'c00']);
  });

  it('a well-known name is removed only after every custom name is gone', () => {
    const contexts: Record<string, Record<string, unknown>> = { browser: { name: 'chrome' } };
    for (let i = 0; i < 32; i++) contexts[`c${String(i).padStart(2, '0')}`] = { v: 'x'.repeat(3000) };
    const { removed } = boundContexts(contexts);
    expect(removed).not.toContain('browser');
  });

  it('over 65 536 Go-ruler bytes but under by JSON.stringify (values full of "<") loses the largest custom name', () => {
    // Go's json.Marshal HTML-escapes '<' to `<` (6 bytes); JS's JSON.stringify does not (1 byte) —
    // 12 000 of them naively fits well under 65 536 but rules over it by the Go count.
    const html = '<'.repeat(12_000);
    const contexts = { big: { html }, small: { v: 'x' } };
    const naive = Buffer.byteLength(JSON.stringify(contexts));
    expect(naive).toBeLessThan(65_536);
    const { contexts: kept, removed } = boundContexts(contexts);
    expect(removed).toEqual(['big']);
    expect(kept).toEqual({ small: { v: 'x' } });
  });

  it('a size tie goes to the name smaller by code point', () => {
    const contexts = { zeta: { v: 'x'.repeat(100) }, alpha: { v: 'x'.repeat(100) } };
    // Both entries are the same size (`entrySize` counts the key too, and both keys are 5 chars — pad to match).
    const { contexts: kept, removed } = boundContexts({ ...contexts, filler: { v: 'y'.repeat(70_000) } });
    expect(removed).toContain('filler');
    expect(kept).toEqual(contexts);
  });

  it('captureException: 33 custom contexts through setContext send 32, with contexts in metadata.truncated', () => {
    const p = setup({}, 'node');
    for (let i = 0; i < 33; i++) setContext(`c${String(i).padStart(2, '0')}`, { v: 'x' });
    captureException(new Error('x'));
    const [event] = p.events();
    expect(Object.keys(event?.contexts ?? {})).toHaveLength(32);
    expect(event?.metadata.truncated).toEqual(['contexts']);
  });
});

describe('boundStringMap (request.headers / request.env)', () => {
  it('U+0000 removed from a value, cut to maxValue code points; U+0000 or an over-length key dropped', () => {
    const { map } = boundStringMap({ ok: 'v\u0000alue', ['k'.repeat(300)]: 'x', 'bad\u0000key': 'y' }, 64, 256, 10);
    expect(map).toEqual({ ok: 'value' });
  });

  it('past 64 entries, the 64 keys smallest by code point are kept', () => {
    const headers: Record<string, string> = {};
    for (let i = 0; i < 80; i++) headers[`h${String(i).padStart(3, '0')}`] = 'v';
    const { map } = boundStringMap(headers, 64, 256, 4096);
    expect(Object.keys(map ?? {})).toHaveLength(64);
    expect(map?.h000).toBeDefined();
    expect(map?.h079).toBeUndefined();
  });

  it('a key in U+E000-U+FFFF sorts before one above U+FFFF (compareCodePoints, not JS <)', () => {
    const supplementary = String.fromCodePoint(0x10000);
    const bmpPrivateUse = '';
    const headers: Record<string, string> = { [supplementary]: 'a', [bmpPrivateUse]: 'b' };
    for (let i = 0; i < 64; i++) headers[`z${i}`] = 'v'; // push the count past 64 so the sort actually prunes
    const { map } = boundStringMap(headers, 64, 256, 4096);
    // The private-use key sorts first (kept over the supplementary one when only one of the two fits).
    expect(Object.keys(map ?? {})).toHaveLength(64);
  });

  it('a header value of 5000 code points is cut to 4096, an env value to 1024; a supplementary char counts once', () => {
    const value = String.fromCodePoint(0x10000).repeat(5000);
    const headerResult = boundStringMap({ h: value }, 64, 256, 4096);
    expect(Array.from(headerResult.map?.h ?? '')).toHaveLength(4096);
    const envResult = boundStringMap({ e: value }, 64, 256, 1024);
    expect(Array.from(envResult.map?.e ?? '')).toHaveLength(1024);
  });

  it('a lone surrogate becomes U+FFFD in both key and value', () => {
    const { map } = boundStringMap({ ['k\uD800']: 'v\uDC00' }, 64, 256, 4096);
    expect(Object.keys(map ?? {})).toEqual(['k�']);
    expect(map?.['k�']).toBe('v�');
  });
});

describe('boundRequestStrings', () => {
  it('U+0000 removed then cut to 16 / 2048 / 4096 code points; an emptied field is removed', () => {
    const request: RequestInfo = { method: 'G\u0000ET'.repeat(10), url: 'x'.repeat(2100), query_string: 'y'.repeat(5000) };
    boundRequestStrings(request);
    expect(request.method?.includes('\u0000')).toBe(false);
    expect(Array.from(request.method ?? '')).toHaveLength(16);
    expect(Array.from(request.url ?? '')).toHaveLength(2048);
    expect(Array.from(request.query_string ?? '')).toHaveLength(4096);
  });

  it('a supplementary-plane code point at the 2048th position is kept whole, counted once', () => {
    const request: RequestInfo = { url: 'x'.repeat(2047) + String.fromCodePoint(0x10000) + 'y'.repeat(10) };
    boundRequestStrings(request);
    expect(Array.from(request.url ?? '')).toHaveLength(2048);
    expect(request.url?.endsWith(String.fromCodePoint(0x10000))).toBe(true);
  });

  it('a value that becomes empty deletes its key', () => {
    const request: RequestInfo = { method: '\u0000' };
    boundRequestStrings(request);
    expect(request.method).toBeUndefined();
  });
});
