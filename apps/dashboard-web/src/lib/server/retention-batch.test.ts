import { describe, it, expect } from 'vitest';
import {
	RETENTION_OCCURRENCE_BATCH,
	RETENTION_OCCURRENCE_DUTY,
	RETENTION_OCCURRENCE_PAUSE_FLOOR_MS,
	RETENTION_OCCURRENCE_BUDGET_MS,
	RETENTION_OCCURRENCE_MIN_REMAINING_MS,
	RETENTION_OCCURRENCE_HELD_CEILING_MS,
	RETENTION_CYCLE_OUTSIDE_HOLD_ALLOWANCE_MS,
	RETENTION_AGING_PEAK_PER_HOUR,
	RETENTION_BACKLOG_WARN_SECONDS,
	RETENTION_OCCURRENCE_LOCK_TIMEOUT_MS,
	RETENTION_OCCURRENCE_LOCK_PHASE_TIMEOUT_MS,
	RETENTION_OCCURRENCE_DELETE_TIMEOUT_MS,
	RETENTION_OCCURRENCE_BATCH_FLOOR,
	RETENTION_ROUTE_OTHER_STEPS_ALLOWANCE_MS,
	RETENTION_ROUTE_DEADLINE_MS,
	RETENTION_RUN_MIN_STATEMENT_MS,
	RETENTION_RUN_MIN_CAP_MS,
	RETENTION_ROUTE_LOCK_STALE_MS,
	RETENTION_TAG_REBUILD_BUDGET_MS,
	RETENTION_TAG_REBUILD_PROBE_BUDGET_MS,
	RETENTION_TAG_REBUILD_PROBE_TIMEOUT_MS,
	RETENTION_TAG_REBUILD_FIXED_MS,
	RETENTION_TAG_REBUILD_US_PER_OCCURRENCE,
	retentionPauseMs,
	heldPercentile,
	occurrenceStopReason,
	nextOccurrenceBatch,
	OccurrenceBudgetSpent,
	retentionRunOutcome,
	retentionRunCapacity,
	tagRebuildEstimateMs,
	tagRebuildCapFull
} from './retention-batch';

// sentinel-issue-tag-facets spec §5 and §10.2 "retention pacing" (unit half): the constants and the pure
// functions the retention step's pacing, stop classification and outcome rest on.

describe('retention-batch constants (spec §5)', () => {
	it('has the values the keep-up derivation was made with', () => {
		expect(RETENTION_OCCURRENCE_BATCH).toBe(200);
		expect(RETENTION_OCCURRENCE_DUTY).toBe(0.5);
		expect(RETENTION_OCCURRENCE_PAUSE_FLOOR_MS).toBe(50);
		expect(RETENTION_OCCURRENCE_BUDGET_MS).toBe(120_000);
		expect(RETENTION_OCCURRENCE_MIN_REMAINING_MS).toBe(1_000);
		expect(RETENTION_OCCURRENCE_HELD_CEILING_MS).toBe(150);
		expect(RETENTION_CYCLE_OUTSIDE_HOLD_ALLOWANCE_MS).toBe(25);
		expect(RETENTION_AGING_PEAK_PER_HOUR).toBe(34_481);
		expect(RETENTION_BACKLOG_WARN_SECONDS).toBe(7_200);
		expect(RETENTION_OCCURRENCE_LOCK_TIMEOUT_MS).toBe(250);
		expect(RETENTION_OCCURRENCE_LOCK_PHASE_TIMEOUT_MS).toBe(1_000);
		expect(RETENTION_OCCURRENCE_DELETE_TIMEOUT_MS).toBe(1_000);
		expect(RETENTION_OCCURRENCE_BATCH_FLOOR).toBe(25);
		expect(RETENTION_ROUTE_OTHER_STEPS_ALLOWANCE_MS).toBe(30_000);
		expect(RETENTION_ROUTE_DEADLINE_MS).toBe(170_000);
		expect(RETENTION_RUN_MIN_STATEMENT_MS).toBe(100);
		expect(RETENTION_RUN_MIN_CAP_MS).toBe(1);
		expect(RETENTION_ROUTE_LOCK_STALE_MS).toBe(240_000);
		expect(RETENTION_TAG_REBUILD_BUDGET_MS).toBe(5_000);
		expect(RETENTION_TAG_REBUILD_PROBE_BUDGET_MS).toBe(2_000);
		expect(RETENTION_TAG_REBUILD_PROBE_TIMEOUT_MS).toBe(1_000);
		expect(RETENTION_TAG_REBUILD_FIXED_MS).toBe(100);
		expect(RETENTION_TAG_REBUILD_US_PER_OCCURRENCE).toBe(6);
	});

	it('the budgets fit the run deadline, which a live run ends by before the stale bound', () => {
		expect(
			RETENTION_OCCURRENCE_BUDGET_MS +
				RETENTION_TAG_REBUILD_BUDGET_MS +
				RETENTION_TAG_REBUILD_PROBE_BUDGET_MS +
				2 * RETENTION_TAG_REBUILD_PROBE_TIMEOUT_MS +
				RETENTION_ROUTE_OTHER_STEPS_ALLOWANCE_MS
		).toBeLessThanOrEqual(RETENTION_ROUTE_DEADLINE_MS);
		expect(RETENTION_ROUTE_DEADLINE_MS + 60_000).toBeLessThanOrEqual(RETENTION_ROUTE_LOCK_STALE_MS);
		expect(RETENTION_ROUTE_LOCK_STALE_MS).toBeLessThan(3_600_000);
	});
});

describe('retentionPauseMs: max(floor, held × (1 − d) / d), d = 0.5', () => {
	it('keeps the duty cycle whatever a batch costs', () => {
		expect(retentionPauseMs(10)).toBe(50);
		expect(retentionPauseMs(100)).toBe(100);
		expect(retentionPauseMs(400)).toBe(400);
	});
});

