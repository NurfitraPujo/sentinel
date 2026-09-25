import { describe, it, expect } from 'vitest';
import {
	formatTagParam,
	tagSearchHref,
	parseTagParam,
	parseSearchText,
	formatSearchToken,
	TAG_KEY_PATTERN,
	MAX_TAG_VALUE_BYTES,
	MAX_TAG_FILTERS,
} from './tag-filter';

// sentinel-issue-tag-facets spec §6.1-§6.2 (TASK-03; contract decisions 8 and 10): the one tag-link
// encoder shared with sentinel-occurrence-detail-panels, the `tag` parameter parser (P1-P6), and the
// free-text box's `key:value` grammar.

// Parse one href the way the loader sees it: after the normal query decoding.
const paramOf = (href: string) => new URL(href, 'http://x').searchParams.getAll('tag')[0];

describe('the shared href table (spec §6.1; sentinel-occurrence-detail-panels pins the same rows)', () => {
	const TABLE: [string, string, string, string][] = [
		['device.model', 'SM-A115F', '/search?tag=device.model%3ASM-A115F', 'device.model:SM-A115F'],
		['a:b', 'c:d e/%', '/search?tag=a%253Ab%3Ac%253Ad%2520e%252F%2525', 'a%3Ab:c%3Ad%20e%2F%25'],
		['k', "it's (x)!~", "/search?tag=k%3Ait's%2520(x)!~", "k:it's%20(x)!~"],
	];
	for (const [key, value, href, param] of TABLE) {
		it(`${key} = ${value}`, () => {
			expect(tagSearchHref(key, value)).toBe(href);
			expect(formatTagParam(key, value)).toBe(param);
			expect(paramOf(href)).toBe(param);
			expect(parseTagParam(paramOf(href))).toEqual({ key, value });
		});
	}

	it("the URLSearchParams spelling of the ! ' ( ) ~ row parses to the same pair (decision 10)", () => {
		const usp = '/search?tag=k%3Ait%27s%2520%28x%29%21%7E';
		expect(`/search?${new URLSearchParams({ tag: formatTagParam('k', "it's (x)!~") })}`).toBe(usp);
		expect(usp).not.toBe(tagSearchHref('k', "it's (x)!~"));
		expect(parseTagParam(paramOf(usp))).toEqual({ key: 'k', value: "it's (x)!~" });
	});
});

describe('parseTagParam: ignored, never an error (P1-P6)', () => {
	it.each([
		['P1 no literal colon', 'abc'],
		['P2 more than one literal colon', 'a:b:c'],
		['P3 an empty key', ':v'],
		['P3 an empty value', 'k:'],
		['P4 a decode that throws', '%E0%A4%A:v'],
		['P4 a decode that throws on the value', 'k:%E0%A4%A'],
		['P5 a key outside the pattern', 'a%20b:v'],
		['P5 a 33-character key', `${'k'.repeat(33)}:v`],
		['P6 a value over 1024 UTF-8 bytes', `k:${encodeURIComponent('é'.repeat(513))}`],
	])('%s', (_name, raw) => {
		expect(parseTagParam(raw)).toBeNull();
	});

	it('accepts a value of exactly 1024 bytes, untrimmed and unnormalised', () => {
		expect(parseTagParam(`k:${encodeURIComponent('é'.repeat(512))}`)).toEqual({ key: 'k', value: 'é'.repeat(512) });
		expect(parseTagParam(`k:${encodeURIComponent('  x ')}`)).toEqual({ key: 'k', value: '  x ' });
	});

	it('exports the bounds the rollup has', () => {
		expect(TAG_KEY_PATTERN.test('client_os.name')).toBe(true);
		expect(TAG_KEY_PATTERN.test('a:b')).toBe(true);
		expect(TAG_KEY_PATTERN.test('a b')).toBe(false);
		expect(MAX_TAG_VALUE_BYTES).toBe(1024);
		expect(MAX_TAG_FILTERS).toBe(10);
	});
});

