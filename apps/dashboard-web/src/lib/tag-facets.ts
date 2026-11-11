// sentinel-issue-tag-facets spec §7.3 (TASK-02): the issue page's tag distribution blocks, built from the
// rows of `issueQueries.getTagFacets` (spec §7.2: each key's top 5 named values, its overflow row when it
// has one, and its total). Pure and client-safe.

/** One row of getTagFacets: `value = ''` is the key's overflow row ("Other"). */
export interface TagFacetRow {
	key: string;
	value: string;
	count: number;
	/** Σ of every rollup row of the key: the named values past the top 5 and the overflow row included. */
	keyTotal: number;
	/** The key's position in issue_tag_derived (1-7), null for a custom tag. */
	derivedOrd: number | null;
}

export interface TagFacetValue {
	value: string;
	count: number;
	/** One decimal; with the block's other lines it sums to exactly 100.0 (F4). */
	percent: number;
}

export interface TagFacetBlock {
	key: string;
	total: number;
	values: TagFacetValue[];
	/** Total − the shown values: the named values past the top 5 plus the overflow row; null at 0 (F3). */
	other: { count: number; percent: number } | null;
}

export const TAG_FACET_TOP_VALUES = 5;

const encoder = new TextEncoder();

/** Byte (UTF-8) order, as PostgreSQL's COLLATE "C" sorts: never locale order. */
export function compareBytes(a: string, b: string): number {
	if (a === b) return 0;
	const x = encoder.encode(a);
	const y = encoder.encode(b);
	const n = Math.min(x.length, y.length);
	for (let i = 0; i < n; i++) {
		if (x[i] !== y[i]) return x[i] - y[i];
	}
	return x.length - y.length;
}

/**
 * F4: each line gets floor(1000 × count / total) tenths; the tenths left over go, one each, to the lines
 * with the largest fractional parts (ties: the earlier line). So the lines sum to exactly 1000 tenths.
 */
export function largestRemainderTenths(counts: number[], total: number): number[] {
	if (counts.length === 0 || total <= 0) return counts.map(() => 0);
	const exact = counts.map((c) => (1000 * c) / total);
	const tenths = exact.map((e) => Math.floor(e));
	let left = 1000 - tenths.reduce((a, b) => a + b, 0);
	const byRemainder = exact
		.map((e, i) => ({ i, r: e - Math.floor(e) }))
		.sort((a, b) => b.r - a.r || a.i - b.i);
	for (let k = 0; left > 0 && k < byRemainder.length; k++, left--) {
		tenths[byRemainder[k].i] += 1;
	}
	return tenths;
}

/** F1-F6: one block per key, derived keys in issue_tag_derived order, then custom keys in byte order. */
export function buildTagFacets(rows: readonly TagFacetRow[]): TagFacetBlock[] {
	const byKey = new Map<string, { derivedOrd: number | null; total: number; named: TagFacetRow[] }>();
	for (const r of rows) {
		let entry = byKey.get(r.key);
		if (!entry) {
			entry = { derivedOrd: r.derivedOrd, total: r.keyTotal, named: [] };
			byKey.set(r.key, entry);
		}
		if (r.value !== '') entry.named.push(r);
	}
	const keys = [...byKey.keys()].sort((a, b) => {
		const oa = byKey.get(a)!.derivedOrd;
		const ob = byKey.get(b)!.derivedOrd;
		if (oa !== null && ob !== null) return oa - ob;
		if (oa !== null) return -1;
		if (ob !== null) return 1;
		return compareBytes(a, b);
	});
	return keys.map((key) => {
		const { total, named } = byKey.get(key)!;
		const shown = [...named]
			.sort((a, b) => b.count - a.count || compareBytes(a.value, b.value))
			.slice(0, TAG_FACET_TOP_VALUES);
		const otherCount = total - shown.reduce((n, r) => n + r.count, 0);
		const lines = [...shown.map((r) => r.count), ...(otherCount > 0 ? [otherCount] : [])];
		const tenths = largestRemainderTenths(lines, total);
		return {
			key,
			total,
			values: shown.map((r, i) => ({ value: r.value, count: r.count, percent: tenths[i] / 10 })),
			other: otherCount > 0 ? { count: otherCount, percent: tenths[shown.length] / 10 } : null,
		};
	});
}
