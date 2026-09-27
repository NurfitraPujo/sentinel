// `setExtra` / `setExtras` (spec §3.7, TASK-01) and the metadata shrink that never drops (spec review round 10,
// TASK-02 case (10) shrink parts: the coordinator's pin, the exempt keys' order, and the cap). Shuffle-safe:
// every test starts from a deleted `globalThis` slot.

import { beforeEach, describe, expect, it } from 'vitest';
import { captureException } from '../../src/core/capture.js';
import { MAX_METADATA_BYTES } from '../../src/core/event.js';
import { setExtra, setExtras } from '../../src/core/scope.js';
import { metadataWireBytes } from '../../src/core/serialize.js';
import { getSlot } from '../../src/core/state.js';
import { resetPipeline, setup } from '../helpers/pipeline.js';

beforeEach(resetPipeline);

describe('setExtra / setExtras (spec §3.7)', () => {
  it('setExtra(key, value) → metadata[key]; null and undefined delete it', () => {
    const p = setup();
    setExtra('route', '/x');
    captureException(new Error('a'));
    expect(p.events()[0]?.metadata.route).toBe('/x');

    setExtra('route', null);
    captureException(new Error('b'));
    expect(p.events()[1]?.metadata.route).toBeUndefined();

    setExtra('route', '/y');
    setExtra('route', undefined);
    captureException(new Error('c'));
    expect(p.events()[2]?.metadata.route).toBeUndefined();
  });

  it('setExtras(extras) is setExtra for every own key, in order', () => {
    const p = setup();
    setExtras({ a: 1, b: 2 });
    captureException(new Error('x'));
    expect(p.events()[0]?.metadata).toMatchObject({ a: 1, b: 2 });
  });

  it('a reserved key is refused, with a debug line and no onError: runtime, source, level, truncated, truncated_keys, user_id, tenant_id', () => {
    const p = setup();
    for (const key of ['runtime', 'source', 'level', 'truncated', 'truncated_keys', 'user_id', 'tenant_id']) {
      setExtra(key, 'x');
    }
    expect(getSlot().scope.extras.size).toBe(0);
    captureException(new Error('x'));
    expect(p.events()[0]?.metadata.source).toBeUndefined();
    expect(p.events()[0]?.metadata.user_id).toBeUndefined();
    expect(p.errors).toEqual([]);
    expect(p.debugLines.some((l) => l.includes("setExtra('runtime') dropped"))).toBe(true);
  });

  it('setExtras with a non-object argument is a no-op', () => {
    setExtras(null as unknown as Record<string, unknown>);
    setExtras(3 as unknown as Record<string, unknown>);
    expect(getSlot().scope.extras.size).toBe(0);
  });
});

describe('the metadata shrink never drops (spec §3.7, spec review round 10; TASK-02 case (10) shrink parts)', () => {
  it("the coordinator's pin: setExtra('route', 70 000 chars) alone is sent with route removed, truncated_keys: [route], runtime kept, no onError", () => {
    const p = setup();
    setExtra('route', 'x'.repeat(70_000));
    const id = captureException(new Error('x'));
    expect(id).toBeTruthy();
    const [event] = p.events();
    expect(event?.metadata.route).toBeUndefined();
    expect(event?.metadata.truncated_keys).toEqual(['route']);
    expect(event?.metadata.runtime).toBe('browser');
    expect(metadataWireBytes(JSON.stringify(event?.metadata))).toBeLessThanOrEqual(MAX_METADATA_BYTES);
    expect(p.errors).toEqual([]);
  });

  it('the exempt keys go last, in the server’s order: setExtra(request_id, 70 000 chars) is sent with request_id removed', () => {
    const p = setup();
    setExtra('request_id', 'x'.repeat(70_000));
    captureException(new Error('x'));
    const [event] = p.events();
    expect(event?.metadata.request_id).toBeUndefined();
    expect(event?.metadata.truncated_keys).toEqual(['request_id']);
  });

  it('setExtras({ request_id: 40 000, "request-id": 40 000, small: 1 }) removes small first, then request-id (lexically smaller at the same size), keeps request_id', () => {
    const p = setup();
    setExtras({ request_id: 'x'.repeat(40_000), 'request-id': 'x'.repeat(40_000), small: 1 });
    captureException(new Error('x'));
    const [event] = p.events();
    expect(event?.metadata.request_id).toBeDefined();
    expect(event?.metadata['request-id']).toBeUndefined();
    expect(event?.metadata.small).toBeUndefined();
    expect(event?.metadata.truncated_keys).toEqual(['small', 'request-id']);
  });

  it('the cap: setExtra(request_id, r1) plus 3 000 60-char-key extras through the browser runtime keeps 958 extras, request_id, and caps truncated_keys at 51 entries — the server’s own outcome on dde0ea2', () => {
    const keyFor = (i: number): string => 'k' + String(i).padStart(59, '0');
    const p = setup({}, 'browser');
    setExtra('request_id', 'r1');
    for (let i = 0; i < 3_000; i++) setExtra(keyFor(i), 0);
    captureException(new Error('cap'));
    const [event] = p.events();
    expect(event?.metadata.runtime).toBe('browser');
    expect(event?.metadata.request_id).toBe('r1');
    const keys = Object.keys(event?.metadata ?? {}).filter((k) => k.startsWith('k'));
    expect(keys).toHaveLength(958);
    expect(keys[0]).toBe(keyFor(2042));
    expect(keys.at(-1)).toBe(keyFor(2999));
    const truncatedKeys = event?.metadata.truncated_keys as string[];
    expect(truncatedKeys).toHaveLength(51);
    expect(truncatedKeys[0]).toBe(keyFor(0));
    expect(truncatedKeys[49]).toBe(keyFor(49));
    expect(truncatedKeys[50]).toBe('+1992 more');
    expect(metadataWireBytes(JSON.stringify(event?.metadata))).toBeLessThanOrEqual(MAX_METADATA_BYTES);
    expect(p.errors).toEqual([]);
  }, 20_000);
});
