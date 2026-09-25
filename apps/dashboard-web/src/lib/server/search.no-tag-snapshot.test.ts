import { describe, it, expect, vi, beforeEach } from 'vitest';

// sentinel-issue-tag-facets spec §6.6 (Compatibility): with no tag filter, searchErrors builds exactly
// today's SQL. The snapshots below were GENERATED FROM f49721f's search.ts, before the tag-filter change
// touched it, and are committed with the change: they render the result and count statements (SQL text
// and parameters) of 12 no-tag filter combinations through the real drizzle builder over a recording
// client (no database). Any drift in the no-tag path is a snapshot failure.

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

let scopes = new Map<string, string[] | null>();
vi.mock('./issue-access', () => ({
	getAccessibleProjects: vi.fn(async () => [{ id: 'p-1' }, { id: 'p-2' }]),
}));
vi.mock('./environment-access', async (importOriginal) => ({
	...(await importOriginal<Record<string, unknown>>()),
	resolveEnvironmentScopes: vi.fn(async () => scopes),
}));

const { searchErrors } = await import('./search');

const USER = { id: 'user-1' } as never;

const COMBINATIONS: [string, Record<string, string>, Map<string, string[] | null>?][] = [
	['no filter', {}],
	['user_id', { userId: 'u-1' }],
	['tenant_id', { tenantId: 't-1' }],
	['trace_id', { traceId: 'tr-1' }],
	['span_id', { spanId: 'sp-1' }],
	['request_id', { requestId: 'rq-1' }],
	['user_id + trace_id (the OR group)', { userId: 'u-1', traceId: 'tr-1' }],
	['project', { projectId: 'p-2' }],
	['environment', { environment: 'staging' }],
	['platform', { platform: 'go' }],
	['search', { search: 'timeout *conn* fail*' }],
	[
		'everything, under an environment scope',
		{ userId: 'u-1', tenantId: 't-1', projectId: 'p-1', environment: 'production', platform: 'javascript', search: 'boom' },
		new Map([['p-1', ['production']]]),
	],
];

beforeEach(() => {
	statements.length = 0;
	scopes = new Map();
});

describe('searchErrors without a tag filter is exactly today (spec §6.6)', () => {
	for (const [name, filters, scoped] of COMBINATIONS) {
		it(name, async () => {
			if (scoped) scopes = scoped;
			await searchErrors(USER, filters, { limit: 50, offset: 0 });
			expect(statements).toHaveLength(2);
			expect(statements).toMatchSnapshot();
		});
	}
});
