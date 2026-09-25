// sentinel-issue-tag-facets spec §5: the retention route's constants and the pure functions its occurrence
// step, its deferred rebuilds and its outcome are built from. Constants and pure functions ONLY: this module
// imports nothing from $lib/server/db (the route imports it for its constants, and route tests do not mock
// it), and every constant lives here, never in a route file (B12: a route may export only its handlers).
// tests/unit/tag_facets_gate_test.go reads this file and asserts the budgets fit the run deadline and
// curl's --max-time (values.yaml retentionCron.timeoutSeconds) with 10 s to spare.

// ---- The occurrence step (spec §5 "What a batch costs ingest", "Keep-up") ----

/** Rows per batch transaction. The hold, and so a delivery's worst wait, scales with it. */
export const RETENTION_OCCURRENCE_BATCH = 200;
/** A hot issue is free at least half of the step's wall-clock time. */
export const RETENTION_OCCURRENCE_DUTY = 0.5;
export const RETENTION_OCCURRENCE_PAUSE_FLOOR_MS = 50;
/** The step's wall-clock budget per run. */
export const RETENTION_OCCURRENCE_BUDGET_MS = 120_000;
/** No batch starts with less of the step's budget left. */
export const RETENTION_OCCURRENCE_MIN_REMAINING_MS = 1_000;
/** The p95 batch hold P5 gates on; the keep-up margin of 2 is derived at a hold this high. */
export const RETENTION_OCCURRENCE_HELD_CEILING_MS = 150;
/** r: a batch's time outside its hold (candidate read, BEGIN, round trips); measured 9.5-9.7 ms. */
export const RETENTION_CYCLE_OUTSIDE_HOLD_ALLOWANCE_MS = 25;
/** A: production's measured peak hourly aging rate (spec T30). */
export const RETENTION_AGING_PEAK_PER_HOUR = 34_481;
/** The oldest surviving occurrence may lag the cutoff by this much before a run is behind. */
export const RETENTION_BACKLOG_WARN_SECONDS = 7_200;
/** lock_timeout of a batch's issue locks and DELETE; a queued delivery waits ≤ lock phase + DELETE cap. */
export const RETENTION_OCCURRENCE_LOCK_TIMEOUT_MS = 250;
/** statement_timeout of a batch's issues FOR UPDATE. */
export const RETENTION_OCCURRENCE_LOCK_PHASE_TIMEOUT_MS = 1_000;
/** statement_timeout of a batch's DELETE: past it the batch is halved and retried. */
export const RETENTION_OCCURRENCE_DELETE_TIMEOUT_MS = 1_000;
/** A DELETE timeout at this size stops the step ('delete_timeout') rather than halving again. */
export const RETENTION_OCCURRENCE_BATCH_FLOOR = 25;

// ---- The run (spec §5 "One run, one connection, one deadline", "Budgets and the client timeout") ----

/** Three times the measured maximum of the route's other steps (2.5-9.9 s with nothing to delete, T31). */
export const RETENTION_ROUTE_OTHER_STEPS_ALLOWANCE_MS = 30_000;
/** Counted from the moment the route lock is granted; ≤ curl's --max-time − 10 s. */
export const RETENTION_ROUTE_DEADLINE_MS = 170_000;
/** No statement is sent with less of the run left than this. */
export const RETENTION_RUN_MIN_STATEMENT_MS = 100;
/** No statement_timeout / lock_timeout below this is ever sent (0 would mean no timeout). */
export const RETENTION_RUN_MIN_CAP_MS = 1;
/** A route-lock holder older than this cannot be a live run: the deadline plus 70 s. */
export const RETENTION_ROUTE_LOCK_STALE_MS = 240_000;

// ---- Deferred rebuilds (spec §5 "Deferred rebuilds") ----

export const RETENTION_TAG_REBUILD_BUDGET_MS = 5_000;
/** Probes before the first attempt stop once they have used this much. */
export const RETENTION_TAG_REBUILD_PROBE_BUDGET_MS = 2_000;
/** A size probe's statement_timeout; a probe that times out marks its issue operator-only. */
export const RETENTION_TAG_REBUILD_PROBE_TIMEOUT_MS = 1_000;
export const RETENTION_TAG_REBUILD_FIXED_MS = 100;
export const RETENTION_TAG_REBUILD_US_PER_OCCURRENCE = 6;

