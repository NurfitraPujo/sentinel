import { describe, it, expect } from 'vitest';
import {
	deriveReportTitle,
	normalizeReportText,
	truncateAtWordBoundary,
	TITLE_FALLBACK,
	TITLE_MAX_CODE_POINTS,
	TITLE_TRUNCATE_CODE_POINTS,
} from './report-title';

/**
 * sentinel-user-report-issue §4.5a, and §11's boundary for it: "The title derivation function
 * (§4.5a) | new -- a pure function … asserting them through a created issue's `message` is
 * indirection for nothing."
 *
 * Table-driven, no database, no clock, no HTTP. The endpoint's own integration test asserts only
 * that `issues.message` equals what this same function returns for the same input, which is the
 * "identical" half of TASK-03's criterion 3 -- not a second copy of these cases.
 */

/** Code points, not UTF-16 units -- the unit §4.5a counts in. */
function codePoints(value: string): number {
	return Array.from(value).length;
}

describe('§4.5a normalizeReportText', () => {
	it('trims and collapses every internal whitespace run to a single space', () => {
		expect(normalizeReportText('  the   save\tbutton\n\nspins  ')).toBe('the save button spins');
	});

	it('collapses non-ASCII whitespace too (NBSP, ideographic space)', () => {
		expect(normalizeReportText('save button　spins')).toBe('save button spins');
	});

	it('yields the empty string for a whitespace-only report', () => {
		expect(normalizeReportText(' \t\n ')).toBe('');
	});
});

describe('§4.5a truncateAtWordBoundary', () => {
	it('leaves text at or under the limit untouched, and adds no ellipsis', () => {
		const exactly80 = 'a'.repeat(80);
		expect(codePoints(exactly80)).toBe(TITLE_TRUNCATE_CODE_POINTS);
		expect(truncateAtWordBoundary(exactly80)).toBe(exactly80);
		expect(truncateAtWordBoundary(exactly80)).not.toContain('…');
	});

	it('cuts at the last whitespace boundary at or before 80, so a word is never split', () => {
		// 'word ' x 20 = 100 chars; the 80th code point falls inside the 16th 'word'.
		const text = Array(20).fill('word').join(' ');
		const out = truncateAtWordBoundary(text);
		expect(out.endsWith('…')).toBe(true);
		// Every retained token is a whole 'word' -- nothing is half a word.
		for (const token of out.slice(0, -1).split(' ')) {
			expect(token).toBe('word');
		}
		expect(codePoints(out.slice(0, -1))).toBeLessThanOrEqual(TITLE_TRUNCATE_CODE_POINTS);
	});

	it('appends the single-code-point U+2026, never three ASCII dots', () => {
		const out = truncateAtWordBoundary(`${'a'.repeat(40)} ${'b'.repeat(60)}`);
		expect(out.endsWith('…')).toBe(true);
		expect(out.endsWith('...')).toBe(false);
	});

	it('hard-cuts a single word longer than the limit, since it offers no boundary to cut at', () => {
		const out = truncateAtWordBoundary('x'.repeat(200));
		expect(out).toBe(`${'x'.repeat(TITLE_TRUNCATE_CODE_POINTS)}…`);
	});

	it('counts CODE POINTS, not bytes: 80 three-byte characters are not truncated', () => {
		// 80 CJK ideographs = 240 UTF-8 bytes. A byte-counting implementation truncates this.
		const cjk = '保'.repeat(80);
		expect(new TextEncoder().encode(cjk).byteLength).toBe(240);
		expect(truncateAtWordBoundary(cjk)).toBe(cjk);
	});

	it('counts CODE POINTS, not UTF-16 units: 80 astral characters are not truncated', () => {
		// Each of these is one code point and TWO UTF-16 units, so `String.length` sees 160.
		const astral = '𝔘'.repeat(80);
		expect(astral.length).toBe(160);
		expect(codePoints(astral)).toBe(80);
		expect(truncateAtWordBoundary(astral)).toBe(astral);
	});

	it('never splits a grapheme cluster: a ZWJ emoji family is kept whole or dropped whole', () => {
		// 👩‍👩‍👧‍👦 is one grapheme cluster of 7 code points. Ten of them is 70 code points; an
		// eleventh would cross 80, so the cut must land on a cluster boundary and never mid-family.
		const family = '👩‍👩‍👧‍👦';
		expect(codePoints(family)).toBe(7);
		const out = truncateAtWordBoundary(family.repeat(20));
		const body = out.slice(0, -1);
		expect(codePoints(body) % 7).toBe(0);
		// A split cluster leaves a bare ZWJ or a lone person emoji at the end; neither may appear.
		expect(body.endsWith('‍')).toBe(false);
		expect(body).toBe(family.repeat(codePoints(body) / 7));
	});

	it('never splits a combining sequence', () => {
		// 'e' + U+0301 COMBINING ACUTE = one cluster, two code points.
		const cluster = 'é';
		const out = truncateAtWordBoundary(cluster.repeat(60));
		const body = out.slice(0, -1);
		expect(codePoints(body) % 2).toBe(0);
		expect(body.endsWith('́')).toBe(true);
	});
});

