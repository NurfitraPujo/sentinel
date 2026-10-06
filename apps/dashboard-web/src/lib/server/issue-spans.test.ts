import { describe, it, expect, vi, beforeEach } from 'vitest';

// sentinel-occurrence-spans TASK-01: the bounded walk that finds an issue's occurrences WITH spans. What "has spans" means
// (the waterfall's guard) is span-pull.integration.test.ts's boundary; this file owns the walk: the bound, the batches,
// the early exit, the order, and the continuation point.

const scanOccurrenceTraces = vi.fn();
vi.mock('$lib/server/queries/issue-queries', () => ({ issueQueries: { scanOccurrenceTraces } }));
const getSpansStatuses = vi.fn();
vi.mock('$lib/server/queries/span-queries', () => ({ getSpansStatuses }));

const { walkOccurrencesWithSpans, statusByOccurrence, SPANS_SCAN_BOUND, SPANS_STATUS_BATCH } = await import('./issue-spans');

const scope = { orgId: 'org-1', projects: [{ id: 'p' }], environmentScopes: new Map() };
const trace = (n: number) => n.toString(16).padStart(32, '0');
/** `n` occurrences, newest first; every `step`-th has no trace id. */
const window = (n: number, noTraceEvery = 0) =>
	Array.from({ length: n }, (_, i) => ({
		id: `occ-${i}`,
		traceId: noTraceEvery && i % noTraceEvery === 0 ? null : trace(i),
		createdAt: new Date(Date.UTC(2026, 9, 6) - i * 1000),
	}));
/** `has(i)` -> the trace of occurrence i has spans. */
const spansWhere = (has: (i: number) => boolean) =>
	getSpansStatuses.mockImplementation(async (ids: string[]) => new Map(ids.map((id) => [id, has(parseInt(id, 16)) ? 'available' : 'none'])));

beforeEach(() => vi.clearAllMocks());

describe('walkOccurrencesWithSpans', () => {
	it('reads one bounded window off the issue’s index and asks for statuses SPANS_STATUS_BATCH traces at a time', async () => {
		scanOccurrenceTraces.mockResolvedValue(window(450));
		spansWhere(() => false);
		const r = await walkOccurrencesWithSpans({ issueId: 'i1', scope, want: 100 });
		expect(scanOccurrenceTraces).toHaveBeenCalledTimes(1);
		expect(scanOccurrenceTraces).toHaveBeenCalledWith('i1', SPANS_SCAN_BOUND, { offset: 0, before: undefined });
		expect(getSpansStatuses.mock.calls.map((c) => c[0].length)).toEqual([SPANS_STATUS_BATCH, SPANS_STATUS_BATCH, 50]);
		expect(r).toMatchObject({ ids: [], scanned: 450, exhausted: true, continueBefore: null });
	});

	it('returns matches newest first and stops asking once `want` are found', async () => {
		scanOccurrenceTraces.mockResolvedValue(window(1000));
		spansWhere((i) => i % 10 === 0); // 100 matches in 1000
		const r = await walkOccurrencesWithSpans({ issueId: 'i1', scope, want: 3 });
		expect(r.ids).toEqual(['occ-0', 'occ-10', 'occ-20']);
		expect(r.traceIds.get('occ-10')).toBe(trace(10));
		expect(getSpansStatuses).toHaveBeenCalledTimes(1); // the first batch already had 3
		expect(r.scanned).toBe(21); // inspected up to the last one returned, not the whole window
		expect(r.exhausted).toBe(false);
		expect(r.continueBefore).toEqual(new Date(Date.UTC(2026, 9, 6) - 20 * 1000)); // continue after the last returned
	});

	it('skips occurrences without a trace id without asking about them', async () => {
		scanOccurrenceTraces.mockResolvedValue(window(10, 2)); // 0,2,4,.. have no trace
		spansWhere(() => true);
		const r = await walkOccurrencesWithSpans({ issueId: 'i1', scope, want: 100 });
		expect(r.ids).toEqual(['occ-1', 'occ-3', 'occ-5', 'occ-7', 'occ-9']);
		expect(getSpansStatuses.mock.calls[0][0]).toHaveLength(5);
	});

	it('a full window means older occurrences may remain: not exhausted, and it says where to continue', async () => {
		scanOccurrenceTraces.mockResolvedValue(window(SPANS_SCAN_BOUND));
		spansWhere(() => false);
		const r = await walkOccurrencesWithSpans({ issueId: 'i1', scope, want: 100 });
		expect(r.exhausted).toBe(false);
		expect(r.scanned).toBe(SPANS_SCAN_BOUND);
		expect(r.continueBefore).toEqual(new Date(Date.UTC(2026, 9, 6) - (SPANS_SCAN_BOUND - 1) * 1000));
		// 10 status statements for 2,000 occurrences: the cost is bounded however large the issue is.
		expect(getSpansStatuses).toHaveBeenCalledTimes(SPANS_SCAN_BOUND / SPANS_STATUS_BATCH);
	});

	it('passes skip and before through to the scan (issue-level link look-back; the agent API’s cursor)', async () => {
		scanOccurrenceTraces.mockResolvedValue([]);
		const before = new Date('2026-01-01T00:00:00Z');
		await walkOccurrencesWithSpans({ issueId: 'i1', scope, want: 1, skip: 100, before, bound: 50 });
		expect(scanOccurrenceTraces).toHaveBeenCalledWith('i1', 50, { offset: 100, before });
		expect(getSpansStatuses).not.toHaveBeenCalled();
	});
});

describe('statusByOccurrence', () => {
	it('maps each listed occurrence to its trace’s status in ONE call; occurrences without a canonical trace id are absent', async () => {
		getSpansStatuses.mockResolvedValue(new Map([[trace(1), 'available'], [trace(2), 'pending']]));
		const r = await statusByOccurrence(
			[{ id: 'a', traceId: trace(1) }, { id: 'b', traceId: trace(2) }, { id: 'c', traceId: null }, { id: 'd', traceId: 'NOT-CANONICAL' }],
			scope
		);
		expect(r).toEqual({ a: 'available', b: 'pending' });
		expect(getSpansStatuses).toHaveBeenCalledTimes(1);
	});
});
