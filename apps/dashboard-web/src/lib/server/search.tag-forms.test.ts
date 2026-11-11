import { describe, it, expect, vi, beforeEach } from 'vitest';

// sentinel-issue-tag-facets spec §6.4 (TASK-03): which query form a tag search uses, and what goes inside
// the per-issue LATERAL. Rendered through the real drizzle builder over a recording client (no database):
// with no id filter a tag search is form L, with `issues.environment = $n` on the OUTER row, NO
// environment test inside the LATERAL, and the platform test inside it; with an id filter it is form J
// (today's join, the tag conditions added). Plus the rows-read metric (5c) on a canned plan.

const statements: { sql: string; params: unknown[] }[] = [];
const recordingClient = {
	options: { parsers: {}, serializers: {} },
	unsafe(query: string, params: unknown[]) {
		statements.push({ sql: query, params });
		const rows = Promise.resolve([] as unknown[]) as unknown as Promise<unknown[]> & { values: () => Promise<unknown[]> };
		rows.values = () => Promise.resolve([]);
		return rows;
	},
};
vi.mock('$lib/server/db', async () => {
	const { drizzle } = await import('drizzle-orm/postgres-js');
	return { db: drizzle(recordingClient as never) };
});
vi.mock('./issue-access', () => ({ getAccessibleProjects: vi.fn(async () => [{ id: 'p-1' }]) }));
vi.mock('./environment-access', async (importOriginal) => ({
	...(await importOriginal<Record<string, unknown>>()),
	resolveEnvironmentScopes: vi.fn(async () => new Map()),
}));

const { searchErrors, rowsReadFromPlan } = await import('./search');
const USER = { id: 'user-1' } as never;
const flat = (s: string) => s.replace(/\s+/g, ' ');

function lateral(sqlText: string): { inner: string; outer: string } {
	const s = flat(sqlText);
	const start = s.indexOf('CROSS JOIN LATERAL (');
	expect(start, 'a CROSS JOIN LATERAL').toBeGreaterThan(-1);
	let depth = 0;
	let end = -1;
	for (let i = start + 'CROSS JOIN LATERAL '.length; i < s.length; i++) {
		if (s[i] === '(') depth++;
		else if (s[i] === ')' && --depth === 0) {
			end = i;
			break;
		}
	}
	return { inner: s.slice(start, end + 1), outer: s.slice(0, start) + s.slice(end + 1) };
}

beforeEach(() => {
	statements.length = 0;
});

describe('tag search forms (spec §6.4)', () => {
	it('form L: issues.environment on the outer row, platform inside the LATERAL, no environment inside it', async () => {
		await searchErrors(USER, { tags: [{ key: 'release', value: '1.0' }], environment: 'staging', platform: 'go' });
		expect(statements).toHaveLength(2);
		for (const st of statements) {
			const { inner, outer } = lateral(st.sql);
			expect(outer).toMatch(/"issues"\."environment" = \$\d+/);
			// the LATERAL's own condition: everything between its WHERE and its ORDER BY / LIMIT
			const innerWhere = inner.slice(inner.indexOf(' WHERE '), inner.search(/ ORDER BY | LIMIT /));
			expect(innerWhere).toMatch(/o\.issue_id = "issues"\."id"/);
			expect(innerWhere).not.toMatch(/environment/i);
			expect(innerWhere).toMatch(/o\.platform = \$\d+/);
			expect(inner).toMatch(/JOIN error_search_index s ON s\.occurrence_id = o\.id/);
			expect(inner).toMatch(/LIMIT 1/);
			expect(st.params).toContain('staging');
			expect(st.params).toContain('go');
		}
		expect(flat(statements[0].sql)).toMatch(/ORDER BY o\.created_at DESC, o\.id DESC LIMIT 1/);
		expect(flat(statements[0].sql)).toMatch(/ORDER BY "issues"\."last_seen" DESC, "issues"\."id"/);
	});

	it('form J: with an id filter the tag search is today\'s join plus the tag conditions, never the LATERAL', async () => {
		await searchErrors(USER, { tags: [{ key: 'release', value: '1.0' }], userId: 'u-1', environment: 'staging' });
		for (const st of statements) {
			expect(flat(st.sql)).not.toContain('CROSS JOIN LATERAL (SELECT o.id');
			expect(flat(st.sql)).toContain('inner join "error_search_index"');
			expect(flat(st.sql)).toContain('issue_tag_values t');
			expect(flat(st.sql)).toMatch(/"error_occurrences"\."environment" = \$\d+/);
		}
	});

	it('every tag is its own ANDed element: two tags, two conditions', async () => {
		await searchErrors(USER, { tags: [{ key: 'release', value: '1.0' }, { key: 'k', value: 'v' }] });
		const s = flat(statements[1].sql);
		expect(s.match(/FROM issue_tag_values t WHERE/g)).toHaveLength(2);
		expect(s.match(/FROM issue_tag_values ov/g)).toHaveLength(2);
	});
});

describe('rowsReadFromPlan: (Actual Rows + Rows Removed by Filter + Rows Removed by Index Recheck) × Actual Loops', () => {
	it('adds both removed terms (a scan that reads 405 rows a loop and keeps 5 reads 20,250)', () => {
		const plan = {
			Plan: {
				'Node Type': 'Nested Loop',
				'Actual Rows': 5,
				'Actual Loops': 1,
				Plans: [
					{ 'Node Type': 'Seq Scan', 'Relation Name': 'issues', 'Actual Rows': 50, 'Actual Loops': 1 },
					{
						'Node Type': 'Index Scan',
						'Relation Name': 'error_occurrences',
						'Index Name': 'idx_error_occurrences_issue_id',
						'Actual Rows': 5,
						'Rows Removed by Filter': 400,
						'Actual Loops': 50,
					},
				],
			},
		};
		expect(rowsReadFromPlan(plan, new Map())).toBe(20_250);
	});

	it('counts a Bitmap Index Scan (by its index\'s table) and its Bitmap Heap Scan both', () => {
		const plan = {
			Plan: {
				'Node Type': 'Bitmap Heap Scan',
				'Relation Name': 'error_search_index',
				'Actual Rows': 30,
				'Rows Removed by Index Recheck': 2,
				'Actual Loops': 1,
				Plans: [{ 'Node Type': 'Bitmap Index Scan', 'Index Name': 'idx_error_search_user_id', 'Actual Rows': 30, 'Actual Loops': 1 }],
			},
		};
		expect(rowsReadFromPlan(plan, new Map([['idx_error_search_user_id', 'error_search_index']]))).toBe(62);
		expect(rowsReadFromPlan(plan, new Map())).toBe(32);
	});
});