describe('the keep-up inequality at the ceiling (spec §5 "Keep-up")', () => {
	it('floor(119,000 / (25 + 2 × 150)) × 200 = 73,200 ≥ 2 × the measured peak', () => {
		const capacity = retentionRunCapacity(RETENTION_CYCLE_OUTSIDE_HOLD_ALLOWANCE_MS + 2 * RETENTION_OCCURRENCE_HELD_CEILING_MS);
		expect(capacity).toBe(73_200);
		expect(capacity).toBeGreaterThanOrEqual(2 * RETENTION_AGING_PEAK_PER_HOUR);
		expect(2 * RETENTION_AGING_PEAK_PER_HOUR).toBe(68_962);
	});
});

describe('heldPercentile: nearest rank', () => {
	const range = (n: number) => Array.from({ length: n }, (_, i) => i + 1);
	it('is the ⌈q × n⌉-th smallest, 0 when no batch ran', () => {
		expect(heldPercentile(range(100), 0.95)).toBe(95);
		expect(heldPercentile(range(20), 0.95)).toBe(19);
		expect(heldPercentile([7], 0.95)).toBe(7);
		expect(heldPercentile([], 0.95)).toBe(0);
	});
	it('one outlier cannot move the gated statistic', () => {
		const holds = [...Array.from({ length: 99 }, () => 30), 900];
		expect(heldPercentile(holds, 0.95)).toBe(30);
		expect(Math.max(...holds)).toBe(900);
	});
	it('does not reorder its input', () => {
		const holds = [5, 1, 3];
		heldPercentile(holds, 0.5);
		expect(holds).toEqual([5, 1, 3]);
	});
});

describe('occurrenceStopReason: by SQLSTATE and phase', () => {
	it('classifies each stop', () => {
		for (const cap of [true, false]) {
			expect(occurrenceStopReason('55P03', 'lock', cap)).toBe('lock_timeout');
			expect(occurrenceStopReason('55P03', 'delete', cap)).toBe('lock_timeout');
			expect(occurrenceStopReason('57014', 'lock', cap)).toBe('lock_timeout');
		}
		expect(occurrenceStopReason('57014', 'delete', true)).toBe('budget');
		expect(occurrenceStopReason('57014', 'delete', false)).toBe('delete_timeout');
	});
});

describe('nextOccurrenceBatch: halve down to the floor, then stop', () => {
	it('halves', () => {
		expect(nextOccurrenceBatch(200, 25)).toBe(100);
		expect(nextOccurrenceBatch(100, 25)).toBe(50);
		expect(nextOccurrenceBatch(50, 25)).toBe(25);
		expect(nextOccurrenceBatch(25, 25)).toBeNull();
		expect(nextOccurrenceBatch(7, 2)).toBe(3);
		expect(nextOccurrenceBatch(7, 5)).toBe(5);
		expect(nextOccurrenceBatch(5, 5)).toBeNull();
		expect(nextOccurrenceBatch(7, 25)).toBeNull();
	});
});

describe('OccurrenceBudgetSpent', () => {
	it('is an Error the loop can recognise', () => {
		const e = new OccurrenceBudgetSpent();
		expect(e).toBeInstanceOf(Error);
		expect(e).toBeInstanceOf(OccurrenceBudgetSpent);
		expect(e.name).toBe('OccurrenceBudgetSpent');
	});
});

describe('retentionRunOutcome: behind iff stopped early and (nothing deleted or backlog > 2 h)', () => {
	const r = (occurrenceStopped: 'drained' | 'budget' | 'lock_timeout' | 'delete_timeout', deletedOccurrences: number, occurrenceBacklogSeconds: number) =>
		retentionRunOutcome({ occurrenceStopped, deletedOccurrences, occurrenceBacklogSeconds });
	it('a drained run is completed', () => {
		expect(r('drained', 0, 0)).toBe('completed');
	});
	it('an early stop that deleted nothing is behind', () => {
		expect(r('lock_timeout', 0, 0)).toBe('behind');
		expect(r('delete_timeout', 0, 0)).toBe('behind');
		expect(r('budget', 0, 0)).toBe('behind');
	});
	it('an early stop that made progress inside the backlog bound is completed', () => {
		expect(r('lock_timeout', 5, 100)).toBe('completed');
		expect(r('budget', 5, 7_200)).toBe('completed');
	});
	it('an early stop past the backlog bound is behind', () => {
		expect(r('budget', 5, 7_201)).toBe('behind');
	});
	it('a drained run is completed whatever its backlog (tfs-rev-behind-drained-late-rows)', () => {
		expect(r('drained', 5, 7_201)).toBe('completed');
		// a 10-day-old event stored mid-step at DATA_RETENTION_DAYS = 7
		expect(r('drained', 0, 259_200)).toBe('completed');
	});
});

describe('the deferred rebuild estimate (spec §5 "Deferred rebuilds")', () => {
	it('est(n) = 100 ms + 6 µs × n; capFull = floor((budget − 100 ms) / 6 µs)', () => {
		expect(tagRebuildEstimateMs(0)).toBe(100);
		expect(tagRebuildEstimateMs(1_000_000)).toBe(6_100);
		expect(tagRebuildCapFull(RETENTION_TAG_REBUILD_BUDGET_MS)).toBe(816_666);
		// the head-of-line case's overrides: 450 ms budget, 100 ms fixed, 1,000 µs per occurrence
		expect(tagRebuildCapFull(450, 100, 1_000)).toBe(350);
		expect(tagRebuildEstimateMs(349, 100, 1_000)).toBe(449);
		expect(tagRebuildCapFull(0)).toBeLessThan(0);
	});
});
