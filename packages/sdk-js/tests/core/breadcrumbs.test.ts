// The breadcrumb buffer (spec §4.1-§4.4, TASK-02 §11.2): the ring's capacity and `maxBreadcrumbs` degradation,
// `addBreadcrumb`'s normalisation, `beforeBreadcrumb`'s fail-closed application, the copy taken at capture, and
// the 131 072-byte total. Shuffle-safe: every test starts from a deleted slot.

import { beforeEach, describe, expect, it } from 'vitest';
import { addBreadcrumb, captureException } from '../../src/core/index.js';
import { MAX_BREADCRUMB_DATA_BYTES, MAX_BREADCRUMBS_TOTAL_BYTES } from '../../src/core/breadcrumbs.js';
import { initCore } from '../../src/core/init.js';
import { metadataWireBytes } from '../../src/core/serialize.js';
import { getSlot } from '../../src/core/state.js';
import type { Breadcrumb, BreadcrumbHint } from '../../src/core/types.js';
import { resetPipeline, setup } from '../helpers/pipeline.js';

beforeEach(resetPipeline);

describe('the ring and maxBreadcrumbs (spec §4.1)', () => {
  it('150 adds keep only the last 100 (the default), oldest first', () => {
    setup();
    for (let i = 0; i < 150; i++) addBreadcrumb({ message: `m${i}` });
    const crumbs = getSlot().scope.breadcrumbs;
    expect(crumbs).toHaveLength(100);
    expect(crumbs[0]?.message).toBe('m50');
    expect(crumbs[99]?.message).toBe('m149');
  });

  it('maxBreadcrumbs: 5 keeps only the last 5', () => {
    setup({ maxBreadcrumbs: 5 });
    for (let i = 0; i < 20; i++) addBreadcrumb({ message: `m${i}` });
    const crumbs = getSlot().scope.breadcrumbs;
    expect(crumbs).toHaveLength(5);
    expect(crumbs.map((c) => c.message)).toEqual(['m15', 'm16', 'm17', 'm18', 'm19']);
  });

  it('maxBreadcrumbs: 0 disables breadcrumbs — addBreadcrumb is a no-op', () => {
    setup({ maxBreadcrumbs: 0 });
    addBreadcrumb({ message: 'x' });
    expect(getSlot().scope.breadcrumbs).toHaveLength(0);
  });

  it('maxBreadcrumbs: 101 degrades to 100 with invalid_option; a non-integer / negative degrades to 100', () => {
    const p1 = setup({ maxBreadcrumbs: 101 });
    expect(p1.errors.map((e) => e.code)).toEqual(['invalid_option']);
    resetPipeline();
    const p2 = setup({ maxBreadcrumbs: -1 });
    expect(p2.errors.map((e) => e.code)).toEqual(['invalid_option']);
    resetPipeline();
    const p3 = setup({ maxBreadcrumbs: 1.5 });
    expect(p3.errors.map((e) => e.code)).toEqual(['invalid_option']);
    for (let i = 0; i < 150; i++) addBreadcrumb({ message: `m${i}` });
    expect(getSlot().scope.breadcrumbs).toHaveLength(100);
  });

  it('on a second init the buffer is trimmed to the new capacity (oldest dropped)', () => {
    setup({ maxBreadcrumbs: 10 });
    for (let i = 0; i < 10; i++) addBreadcrumb({ message: `m${i}` });
    initCore({ maxBreadcrumbs: 3, debug: true }, 'browser');
    const crumbs = getSlot().scope.breadcrumbs;
    expect(crumbs).toHaveLength(3);
    expect(crumbs.map((c) => c.message)).toEqual(['m7', 'm8', 'm9']);
  });

  it('a retained pre-init crumb is dropped by a beforeBreadcrumb installed at init (spec §4.1)', () => {
    addBreadcrumb({ message: 'keep-me' });
    addBreadcrumb({ message: 'drop-me' });
    initCore({ debug: true, beforeBreadcrumb: (c) => (c.message === 'drop-me' ? null : c) }, 'browser');
    const crumbs = getSlot().scope.breadcrumbs;
    expect(crumbs.map((c) => c.message)).toEqual(['keep-me']);
  });
});

