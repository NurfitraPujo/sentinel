import { describe, it, expect } from 'vitest';
import { buildWaterfall, formatDuration, isFailedStatus, MIN_WIDTH_PCT, type WaterfallError, type WaterfallSpan } from './waterfall';

// sentinel-span-ingest D6: the layout rules — nesting by parent, orphans as roots, lanes, pinning, the cap.

const T0 = Date.parse('2026-10-02T10:00:00.000Z') * 1000; // µs
const span = (spanId: string, over: Partial<WaterfallSpan> = {}): WaterfallSpan => ({
	spanId,
	parentSpanId: '',
	name: `name-${spanId}`,
	op: '',
	kind: '',
	source: 'pulled',
	serviceName: 'core-api',
	projectName: null,
	environment: 'production',
	startUs: T0,
	durationUs: 1000,
	status: 'ok',
	attributes: {},
	...over,
});
const err = (id: string, spanId: string | null, atUs: number): WaterfallError => ({
	id,
	spanId,
	createdAt: new Date(atUs / 1000).toISOString(),
	errorClass: 'TypeError',
	message: 'boom',
	issueId: 'issue-' + id,
});
const rowsOf = (wf: ReturnType<typeof buildWaterfall>, lane = 0) => wf.lanes[lane].rows.map((r) => [r.span.spanId, r.depth]);

describe('buildWaterfall', () => {
	it('nests children under their parent, in start order, depth counted within the lane', () => {
		const wf = buildWaterfall(
			[
				span('c', { parentSpanId: 'b', startUs: T0 + 300 }),
				span('a', { startUs: T0 }),
				span('b', { parentSpanId: 'a', startUs: T0 + 100 }),
				span('d', { parentSpanId: 'a', startUs: T0 + 400 }),
			],
			[]
		);
		expect(wf.lanes).toHaveLength(1);
		expect(rowsOf(wf)).toEqual([['a', 0], ['b', 1], ['c', 2], ['d', 1]]);
	});

	it('draws a span whose parent is not stored as a root (the browser’s unrecorded span, a dropped span)', () => {
		const wf = buildWaterfall(
			[span('a', { parentSpanId: 'ffffffffffffffff' }), span('b', { parentSpanId: 'a', startUs: T0 + 10 })],
			[]
		);
		expect(rowsOf(wf)).toEqual([['a', 0], ['b', 1]]);
	});

	it('survives a parent loop and a self-parent without dropping or repeating a span', () => {
		const wf = buildWaterfall(
			[
				span('a', { parentSpanId: 'b' }),
				span('b', { parentSpanId: 'a', startUs: T0 + 1 }),
				span('s', { parentSpanId: 's', startUs: T0 + 2 }),
			],
			[]
		);
		expect(wf.lanes[0].rows.map((r) => r.span.spanId).sort()).toEqual(['a', 'b', 's']);
		expect(wf.spanCount).toBe(3);
	});

	it('groups into one lane per service and source, and counts depth only across same-lane ancestors', () => {
		const wf = buildWaterfall(
			[
				span('m', { source: 'mobile', serviceName: '', projectName: 'zasri-mobile' }),
				span('api', { parentSpanId: 'm', startUs: T0 + 10, serviceName: 'core-api' }),
				span('db', { parentSpanId: 'api', startUs: T0 + 20, serviceName: 'core-db' }),
				span('api2', { parentSpanId: 'api', startUs: T0 + 30, serviceName: 'core-api' }),
			],
			[]
		);
		expect(wf.lanes.map((l) => l.label)).toEqual(['zasri-mobile', 'core-api', 'core-db']);
		expect(wf.lanes.map((l) => l.source)).toEqual(['mobile', 'pulled', 'pulled']);
		expect(rowsOf(wf, 1)).toEqual([['api', 0], ['api2', 1]]);
		expect(rowsOf(wf, 2)).toEqual([['db', 0]]);
	});

	it('pins an error to the span carrying its spanId, and puts the rest in the errors lane by time', () => {
		const wf = buildWaterfall(
			[span('a', { durationUs: 1_000_000 }), span('b', { parentSpanId: 'a', startUs: T0 + 500_000, durationUs: 100_000 })],
			[err('e1', 'b', T0 + 550_000), err('e2', 'ffffffffffffffff', T0 + 900_000), err('e3', null, T0 + 100_000)]
		);
		const b = wf.lanes[0].rows.find((r) => r.span.spanId === 'b')!;
		expect(b.errors.map((p) => p.error.id)).toEqual(['e1']);
		expect(wf.lanes[0].rows.find((r) => r.span.spanId === 'a')!.errors).toEqual([]);
		expect(wf.unmatchedErrors.map((p) => p.error.id)).toEqual(['e3', 'e2']); // by time, not input order
		expect(b.errors[0].offsetPct).toBeCloseTo(55, 5);
	});

	it('widens the axis to an error outside every span, and gives a tiny span a visible bar', () => {
		const wf = buildWaterfall([span('a', { durationUs: 1 })], [err('e', null, T0 + 1_000_000)]);
		expect(wf.endUs - wf.startUs).toBe(1_000_000);
		expect(wf.lanes[0].rows[0].widthPct).toBe(MIN_WIDTH_PCT);
		expect(wf.unmatchedErrors[0].offsetPct).toBe(100);
	});

	it('caps at the render limit, keeps the earliest spans and reports how many were left out', () => {
		const many = Array.from({ length: 12 }, (_, i) => span(`s${String(i).padStart(2, '0')}`, { startUs: T0 + i }));
		const wf = buildWaterfall(many, [], 10);
		expect(wf.spanCount).toBe(10);
		expect(wf.truncated).toBe(2);
		expect(wf.lanes[0].rows.map((r) => r.span.spanId)).not.toContain('s11');
		expect(buildWaterfall(many, [], 12).truncated).toBe(0);
	});

	it('is empty (no lanes, no axis) with no spans, and still reports errors it cannot place on a span', () => {
		expect(buildWaterfall([], [])).toMatchObject({ lanes: [], spanCount: 0, truncated: 0 });
		const wf = buildWaterfall([], [err('e', null, T0)]);
		expect(wf.lanes).toEqual([]);
		expect(wf.unmatchedErrors).toEqual([]); // no axis to place it on: the page still lists the occurrence
	});

	it('marks failed spans from the status, and shows an error once when a span id is duplicated', () => {
		expect(['', 'ok', 'unset', 'cancelled'].some(isFailedStatus)).toBe(false);
		expect(['error', 'internal_error', 'deadline_exceeded'].every(isFailedStatus)).toBe(true);
		const wf = buildWaterfall([span('a'), span('a', { startUs: T0 + 5, status: 'error' })], [err('e', 'a', T0)]);
		expect(wf.lanes[0].rows.flatMap((r) => r.errors)).toHaveLength(1);
	});

	it('formats durations in the unit that keeps them readable', () => {
		expect([formatDuration(340), formatDuration(1250), formatDuration(15_000), formatDuration(2_400_000), formatDuration(-1)]).toEqual([
			'340 µs',
			'1.25 ms',
			'15 ms',
			'2.4 s',
			'—',
		]);
	});
});