describe('§4.5a deriveReportTitle', () => {
	it('returns the trimmed, collapsed text when it is short and there is no page', () => {
		expect(deriveReportTitle({ text: '  The save  button spins forever. ' })).toBe(
			'The save button spins forever.'
		);
	});

	it('prefixes "<page> — " when context.page is a non-empty string', () => {
		expect(deriveReportTitle({ text: 'The save button spins forever.', page: 'Visits' })).toBe(
			'Visits — The save button spins forever.'
		);
	});

	it.each([
		['absent', undefined],
		['null', null],
		['empty', ''],
		['whitespace only', '   '],
	])('adds no prefix when context.page is %s', (_label, page) => {
		expect(deriveReportTitle({ text: 'spins forever', page })).toBe('spins forever');
	});

	it('applies the prefix to the ALREADY truncated text, not to the whole report', () => {
		const text = Array(30).fill('word').join(' ');
		const expected = `Visits — ${truncateAtWordBoundary(normalizeReportText(text))}`;
		expect(deriveReportTitle({ text, page: 'Visits' })).toBe(expected);
	});

	it('clamps the whole composed title to 200 code points', () => {
		// A page long enough that page + separator + 81 code points of text exceeds 200.
		const page = 'P'.repeat(180);
		const out = deriveReportTitle({ text: Array(30).fill('word').join(' '), page });
		expect(codePoints(out)).toBe(TITLE_MAX_CODE_POINTS);
		expect(out.startsWith(page)).toBe(true);
	});

	it('clamps on a grapheme boundary, so the 200-code-point cut cannot split a cluster', () => {
		const out = deriveReportTitle({
			text: Array(30).fill('word').join(' '),
			page: 'é'.repeat(120),
		});
		expect(codePoints(out)).toBeLessThanOrEqual(TITLE_MAX_CODE_POINTS);
		expect(out.endsWith('́')).toBe(true);
	});

	it.each([
		['empty', ''],
		['whitespace only', '   \t\n '],
	])('falls back to "User report" when text is %s', (_label, text) => {
		expect(deriveReportTitle({ text })).toBe(TITLE_FALLBACK);
	});

	it('falls back to "User report" ALONE when text is empty, even with a page — §4.5a step 5 is about `text`', () => {
		expect(deriveReportTitle({ text: '', page: 'Visits' })).toBe(TITLE_FALLBACK);
	});

	it('is total: every input in the table yields a non-empty title, because issues.message is NOT NULL', () => {
		const inputs = [
			{ text: '' },
			{ text: ' ' },
			{ text: 'x' },
			{ text: 'x'.repeat(8000) },
			{ text: 'x'.repeat(8000), page: 'P'.repeat(400) },
			{ text: '👩‍👩‍👧‍👦'.repeat(50), page: '👍' },
			{ text: '‍‍‍' },
		];
		for (const input of inputs) {
			const title = deriveReportTitle(input);
			expect(title.length).toBeGreaterThan(0);
			expect(codePoints(title)).toBeLessThanOrEqual(TITLE_MAX_CODE_POINTS);
		}
	});

	it('is pure: the same input yields the same output, and the input object is not mutated', () => {
		const input = { text: '  spins   forever  ', page: ' Visits ' };
		const snapshot = { ...input };
		expect(deriveReportTitle(input)).toBe(deriveReportTitle(input));
		expect(input).toEqual(snapshot);
	});
});
