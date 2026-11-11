import { describe, it, expect } from 'vitest';
import { buildTagFacets, largestRemainderTenths, type TagFacetRow } from './tag-facets';

// sentinel-issue-tag-facets spec §7.3 (TASK-02): the panel's blocks from getTagFacets' rows. F1 order,
// F2 top 5, F3 "Other", F4 largest-remainder percentages summing to exactly 100.0, F6 nothing when empty.

const row = (key: string, value: string, count: number, keyTotal: number, derivedOrd: number | null = null): TagFacetRow => ({
	key,
	value,
	count,
	keyTotal,
	derivedOrd,
});

describe('buildTagFacets', () => {
	it('F6: no rows, no blocks', () => {
		expect(buildTagFacets([])).toEqual([]);
	});

	it('F1: derived keys in derived order, then custom keys in byte order', () => {
		const rows = [
			row('zeta', 'v', 1, 1),
			row('user.id', 'u', 1, 1, 7),
			row('Alpha', 'v', 1, 1), // byte order: uppercase before lowercase
			row('release', '1.0', 1, 1, 1),
			row('environment', 'production', 1, 1, 2),
			row('alpha', 'v', 1, 1),
			row('browser.name', 'Chrome', 1, 1, 6),
		];
		expect(buildTagFacets(rows).map((b) => b.key)).toEqual(['release', 'environment', 'browser.name', 'user.id', 'Alpha', 'alpha', 'zeta']);
	});

	it('F2: up to five named values, by count then value in byte order; the overflow row is never named', () => {
		const rows = [
			row('k', 'b', 5, 30),
			row('k', 'a', 5, 30),
			row('k', 'c', 9, 30),
			row('k', 'd', 1, 30),
			row('k', 'e', 3, 30),
			row('k', 'f', 2, 30),
			row('k', '', 5, 30),
		];
		const [block] = buildTagFacets(rows);
		expect(block.values.map((v) => v.value)).toEqual(['c', 'a', 'b', 'e', 'f']);
	});

	it('F2: the tie-break is byte order, not locale order — they disagree on mixed case and non-ASCII', () => {
		// Byte order (ASCII): 'B' (0x42) < 'a' (0x61), so 'B' sorts first. localeCompare disagrees:
		// 'a'.localeCompare('B') === -1 (locale-aware collation treats case as a secondary key), which
		// would put 'a' first instead — the exact substitution the finding names.
		expect('a'.localeCompare('B')).toBeLessThan(0); // documents the disagreement this test depends on
		const mixedCase = buildTagFacets([row('k', 'a', 1, 2), row('k', 'B', 1, 2)])[0];
		expect(mixedCase.values.map((v) => v.value)).toEqual(['B', 'a']);

		// Byte order (UTF-8): 'e' (0x65) < 'é' (0xC3 0xA9), so 'e' sorts first — localeCompare's
		// language-aware collation instead treats 'é' as a variant of 'e' sorting adjacently, often
		// after plain 'e' but never by raw byte value, so this pins the byte comparison specifically.
		const nonAscii = buildTagFacets([row('k', 'é', 1, 2), row('k', 'e', 1, 2)])[0];
		expect(nonAscii.values.map((v) => v.value)).toEqual(['e', 'é']);

		// Byte order (UTF-8) vs UTF-16 code-unit order also disagree, distinctly from locale order (tfr2-
		// f2-utf16-compare-survives): U+FF5E ('～', a single UTF-16 code unit 0xFF5E) encodes to UTF-8
		// bytes EF BD 9E; U+1F600 ('😀', a UTF-16 surrogate pair starting 0xD83D) encodes to F0 9F 98 80.
		// Byte order: 0xEF < 0xF0, so '～' sorts first. A plain UTF-16 code-unit compare (`a < b`) instead
		// compares the leading units 0xFF5E vs 0xD83D and puts '😀' first — the exact substitution this
		// case exists to kill; a.value.localeCompare(b.value) failed to (SQL already picks the top 5 with
		// COLLATE "C", so only display order among ties at the boundary is at stake).
		const supplementary = buildTagFacets([row('k', '\u{1F600}', 1, 2), row('k', '～', 1, 2)])[0];
		expect(supplementary.values.map((v) => v.value)).toEqual(['～', '\u{1F600}']);
	});

	it('F3: Other = total − the shown values, with and without an overflow row; not shown at 0', () => {
		// Named values past the top 5 are not in the rows at all; the total still counts them.
		const past = buildTagFacets([row('k', 'a', 5, 20), row('k', 'b', 3, 20)])[0];
		expect(past.other).toEqual({ count: 12, percent: expect.any(Number) });
		const withOverflow = buildTagFacets([row('k', 'a', 5, 12), row('k', '', 7, 12)])[0];
		expect(withOverflow.other?.count).toBe(7);
		const none = buildTagFacets([row('k', 'a', 5, 8), row('k', 'b', 3, 8)])[0];
		expect(none.other).toBeNull();
	});

	it('F3: a key whose only row is the overflow row shows one "Other" line at 100%', () => {
		const [block] = buildTagFacets([row('user.id', '', 42, 42, 7)]);
		expect(block.values).toEqual([]);
		expect(block.other).toEqual({ count: 42, percent: 100 });
	});

	it('F4: [1, 1, 1] -> 33.4 / 33.3 / 33.3', () => {
		const [block] = buildTagFacets([row('k', 'a', 1, 3), row('k', 'b', 1, 3), row('k', 'c', 1, 3)]);
		expect(block.values.map((v) => v.percent)).toEqual([33.4, 33.3, 33.3]);
	});

	it('F4: percentages over the shown lines, Other included, sum to exactly 100.0 for 500 random fixtures', () => {
		let seed = 12345;
		const rand = () => {
			seed = (seed * 1103515245 + 12345) % 2 ** 31;
			return seed / 2 ** 31;
		};
		for (let n = 0; n < 500; n++) {
			const values = Math.floor(rand() * 8) + 1;
			const counts = Array.from({ length: values }, () => Math.floor(rand() * 1000) + 1);
			const overflow = rand() < 0.3 ? Math.floor(rand() * 500) + 1 : 0;
			const total = counts.reduce((a, b) => a + b, 0) + overflow;
			// The query returns the top 5 named rows plus the overflow row; the total is every row's.
			const sorted = counts
				.map((c, i) => ({ c, v: `v${i}` }))
				.sort((a, b) => b.c - a.c || (a.v < b.v ? -1 : 1))
				.slice(0, 5);
			const rows = sorted.map((s) => row('k', s.v, s.c, total));
			if (overflow) rows.push(row('k', '', overflow, total));
			const [block] = buildTagFacets(rows);
			const tenths = [...block.values.map((v) => v.percent), ...(block.other ? [block.other.percent] : [])].map((p) =>
				Math.round(p * 10)
			);
			expect(tenths.reduce((a, b) => a + b, 0)).toBe(1000);
			for (const t of tenths) expect(t).toBeGreaterThanOrEqual(0);
		}
	});
});

describe('largestRemainderTenths', () => {
	it('floors each share and gives the remaining tenths to the largest fractions, ties to the earlier line', () => {
		expect(largestRemainderTenths([1, 1, 1], 3)).toEqual([334, 333, 333]);
		expect(largestRemainderTenths([1, 2], 3)).toEqual([333, 667]);
		expect(largestRemainderTenths([5], 5)).toEqual([1000]);
		expect(largestRemainderTenths([], 0)).toEqual([]);
	});
});
