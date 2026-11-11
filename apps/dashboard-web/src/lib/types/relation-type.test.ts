import { describe, it, expect, vi, afterEach } from 'vitest';
import { RELATION_TYPES, isRelationType, filterKnownRelationTypes } from './relation-type';

describe('isRelationType', () => {
	it('accepts every value the DB CHECK constraint permits', () => {
		for (const value of RELATION_TYPES) {
			expect(isRelationType(value)).toBe(true);
		}
	});

	it('rejects values outside the known set', () => {
		expect(isRelationType('parent_of')).toBe(false);
		expect(isRelationType('child_of')).toBe(false);
		expect(isRelationType('')).toBe(false);
		expect(isRelationType(null)).toBe(false);
		expect(isRelationType(undefined)).toBe(false);
		expect(isRelationType(42)).toBe(false);
	});
});

describe('filterKnownRelationTypes', () => {
	afterEach(() => {
		vi.restoreAllMocks();
	});

	it('drops an item whose relationType is not a known RelationType rather than rendering it', () => {
		const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});

		const items = [
			{ id: 'rel-1', relationType: 'linked_to' },
			{ id: 'rel-2', relationType: 'parent_of' }, // not permitted by the DB CHECK constraint
			{ id: 'rel-3', relationType: 'caused_by' },
		];

		const result = filterKnownRelationTypes(items);

		expect(result).toEqual([
			{ id: 'rel-1', relationType: 'linked_to' },
			{ id: 'rel-3', relationType: 'caused_by' },
		]);
		expect(result.some((r) => r.id === 'rel-2')).toBe(false);
		expect(warnSpy).toHaveBeenCalledTimes(1);
		expect(warnSpy.mock.calls[0][0]).toContain('rel-2');
		expect(warnSpy.mock.calls[0][0]).toContain('parent_of');
	});

	it('keeps every item when all relationTypes are known', () => {
		const items = [
			{ id: 'rel-1', relationType: 'linked_to' },
			{ id: 'rel-2', relationType: 'duplicate_of' },
		];

		expect(filterKnownRelationTypes(items)).toEqual(items);
	});

	it('returns an empty array, not a throw, when every item is unrecognized', () => {
		vi.spyOn(console, 'warn').mockImplementation(() => {});
		const items = [{ id: 'rel-1', relationType: 'bogus' }];

		expect(filterKnownRelationTypes(items)).toEqual([]);
	});
});
