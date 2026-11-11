import { describe, it, expect } from 'vitest';
import { tagCondition, TAG_SEARCH_HINTS } from './search';
import { parseTagParam, TAG_KEY_PATTERN } from '$lib/tag-filter';

// sentinel-issue-tag-facets spec §6.1: a tag key is ignored or applied, never an error. TAG_SEARCH_HINTS is
// a plain object literal, so a key that names an inherited Object.prototype member (constructor, toString,
// valueOf, hasOwnProperty, __proto__) resolves TAG_SEARCH_HINTS[key] to that inherited member instead of
// undefined, and `hint.predicate(value)` then throws — a 500 for /search?tag=constructor:x, the search box,
// or a stored custom tag's panel link (all three converge on tagCondition through parseTagParam).
const POISON_KEYS = ['constructor', 'toString', 'valueOf', 'hasOwnProperty', '__proto__'];

describe('tag key safety against Object.prototype pollution (§6.1)', () => {
	it('every poison key still passes TAG_KEY_PATTERN, so tagCondition must handle it, not reject it upstream', () => {
		for (const key of POISON_KEYS) expect(TAG_KEY_PATTERN.test(key), key).toBe(true);
	});

	it('parseTagParam accepts a poison key as an ordinary custom tag (no special-casing upstream)', () => {
		for (const key of POISON_KEYS) {
			const parsed = parseTagParam(`${key}:x`);
			expect(parsed, key).toEqual({ key, value: 'x' });
		}
	});

	it('TAG_SEARCH_HINTS has no own entry for any poison key (they are all custom, un-hinted tags)', () => {
		for (const key of POISON_KEYS) expect(Object.hasOwn(TAG_SEARCH_HINTS, key), key).toBe(false);
	});

	it('tagCondition never throws for a poison key, whether or not it names a real hint', () => {
		for (const key of POISON_KEYS) {
			expect(() => tagCondition({ key, value: 'x' }), key).not.toThrow();
		}
	});
});
