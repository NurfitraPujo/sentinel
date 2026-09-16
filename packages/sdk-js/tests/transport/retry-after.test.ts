// `Retry-After` (contract §4.4 *Retry*; B16 unit): the seconds and HTTP-date forms honoured and capped at 60 s;
// an unparseable, ≤ 0 or past-dated value ignored — `undefined`, so the caller's ladder applies.

import { describe, expect, it } from 'vitest';
import { MAX_RETRY_AFTER_MS, parseRetryAfter } from '../../src/core/transport/retry-after.js';

const NOW = Date.UTC(2026, 8, 16, 12, 0, 0);

describe('B16 (unit): Retry-After parsing', () => {
  it('B16: "60" → 60 000 ms (the ingestor\'s value); "5" → 5 000; " 7 " trimmed', () => {
    expect(parseRetryAfter('60', NOW)).toBe(60_000);
    expect(parseRetryAfter('5', NOW)).toBe(5_000);
    expect(parseRetryAfter(' 7 ', NOW)).toBe(7_000);
  });

  it('B16: an HTTP-date 30 s out → ≈ 30 000 ms; a date an hour out and "120" are capped at 60 s', () => {
    expect(parseRetryAfter(new Date(NOW + 30_000).toUTCString(), NOW)).toBe(30_000);
    expect(parseRetryAfter(new Date(NOW + 3_600_000).toUTCString(), NOW)).toBe(MAX_RETRY_AFTER_MS);
    expect(parseRetryAfter('120', NOW)).toBe(MAX_RETRY_AFTER_MS);
    expect(MAX_RETRY_AFTER_MS).toBe(60_000);
  });

  it('B16: "soon", "", "0", "-5", a date an hour in the past, and an absent header are ignored (undefined)', () => {
    expect(parseRetryAfter('soon', NOW)).toBeUndefined();
    expect(parseRetryAfter('', NOW)).toBeUndefined();
    expect(parseRetryAfter('0', NOW)).toBeUndefined();
    expect(parseRetryAfter('-5', NOW)).toBeUndefined();
    expect(parseRetryAfter(new Date(NOW - 3_600_000).toUTCString(), NOW)).toBeUndefined();
    expect(parseRetryAfter(new Date(NOW).toUTCString(), NOW)).toBeUndefined();
    expect(parseRetryAfter(null, NOW)).toBeUndefined();
    expect(parseRetryAfter(undefined, NOW)).toBeUndefined();
  });
});
