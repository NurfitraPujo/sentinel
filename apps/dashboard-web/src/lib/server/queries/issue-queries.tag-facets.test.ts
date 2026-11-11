import { describe, it, expect, vi, beforeEach } from 'vitest';
import { PgDialect } from 'drizzle-orm/pg-core';
import type { SQL } from 'drizzle-orm';

// sentinel-issue-tag-facets spec §7.2 (TASK-02): getTagFacets is ONE statement per page load (never one
// per key), over issue_tag_values for one issue, with the derived-key order taken from
// issue_tag_derived (never a TypeScript list). bigint counts come back as numbers.

const calls: string[] = [];
const execute = vi.fn(async (_q: SQL) => [
	{ key: 'release', value: '1.0', count: '6', key_total: '10', derived_ord: 1 },
	{ key: 'release', value: '1.1', count: '4', key_total: '10', derived_ord: 1 },
	{ key: 'tenant', value: 'acme', count: '2', key_total: '2', derived_ord: null },
]);
const dbMock = new Proxy(
	{ execute },
	{
		get(target, prop: string) {
			calls.push(prop);
			return (target as Record<string, unknown>)[prop] ?? (() => {
				throw new Error(`getTagFacets used db.${prop}`);
			});
		},
	}
);
vi.mock('$lib/server/db', () => ({ db: dbMock }));

const { issueQueries } = await import('./issue-queries');

beforeEach(() => {
	vi.clearAllMocks();
	calls.length = 0;
});

describe('issueQueries.getTagFacets', () => {
	it('issues exactly one statement and maps the rows', async () => {
		const rows = await issueQueries.getTagFacets('11111111-1111-1111-1111-111111111111');
		expect(execute).toHaveBeenCalledTimes(1);
		expect(calls).toEqual(['execute']);
		expect(rows).toEqual([
			{ key: 'release', value: '1.0', count: 6, keyTotal: 10, derivedOrd: 1 },
			{ key: 'release', value: '1.1', count: 4, keyTotal: 10, derivedOrd: 1 },
			{ key: 'tenant', value: 'acme', count: 2, keyTotal: 2, derivedOrd: null },
		]);
	});

	it("is spec §7.2's query: one issue's rollup, top 5 plus the overflow row, ordered by issue_tag_derived", async () => {
		await issueQueries.getTagFacets('11111111-1111-1111-1111-111111111111');
		const { sql: text, params } = new PgDialect().sqlToQuery(execute.mock.calls[0][0]);
		const flat = text.replace(/\s+/g, ' ');
		expect(flat).toContain('FROM issue_tag_values t');
		expect(flat).toContain('WHERE t.issue_id = $1');
		expect(flat).toContain("row_number() OVER (PARTITION BY t.key ORDER BY (t.value = ''), t.count DESC, t.value COLLATE \"C\")");
		expect(flat).toContain('LEFT JOIN issue_tag_derived(NULL, NULL, NULL, NULL) d ON d.key = s.key');
		expect(flat).toContain("WHERE s.rn <= 5 OR s.value = ''");
		expect(flat).toContain('ORDER BY d.ord NULLS LAST, s.key COLLATE "C", s.rn');
		expect(params).toEqual(['11111111-1111-1111-1111-111111111111']);
	});
});
