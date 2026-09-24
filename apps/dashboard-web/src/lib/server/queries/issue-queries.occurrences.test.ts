import { describe, it, expect, vi, beforeEach } from 'vitest';
import { errorOccurrences } from '$lib/db/schema';

// sentinel-event-schema-v2 §8.3 / V7e: the issue page's occurrence read keeps its payload unchanged.
// With the 17 v2 columns declared on the Drizzle table, a bare `select()` would start shipping every
// v2 JSONB body (breadcrumbs, contexts, request, ...) of up to 100 occurrences to the page -- a
// UI-visible change in a feature that promises none. So `getOccurrencesByIssueId` projects exactly the
// 12 pre-v2 columns. Asserted on the ARGUMENT of the select call (B10), with the real schema.

function makeChainable() {
	const m: any = {};
	for (const name of ['select', 'from', 'where', 'orderBy', 'limit']) {
		m[name] = vi.fn(() => m);
	}
	m.then = vi.fn((resolve: any) => resolve([]));
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

beforeEach(() => {
	vi.clearAllMocks();
});

describe('issueQueries.getOccurrencesByIssueId (V7e)', () => {
	it('selects exactly the 12 pre-v2 columns, each bound to its own errorOccurrences column', async () => {
		await issueQueries.getOccurrencesByIssueId('issue-1');
		expect(dbMock.select).toHaveBeenCalledTimes(1);
		const projection = dbMock.select.mock.calls[0][0];
		expect(projection, 'a bare select() ships every column the table declares').toBeDefined();
		expect(Object.keys(projection).sort()).toEqual([...PRE_V2_COLUMNS].sort());
		for (const key of PRE_V2_COLUMNS) {
			expect(projection[key]).toBe((errorOccurrences as any)[key]);
		}
	});

	it('selects no v2 column', async () => {
		await issueQueries.getOccurrencesByIssueId('issue-1');
		const projection = dbMock.select.mock.calls[0][0];
		const selected = new Set(Object.values(projection));
		for (const key of V2_COLUMNS) {
			expect(selected.has((errorOccurrences as any)[key]), `v2 column ${key} must not reach the issue page`).toBe(false);
		}
	});

	it('the table really declares the v2 columns (otherwise the assertion above proves nothing)', () => {
		for (const key of V2_COLUMNS) {
			expect((errorOccurrences as any)[key], key).toBeDefined();
		}
	});
});
