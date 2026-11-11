import { describe, it, expect, vi, beforeEach } from 'vitest';
import { errorOccurrences } from '$lib/db/schema';

// sentinel-occurrence-detail-panels §3.1 (Q1-Q4), superseding sentinel-event-schema-v2's V7e. The list
// read carries the 12 pre-v2 columns plus six LIGHT v2 columns for every listed occurrence; the six
// HEAVY JSONB bodies go only to the one selected occurrence, through getOccurrenceDetail. Asserted on
// the ARGUMENTS of the select/where calls (B10), with the real schema.

// eq/ne/and are replaced by descriptors so the WHERE argument can be compared structurally; the
// columns inside them are the real errorOccurrences columns.
vi.mock('drizzle-orm', async (importOriginal) => {
	const actual = await importOriginal<typeof import('drizzle-orm')>();
	return {
		...actual,
		eq: (col: unknown, val: unknown) => ({ op: 'eq', col, val }),
		ne: (col: unknown, val: unknown) => ({ op: 'ne', col, val }),
		and: (...conds: unknown[]) => ({ op: 'and', conds }),
	};
});

let result: unknown[] = [];
function makeChainable() {
	const m: any = {};
	for (const name of ['select', 'from', 'where', 'orderBy', 'limit']) {
		m[name] = vi.fn(() => m);
	}
	m.then = vi.fn((resolve: any) => resolve(result));
	return m;
}

const dbMock: any = makeChainable();
vi.mock('$lib/server/db', () => ({ db: dbMock }));

const { issueQueries } = await import('./issue-queries');

const PRE_V2_COLUMNS = [
	'id',
	'issueId',
	'environment',
	'platform',
	'releaseVersion',
	'stacktrace',
	'metadata',
	'traceId',
	'spanId',
	'eventId',
	'projectId',
	'createdAt',
] as const;

const LIGHT_COLUMNS = ['level', 'message', 'serverName', 'transaction', 'dist', 'sdk'] as const;

// Detail key → the errorOccurrences column it is bound to (`user` aliases eventUser, as agent-reads does).
const HEAVY_COLUMNS = {
	user: 'eventUser',
	tags: 'tags',
	contexts: 'contexts',
	request: 'request',
	breadcrumbs: 'breadcrumbs',
	exception: 'exception',
} as const;

const NEVER_SELECTED = ['logger', 'errorClass', 'source', 'traceFlags', 'receivedAt'] as const;

const V2_COLUMNS = [
	'level',
	'message',
	'errorClass',
	'logger',
	'serverName',
	'transaction',
	'dist',
	'source',
	'traceFlags',
	'receivedAt',
	'sdk',
	'eventUser',
	'tags',
	'contexts',
	'request',
	'breadcrumbs',
	'exception',
] as const;

const col = (key: string) => (errorOccurrences as any)[key];

beforeEach(() => {
	vi.clearAllMocks();
	result = [];
});

describe('issueQueries.getOccurrencesByIssueId (Q1)', () => {
	it('selects exactly the 12 pre-v2 plus the six light columns, each bound to its own column', async () => {
		await issueQueries.getOccurrencesByIssueId('issue-1');
		expect(dbMock.select).toHaveBeenCalledTimes(1);
		const projection = dbMock.select.mock.calls[0][0];
		expect(projection, 'a bare select() ships every column the table declares').toBeDefined();
		expect(Object.keys(projection).sort()).toEqual([...PRE_V2_COLUMNS, ...LIGHT_COLUMNS].sort());
		for (const key of [...PRE_V2_COLUMNS, ...LIGHT_COLUMNS]) {
			expect(projection[key], key).toBe(col(key));
		}
	});

	it('selects none of the heavy or unrendered v2 columns', async () => {
		await issueQueries.getOccurrencesByIssueId('issue-1');
		const projection = dbMock.select.mock.calls[0][0];
		const selected = new Set(Object.values(projection));
		for (const key of [...Object.values(HEAVY_COLUMNS), ...NEVER_SELECTED]) {
			expect(selected.has(col(key)), `v2 column ${key} must not reach the listed rows`).toBe(false);
		}
	});
});

describe('issueQueries.getOccurrenceDetail (Q2)', () => {
	it('projects the 18 list keys plus the six heavy keys, user bound to eventUser', async () => {
		await issueQueries.getOccurrenceDetail('i', 'o');
		const projection = dbMock.select.mock.calls[0][0];
		expect(Object.keys(projection).sort()).toEqual(
			[...PRE_V2_COLUMNS, ...LIGHT_COLUMNS, ...Object.keys(HEAVY_COLUMNS)].sort(),
		);
		for (const key of [...PRE_V2_COLUMNS, ...LIGHT_COLUMNS]) {
			expect(projection[key], key).toBe(col(key));
		}
		for (const [key, column] of Object.entries(HEAVY_COLUMNS)) {
			expect(projection[key], key).toBe(col(column));
		}
		const selected = new Set(Object.values(projection));
		for (const key of NEVER_SELECTED) {
			expect(selected.has(col(key)), key).toBe(false);
		}
	});

	it('scopes the read to both the occurrence id and the issue id, limit 1', async () => {
		await issueQueries.getOccurrenceDetail('i', 'o');
		expect(dbMock.from).toHaveBeenCalledWith(errorOccurrences);
		expect(dbMock.where).toHaveBeenCalledTimes(1);
		expect(dbMock.where.mock.calls[0][0]).toEqual({
			op: 'and',
			conds: [
				{ op: 'eq', col: errorOccurrences.id, val: 'o' },
				{ op: 'eq', col: errorOccurrences.issueId, val: 'i' },
			],
		});
		expect(dbMock.limit).toHaveBeenCalledWith(1);
	});

	it('returns the row, or null when none matches', async () => {
		result = [{ id: 'o' }];
		expect(await issueQueries.getOccurrenceDetail('i', 'o')).toEqual({ id: 'o' });
		result = [];
		expect(await issueQueries.getOccurrenceDetail('i', 'o')).toBeNull();
	});
});

describe('the v2 columns (Q3, kept from V7e)', () => {
	it('the table really declares the 17 v2 columns (otherwise the assertions above prove nothing)', () => {
		for (const key of V2_COLUMNS) {
			expect(col(key), key).toBeDefined();
		}
	});
});

describe('issueQueries.occurrenceExistsOutsideIssue (Q4)', () => {
	it('selects the id only, where id = o and issueId <> i, limit 1', async () => {
		await issueQueries.occurrenceExistsOutsideIssue('i', 'o');
		const projection = dbMock.select.mock.calls[0][0];
		expect(Object.keys(projection)).toEqual(['id']);
		expect(projection.id).toBe(errorOccurrences.id);
		expect(dbMock.from).toHaveBeenCalledWith(errorOccurrences);
		expect(dbMock.where.mock.calls[0][0]).toEqual({
			op: 'and',
			conds: [
				{ op: 'eq', col: errorOccurrences.id, val: 'o' },
				{ op: 'ne', col: errorOccurrences.issueId, val: 'i' },
			],
		});
		expect(dbMock.limit).toHaveBeenCalledWith(1);
	});

	it('is true when a row comes back and false when none does', async () => {
		result = [{ id: 'o' }];
		expect(await issueQueries.occurrenceExistsOutsideIssue('i', 'o')).toBe(true);
		result = [];
		expect(await issueQueries.occurrenceExistsOutsideIssue('i', 'o')).toBe(false);
	});
});