describe('addBreadcrumb normalisation (spec §4.2)', () => {
  it("addBreadcrumb('x') (not a plain object) is a no-op with a debug line", () => {
    const p = setup();
    addBreadcrumb('x' as unknown as Breadcrumb);
    expect(getSlot().scope.breadcrumbs).toHaveLength(0);
    expect(p.debugLines.some((l) => l.includes('not a plain object'))).toBe(true);
  });

  it('type cut to 64, category to 128, message to 8192 code points; absent type defaults to "default"; level defaults to info for an invalid value', () => {
    setup();
    addBreadcrumb({ type: 'x'.repeat(70), category: 'y'.repeat(140), message: 'z'.repeat(8200), level: 'nope' as never });
    addBreadcrumb({ message: 'no type given' });
    const [c1, c2] = getSlot().scope.breadcrumbs;
    expect(c1?.type).toBe('x'.repeat(64));
    expect(c1?.category).toBe('y'.repeat(128));
    expect(c1?.message).toBe('z'.repeat(8192));
    expect(c1?.level).toBe('info');
    expect(c2?.type).toBe('default');
  });

  it.each([
    ['a Date', new Date(0)],
    ['Unix seconds', 1_700_000_000],
    ['a Date.parse-able string', '2024-01-01T00:00:00.000Z'],
  ])('timestamp: %s becomes an RFC 3339 ISO string', (_label, value) => {
    setup();
    addBreadcrumb({ timestamp: value as never });
    const [crumb] = getSlot().scope.breadcrumbs;
    expect(crumb?.timestamp).toBe(new Date(crumb?.timestamp as string).toISOString());
    expect(new Date(crumb?.timestamp as string).toISOString()).not.toContain('+');
  });

  it('an absent or unusable timestamp, and one outside years 0000-9999, becomes now', () => {
    setup();
    addBreadcrumb({ message: 'no timestamp' });
    addBreadcrumb({ timestamp: 'not a date' });
    addBreadcrumb({ timestamp: 253_402_300_800 }); // 10000-01-01 in Unix seconds
    const now = Date.now();
    for (const crumb of getSlot().scope.breadcrumbs) {
      expect(Math.abs(new Date(crumb.timestamp).getTime() - now)).toBeLessThan(5000);
      expect(crumb.timestamp).toMatch(/^\d{4}-/);
    }
  });

  it('data: Date / URL / a toJSON returning an array serialise to a non-object — data omitted, crumb kept', () => {
    setup();
    addBreadcrumb({ message: 'a', data: new Date() as unknown as Record<string, unknown> });
    addBreadcrumb({ message: 'b', data: new URL('https://h/p') as unknown as Record<string, unknown> });
    addBreadcrumb({ message: 'c', data: { toJSON: () => [1] } as unknown as Record<string, unknown> });
    addBreadcrumb({ message: 'd', data: { toJSON: () => ({ a: 1 }) } as unknown as Record<string, unknown> });
    const crumbs = getSlot().scope.breadcrumbs;
    expect(crumbs.map((c) => c.data)).toEqual([undefined, undefined, undefined, { a: 1 }]);
    expect(crumbs.map((c) => c.message)).toEqual(['a', 'b', 'c', 'd']);
  });

  it('data over 4096 Go-ruler bytes is removed; the crumb is kept', () => {
    setup();
    addBreadcrumb({ message: 'big', data: { blob: 'x'.repeat(5000) } });
    const [crumb] = getSlot().scope.breadcrumbs;
    expect(crumb?.data).toBeUndefined();
    expect(crumb?.message).toBe('big');
  });

  it('data within the 4096-byte bound is kept and PII-scrubbed', () => {
    setup();
    addBreadcrumb({ message: 'ok', data: { password: 'hunter2', safe: 'kept' } });
    const [crumb] = getSlot().scope.breadcrumbs;
    expect(crumb?.data).toEqual({ password: '[FILTERED]', safe: 'kept' });
    expect(metadataWireBytes(JSON.stringify(crumb?.data))).toBeLessThanOrEqual(MAX_BREADCRUMB_DATA_BYTES);
  });

  it('data.url / .from / .to run through the URL privacy filter, exactly as an automatic crumb does (spec §5.1)', () => {
    setup();
    addBreadcrumb({ message: 'manual', data: { url: '/reset?reset_token=X', from: '/a?token=Y', to: '/b?token=Z', safe: 'kept' } });
    const [crumb] = getSlot().scope.breadcrumbs;
    expect(crumb?.data?.url).toBe('/reset?reset_token=[Filtered]');
    expect(crumb?.data?.from).toBe('/a?token=[Filtered]');
    expect(crumb?.data?.to).toBe('/b?token=[Filtered]');
    expect(crumb?.data?.safe).toBe('kept');
  });
});