export type OccurrenceStopped = 'drained' | 'budget' | 'lock_timeout' | 'delete_timeout';
export type RetentionRunOutcome = 'completed' | 'behind';

/** The pause after a batch held for heldMs: max(floor, held × (1 − d) / d). */
export function retentionPauseMs(heldMs: number): number {
	return Math.max(
		RETENTION_OCCURRENCE_PAUSE_FLOOR_MS,
		(heldMs * (1 - RETENTION_OCCURRENCE_DUTY)) / RETENTION_OCCURRENCE_DUTY
	);
}

/** Nearest-rank percentile: the ⌈q × n⌉-th smallest hold, 0 when no batch ran. */
export function heldPercentile(heldMs: readonly number[], q: number): number {
	if (heldMs.length === 0) return 0;
	const sorted = [...heldMs].sort((a, b) => a - b);
	const rank = Math.max(1, Math.ceil(q * sorted.length));
	return sorted[Math.min(rank, sorted.length) - 1];
}

/**
 * Why a batch stopped, by SQLSTATE and phase: a lock wait (55P03 in either phase, or 57014 while locking
 * issues) is 'lock_timeout'; a 57014 in the DELETE is 'budget' when its cap was the budget left, else
 * 'delete_timeout' (the DELETE itself ran past its cap).
 */
export function occurrenceStopReason(
	code: string | null | undefined,
	phase: 'lock' | 'delete',
	deleteCapIsBudget: boolean
): 'lock_timeout' | 'budget' | 'delete_timeout' {
	if (code === '55P03' || phase === 'lock') return 'lock_timeout';
	return deleteCapIsBudget ? 'budget' : 'delete_timeout';
}

/** The batch size to retry the same oldest rows at after a DELETE timeout; null at the floor (stop). */
export function nextOccurrenceBatch(size: number, floor: number): number | null {
	if (size <= floor) return null;
	return Math.max(floor, Math.floor(size / 2));
}

/**
 * Thrown inside a batch's transaction callback when less than RETENTION_RUN_MIN_STATEMENT_MS of the step is
 * left before a cap would be set: the batch rolls back (D18: thrown, never returned) and the step stops
 * with 'budget'. It is what keeps every cap the loop sets ≥ 100 ms.
 */
export class OccurrenceBudgetSpent extends Error {
	constructor() {
		super('retention occurrence step: budget spent before the next statement');
		this.name = 'OccurrenceBudgetSpent';
	}
}

/**
 * 'behind' iff the step stopped early (not 'drained') and either deleted nothing or ended with the oldest
 * surviving occurrence more than RETENTION_BACKLOG_WARN_SECONDS behind the cutoff. A drained run is
 * 'completed' whatever its backlog: anything aged it left was stored behind its cursor during the step.
 */
export function retentionRunOutcome(result: {
	occurrenceStopped: OccurrenceStopped;
	deletedOccurrences: number;
	occurrenceBacklogSeconds: number;
}): RetentionRunOutcome {
	if (result.occurrenceStopped === 'drained') return 'completed';
	if (result.deletedOccurrences === 0 || result.occurrenceBacklogSeconds > RETENTION_BACKLOG_WARN_SECONDS) {
		return 'behind';
	}
	return 'completed';
}

/** The most rows one run can delete at a mean batch cycle of cycleMs. */
export function retentionRunCapacity(cycleMs: number): number {
	return (
		Math.floor((RETENTION_OCCURRENCE_BUDGET_MS - RETENTION_OCCURRENCE_MIN_REMAINING_MS) / cycleMs) *
		RETENTION_OCCURRENCE_BATCH
	);
}

/** est(n): the time to rebuild an issue of n occurrences. */
export function tagRebuildEstimateMs(
	n: number,
	fixedMs: number = RETENTION_TAG_REBUILD_FIXED_MS,
	usPerOccurrence: number = RETENTION_TAG_REBUILD_US_PER_OCCURRENCE
): number {
	return fixedMs + (n * usPerOccurrence) / 1000;
}

/** The largest issue a rebuild budget can hold: floor((budget − fixed) / per-occurrence). */
export function tagRebuildCapFull(
	budgetMs: number,
	fixedMs: number = RETENTION_TAG_REBUILD_FIXED_MS,
	usPerOccurrence: number = RETENTION_TAG_REBUILD_US_PER_OCCURRENCE
): number {
	return Math.floor(((budgetMs - fixedMs) * 1000) / usPerOccurrence);
}
