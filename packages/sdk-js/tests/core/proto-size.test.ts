// The estimator's exact-value pins (spec §11.2 TASK-02 "The NATS publish bound"): each equals Go's `proto.Size`
// of `structpb.NewValue` / `NewStruct` of the same value, measured before this revision.

import { describe, expect, it } from 'vitest';
import { fld, structBytes, utf8Bytes, val, vl } from '../../src/core/proto-size.js';

describe('proto-size: val (spec §4.6)', () => {
  it('null, false → 2; 0 → 9; "" → 2; "é" → 4; [0] → 13', () => {
    expect(val(null)).toBe(2);
    expect(val(false)).toBe(2);
    expect(val(0)).toBe(9);
    expect(val('')).toBe(2);
    expect(val('é')).toBe(4);
    expect(val([0])).toBe(13);
  });
});

describe('proto-size: structBytes (spec §4.6)', () => {
  it('{ a: 0 } → 16; { a: { b: [null, true, "x"] } } → 31', () => {
    expect(structBytes({ a: 0 })).toBe(16);
    expect(structBytes({ a: { b: [null, true, 'x'] } })).toBe(31);
  });

  it('{ a: Array(2_030).fill(0) } → 22 345; { a: Array(32_700).fill(0) } → 359 715', () => {
    expect(structBytes({ a: Array.from({ length: 2_030 }, () => 0) })).toBe(22_345);
    expect(structBytes({ a: Array.from({ length: 32_700 }, () => 0) })).toBe(359_715);
  });
});

describe('proto-size: vl / fld / utf8Bytes, the primitives val and structBytes are built from', () => {
  it('vl: the varint length steps at 128, 16 384 and 2 097 152', () => {
    expect(vl(0)).toBe(1);
    expect(vl(127)).toBe(1);
    expect(vl(128)).toBe(2);
    expect(vl(16_383)).toBe(2);
    expect(vl(16_384)).toBe(3);
    expect(vl(2_097_151)).toBe(3);
    expect(vl(2_097_152)).toBe(4);
  });

  it('fld: 1-byte tag + the length varint + the content', () => {
    expect(fld(0)).toBe(2);
    expect(fld(127)).toBe(129);
  });

  it('utf8Bytes: multi-byte characters and a lone surrogate (3 bytes, as Go’s replacement does)', () => {
    expect(utf8Bytes('a')).toBe(1);
    expect(utf8Bytes('é')).toBe(2);
    expect(utf8Bytes('\uD800')).toBe(3);
  });
});