describe('beforeBreadcrumb (spec §4.3)', () => {
  it('modifies a crumb — the result is re-normalised', () => {
    setup({ beforeBreadcrumb: (crumb) => ({ ...crumb, message: 'modified', category: 'y'.repeat(200) }) });
    addBreadcrumb({ message: 'original' });
    const [crumb] = getSlot().scope.breadcrumbs;
    expect(crumb?.message).toBe('modified');
    expect(crumb?.category).toBe('y'.repeat(128));
  });

  it('returning null drops the crumb', () => {
    setup({ beforeBreadcrumb: () => null });
    addBreadcrumb({ message: 'x' });
    expect(getSlot().scope.breadcrumbs).toHaveLength(0);
  });

  it('throwing drops the crumb, no onError', () => {
    const p = setup({
      beforeBreadcrumb: () => {
        throw new Error('nope');
      },
    });
    addBreadcrumb({ message: 'x' });
    expect(getSlot().scope.breadcrumbs).toHaveLength(0);
    expect(p.errors).toEqual([]);
    expect(p.debugLines.some((l) => l.includes('beforeBreadcrumb threw'))).toBe(true);
  });

  it('returning a primitive, an array or a promise drops the crumb', () => {
    setup({ beforeBreadcrumb: () => 'a string' as unknown as Breadcrumb });
    addBreadcrumb({ message: 'x' });
    expect(getSlot().scope.breadcrumbs).toHaveLength(0);
    resetPipeline();
    setup({ beforeBreadcrumb: () => [] as unknown as Breadcrumb });
    addBreadcrumb({ message: 'x' });
    expect(getSlot().scope.breadcrumbs).toHaveLength(0);
    resetPipeline();
    setup({ beforeBreadcrumb: () => Promise.resolve(null) as unknown as Breadcrumb });
    addBreadcrumb({ message: 'x' });
    expect(getSlot().scope.breadcrumbs).toHaveLength(0);
  });

  it('receives the hint unchanged', () => {
    const hints: BreadcrumbHint[] = [];
    setup({
      beforeBreadcrumb: (crumb, hint) => {
        hints.push(hint);
        return crumb;
      },
    });
    addBreadcrumb({ message: 'x' }, { input: 'value' });
    expect(hints).toEqual([{ input: 'value' }]);
  });
});

describe('events carry breadcrumbs (spec §4.4)', () => {
  it('the captured event copies the buffer; a later addBreadcrumb / mutation does not change it', () => {
    const p = setup();
    const data = { a: 1 };
    addBreadcrumb({ message: 'first', data });
    captureException(new Error('x'));
    data.a = 2;
    addBreadcrumb({ message: 'second' });
    const [event] = p.events();
    expect(event?.breadcrumbs).toHaveLength(1);
    expect(event?.breadcrumbs?.[0]?.message).toBe('first');
    expect(event?.breadcrumbs?.[0]?.data).toEqual({ a: 1 });
  });

  it('breadcrumbs is omitted when the buffer is empty', () => {
    const p = setup();
    captureException(new Error('x'));
    expect(p.events()[0]?.breadcrumbs).toBeUndefined();
  });

  it('the 131 072-byte total (Go ruler): the oldest crumbs are dropped while over', () => {
    const p = setup({ maxBreadcrumbs: 100 });
    // Each crumb with an 8000-char message is comfortably over 8 KiB; 100 of them exceed 131 072.
    for (let i = 0; i < 100; i++) addBreadcrumb({ message: `${i}-${'x'.repeat(8000)}` });
    captureException(new Error('x'));
    const [event] = p.events();
    const crumbs = event?.breadcrumbs ?? [];
    expect(crumbs.length).toBeGreaterThan(0);
    expect(crumbs.length).toBeLessThan(100);
    // Oldest dropped: the kept crumbs are a suffix of 0..99, ending at 99.
    const lastIndex = Number(crumbs[crumbs.length - 1]?.message?.split('-')[0]);
    expect(lastIndex).toBe(99);
    const total = crumbs.reduce((sum, c) => sum + metadataWireBytes(JSON.stringify({ ...c, type: c.type ?? '', category: c.category ?? '', data: c.data ?? null })), 2 + (crumbs.length - 1));
    expect(total).toBeLessThanOrEqual(MAX_BREADCRUMBS_TOTAL_BYTES);
  });
});