describe('parseSearchText: the free-text grammar (spec §6.2)', () => {
	it.each([
		['device.model:SM-A115F', [{ key: 'device.model', value: 'SM-A115F' }], '', []],
		['os.name:"Mac OS X"', [{ key: 'os.name', value: 'Mac OS X' }], '', []],
		['"a:b":"c:d e/%"', [{ key: 'a:b', value: 'c:d e/%' }], '', []],
		['url:http://x/y', [{ key: 'url', value: 'http://x/y' }], '', []],
		['k:"say \\"hi\\" \\\\ \\n"', [{ key: 'k', value: 'say "hi" \\ \\n' }], '', []],
		['timeout release:1.2 conn', [{ key: 'release', value: '1.2' }], 'timeout  conn', []],
		['a:1 b:2', [{ key: 'a', value: '1' }, { key: 'b', value: '2' }], ' ', []],
		[':x', [], '', [':x']],
		['k:', [], '', ['k:']],
		[`${'k'.repeat(33)}:v`, [], '', [`${'k'.repeat(33)}:v`]],
		['k:"unterminated', [], '', ['k:"unterminated']],
		['"a:b":x', [{ key: 'a:b', value: 'x' }], '', []],
		['"a b":x', [], '', ['"a b":x']],
	])('%s', (text, filters, remainder, ignored) => {
		const parsed = parseSearchText(text);
		expect(parsed.filters).toEqual(filters);
		expect(parsed.remainder).toBe(remainder);
		expect(parsed.ignored).toEqual(ignored);
	});

	it('a box with no tag token and no dropped token goes to full-text search unchanged', () => {
		for (const text of ['timeout *conn* fail*', '  spaced   out  ', 'plain', '"quoted words"']) {
			expect(parseSearchText(text)).toEqual({ filters: [], remainder: text, ignored: [] });
		}
	});
});

describe('formatSearchToken writes the canonical form, and parseSearchText reads it back', () => {
	it('examples', () => {
		expect(formatSearchToken('device.model', 'SM-A115F')).toBe('device.model:SM-A115F');
		expect(formatSearchToken('os.name', 'Mac OS X')).toBe('os.name:"Mac OS X"');
		expect(formatSearchToken('a:b', 'c:d e/%')).toBe('"a:b":"c:d e/%"');
		expect(formatSearchToken('k', '"q')).toBe('k:"\\"q"');
		expect(formatSearchToken('k', 'a\\b')).toBe('k:"a\\\\b"');
	});

	it('round trip: "a:b" / "c:d e/%" gives exactly that filter and no remainder', () => {
		expect(parseSearchText(formatSearchToken('a:b', 'c:d e/%'))).toEqual({
			filters: [{ key: 'a:b', value: 'c:d e/%' }],
			remainder: '',
			ignored: [],
		});
	});

	it('property: 1,000 generated (key, value) pairs round-trip', () => {
		let seed = 42;
		const rand = () => {
			seed = (seed * 1103515245 + 12345) % 2 ** 31;
			return seed / 2 ** 31;
		};
		const keyAlphabet = 'abcXYZ019_.-:';
		const valueAlphabet = ['a', 'Z', '9', ' ', '"', '\\', ':', '/', '%', 'é', '日', '😀', '\t', "'", '(', ')', '!', '~', '-'];
		for (let n = 0; n < 1000; n++) {
			let key = '';
			const kl = 1 + Math.floor(rand() * 32);
			for (let i = 0; i < kl; i++) key += keyAlphabet[Math.floor(rand() * keyAlphabet.length)];
			let value = '';
			const vl = 1 + Math.floor(rand() * 20);
			for (let i = 0; i < vl; i++) value += valueAlphabet[Math.floor(rand() * valueAlphabet.length)];
			const token = formatSearchToken(key, value);
			const parsed = parseSearchText(token);
			expect(parsed.filters, token).toEqual([{ key, value }]);
			expect(parsed.remainder.trim(), token).toBe('');
			expect(parsed.ignored, token).toEqual([]);
			// and through a tag link, too
			expect(parseTagParam(paramOf(tagSearchHref(key, value))), key).toEqual({ key, value });
		}
	});
});
