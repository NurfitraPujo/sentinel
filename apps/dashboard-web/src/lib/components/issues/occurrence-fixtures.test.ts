import { describe, it, expect } from 'vitest';
import {
	FLUTTER_OCCURRENCE,
	FLUTTER_DETAIL,
	PRE_V2_KEYS,
	LIGHT_KEYS,
	HEAVY_KEYS,
} from './occurrence-fixtures';

// M1 (sentinel-occurrence-detail-panels §3.3, §12.3): the implemented fixture still matches the
// committed measurement. The numbers are measure-payload.mjs's JSON.stringify UTF-8 byte counts for
// payload-fixture.mjs; the fixture here is its verbatim copy, so a drift of more than 10 % means the
// two copies (or the §3.3 table) no longer agree.

function bytes(row: Record<string, unknown>, keys: readonly string[]): number {
	return Buffer.byteLength(JSON.stringify(Object.fromEntries(keys.map((k) => [k, row[k]]))), 'utf8');
}

function within10Percent(actual: number, expected: number) {
	expect(actual, `${actual} bytes vs the §3.3 figure ${expected}`).toBeGreaterThanOrEqual(expected * 0.9);
	expect(actual, `${actual} bytes vs the §3.3 figure ${expected}`).toBeLessThanOrEqual(expected * 1.1);
}

describe('occurrence fixtures match §3.3 (M1)', () => {
	it('one listed row today (the 12 pre-v2 keys) is about 4 574 bytes', () => {
		within10Percent(bytes(FLUTTER_OCCURRENCE, PRE_V2_KEYS), 4574);
	});

	it('the six light columns are about 212 bytes', () => {
		within10Percent(bytes(FLUTTER_OCCURRENCE, LIGHT_KEYS), 212);
	});

	it('the six heavy columns are about 27 504 bytes', () => {
		within10Percent(bytes(FLUTTER_DETAIL, HEAVY_KEYS), 27504);
	});
});
