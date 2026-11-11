// sentinel-occurrence-detail-panels §3.4: no `__proto__` key reaches page data.
//
// SvelteKit serialises load data with devalue, whose stringify/uneval throw `Cannot stringify objects
// with __proto__ keys`. The postgres driver decodes JSONB with JSON.parse, which makes `__proto__` an
// ordinary own key, and nothing at ingest forbids it (tag keys match `^[a-zA-Z0-9_.:-]{1,32}$`, and
// context, header, breadcrumb-data and metadata keys are only length-checked). So one stored event with
// such a key would make the whole issue page a 500. The load passes every JSONB value it returns
// through `dropProtoKeys`.

function isPlainObject(value: unknown): value is Record<string, unknown> {
	if (value === null || typeof value !== 'object') return false;
	const proto = Object.getPrototypeOf(value);
	return proto === Object.prototype || proto === null;
}

/**
 * A copy of `value` with every `__proto__` own key removed, at every depth. Arrays and plain objects
 * are copied; anything else (strings, numbers, booleans, null, a Date) is returned as is. A value with
 * no `__proto__` key comes back deep-equal. The key is skipped before assignment, never assigned:
 * `out['__proto__'] = v` would set the copy's prototype instead.
 */
export function dropProtoKeys(value: unknown): unknown {
	if (Array.isArray(value)) return value.map(dropProtoKeys);
	if (!isPlainObject(value)) return value;
	const out: Record<string, unknown> = {};
	for (const key of Object.keys(value)) {
		if (key === '__proto__') continue;
		out[key] = dropProtoKeys(value[key]);
	}
	return out;
}

/** The JSONB columns an issue-page occurrence row can carry (§3.4). */
const JSONB_KEYS = [
	'stacktrace',
	'metadata',
	'sdk',
	'user',
	'tags',
	'contexts',
	'request',
	'breadcrumbs',
	'exception',
] as const;

/** `row` with each JSONB key it carries passed through `dropProtoKeys`; every other key copied unchanged. */
export function cleanOccurrenceRow<T extends object>(row: T): T {
	const out: Record<string, unknown> = { ...(row as Record<string, unknown>) };
	for (const key of JSONB_KEYS) {
		if (Object.hasOwn(row, key)) out[key] = dropProtoKeys(out[key]);
	}
	return out as T;
}
