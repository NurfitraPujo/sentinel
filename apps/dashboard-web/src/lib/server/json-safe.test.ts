import { describe, it, expect } from 'vitest';
import { dropProtoKeys, cleanOccurrenceRow } from './json-safe';

// sentinel-occurrence-detail-panels §3.4 (J1). SvelteKit serialises page data with devalue, which
// throws `Cannot stringify objects with __proto__ keys`; JSON.parse (the postgres driver's JSONB
// decoder) makes `__proto__` an ordinary own key. dropProtoKeys removes it at every depth.

function everyObject(value: unknown, visit: (o: object) => void) {
	if (Array.isArray(value)) {
		visit(value);
		value.forEach((v) => everyObject(v, visit));
	} else if (value !== null && typeof value === 'object') {
		visit(value);
		Object.values(value).forEach((v) => everyObject(v, visit));
	}
}

describe('dropProtoKeys (J1)', () => {
	it('drops __proto__ at every depth, arrays included, without assigning it', () => {
		const input = JSON.parse('{"__proto__":"t","a":{"__proto__":{"x":1},"b":[{"__proto__":1,"c":2}]}}');
		const out = dropProtoKeys(input);
		expect(out).toEqual({ a: { b: [{ c: 2 }] } });
		everyObject(out, (o) => {
			if (!Array.isArray(o)) {
				expect(Object.getPrototypeOf(o)).toBe(Object.prototype);
				expect(Object.hasOwn(o, '__proto__')).toBe(false);
			}
		});
	});

	it('returns a deep-equal copy of a value with no __proto__ key', () => {
		const input = { a: 1, b: [1, 'x', null, { c: true, d: { e: [] } }], f: null, g: 'str' };
		expect(dropProtoKeys(input)).toEqual(input);
		expect(dropProtoKeys([1, { a: 2 }])).toEqual([1, { a: 2 }]);
	});

	it('returns scalars and a Date as they are', () => {
		const d = new Date('2026-09-25T02:00:00.000Z');
		expect(dropProtoKeys(d)).toBe(d);
		expect(dropProtoKeys('s')).toBe('s');
		expect(dropProtoKeys(7)).toBe(7);
		expect(dropProtoKeys(false)).toBe(false);
		expect(dropProtoKeys(null)).toBe(null);
		expect(dropProtoKeys(undefined)).toBe(undefined);
	});

	it('keeps a null-prototype object plain', () => {
		const o = Object.create(null);
		o.a = 1;
		Object.defineProperty(o, '__proto__', { value: 2, enumerable: true });
		expect(dropProtoKeys(o)).toEqual({ a: 1 });
	});
});

describe('cleanOccurrenceRow', () => {
	it('cleans only the JSONB keys present, and copies every other key unchanged', () => {
		const createdAt = new Date('2026-09-25T02:00:00.000Z');
		const row = {
			id: 'o',
			createdAt,
			message: '__proto__',
			metadata: JSON.parse('{"__proto__":1,"k":"v"}'),
			tags: JSON.parse('{"__proto__":"x","env":"prod"}'),
			user: null,
		};
		const out = cleanOccurrenceRow(row);
		expect(out.createdAt).toBe(createdAt);
		expect(out.message).toBe('__proto__');
		expect(out.metadata).toEqual({ k: 'v' });
		expect(out.tags).toEqual({ env: 'prod' });
		expect(out.user).toBeNull();
		expect(Object.hasOwn(out, 'contexts')).toBe(false);
		expect(Object.hasOwn(out.metadata as object, '__proto__')).toBe(false);
	});
});
