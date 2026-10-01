import { db, openRetentionRunClient } from '$lib/server/db';
import {
	errorOccurrences,
	issues,
	attachments,
	issueComments,
	issueActivity,
	issueTombstones,
	projects,
	agentIdempotencyKeys,
	reportIdempotencyKeys,
	externalIssueReports,
	externalReporters,
	issueAffectedUsers,
	issueAffectedTenants,
	issueOccurrenceBuckets,
	releases,
} from '$lib/db/schema';
import { sql, lt, gte, eq, and, inArray, or, isNull } from 'drizzle-orm';
import type { PgColumn, PgTable } from 'drizzle-orm/pg-core';
import { AGENT_IDEMPOTENCY_RETENTION_DAYS } from '$lib/server/agent-idempotency';
import { REPORT_IDEMPOTENCY_RETENTION_DAYS } from '$lib/server/report-idempotency';
import { deleteObject, isStorageConfigured } from '$lib/server/storage';
import { log } from '$lib/server/observability/log';
import { recordReleaseNextRewriteFailure } from '$lib/server/metrics';
import {
	openRetentionRun,
	RetentionConnectionLost,
	RetentionDeadlineError,
	type RetentionRun,
} from '$lib/server/retention-run';
import {
	RETENTION_ROUTE_DEADLINE_MS,
	RETENTION_OCCURRENCE_BATCH,
	RETENTION_OCCURRENCE_BUDGET_MS,
	RETENTION_OCCURRENCE_MIN_REMAINING_MS,
	RETENTION_OCCURRENCE_HELD_CEILING_MS,
	RETENTION_BACKLOG_WARN_SECONDS,
	RETENTION_OCCURRENCE_LOCK_TIMEOUT_MS,
	RETENTION_OCCURRENCE_LOCK_PHASE_TIMEOUT_MS,
	RETENTION_OCCURRENCE_DELETE_TIMEOUT_MS,
	RETENTION_OCCURRENCE_BATCH_FLOOR,
	RETENTION_RUN_MIN_STATEMENT_MS,
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
	tagRebuildCapFull,
	tagRebuildEstimateMs,
	type OccurrenceStopped,
} from '$lib/server/retention-batch';

// sentinel-issue-tag-facets §5 rule 3: every statement of a retention run goes through the run's one
// connection (`run.db`), and the route's reapers take it as a trailing `exec`; every other caller keeps
// the pool through the default.
type Exec = typeof db;

// There is deliberately no "stale issue" count here any more. The stale-marking step this module used to
// run never did anything, and could not have: it set `status: 'stale'`, which issues.check_status does not
// permit (only 'unresolved' | 'resolved' | 'ignored'), while filtering on `status = 'open'`, which is also
// not a permitted value — so the WHERE never matched a row and the invalid SET was never reached. Two
// mutually-masking bugs: the unreachable filter is precisely what stopped the illegal write from ever
// throwing, and the endpoint reported markedStaleIssues: 0 forever while appearing to work.
//
// Both values came from scripts/db/init.sql, a third and stale schema that declared
// CHECK (status IN ('open','resolved','ignored')). That file has since been DELETED (2026-07-30) — it was
// unreferenced by any container or test and existed only as a source of wrong values to copy from. The concept is dropped rather than repaired: nothing
// consumed the count, no spec asks for a 'stale' state, and adding one would mean a migration widening a
// CHECK constraint to support a feature that has never run.
export interface RetentionResult {
	deletedOccurrences: number;
	deletedOrphanedIssues: number;
	retentionDays: number;
	cutoffDate: Date;
	// A04 (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md): the second, longer cutoff that gates
	// deletion of occurrence-less manual issues (issueType='user_report') — see orphanCondition
	// below. Manual issues never HAVE occurrence rows by construction, so gating them on the same
	// short `retentionDays` used for system-error issues would delete a legitimate, still-open
	// manually-reported issue for no reason other than its type.
	manualRetentionDays: number;
	// N8 (docs/audits/AGENT_AUTOMATION_AUDIT_2026-08-14.md A04, DECISIONS.md D20): one
	// `issue_tombstones` row is written per issue this run actually deleted, so a claim-holding /
	// question-awaiting agent gets a terminal 'issue_deleted' event on the feed instead of a silent
	// 404. `deletedTombstones` counts tombstones aged past `tombstoneRetentionDays` and pruned this
	// run -- the tombstone table is itself bounded, it does not grow without limit.
	tombstonesWritten: number;
	deletedTombstones: number;
	tombstoneRetentionDays: number;
	// sentinel-user-report-issue §6: the manual-issue retention sweep has to know about the SECOND
	// companion. `orphanCondition` below already ages an externally-sourced report out, because it
	// keys on `issues.issue_type = 'user_report'` -- which an external report is -- and the
	// `external_issue_reports` row goes with it through the FK cascade. What did NOT go with it is
	// the `external_reporters` row, which has no cascade from `issues` at all and would otherwise
	// outlive every report it ever made, forever. This counts the reporters reaped for that reason.
	deletedExternalReporters: number;
	// sentinel-affected-users §4.5 (decision 23): the number of `issue_affected_users` /
	// `issue_affected_tenants` rows the sweep removed this run -- the length of each side-table
	// DELETE's RETURNING, exactly as `deletedOccurrences` is the occurrence delete's -- and NOT the
	// number of issues whose count was recomputed. Written by `sweepAffectedSideTable` below.
	deletedAffectedUsers: number;
	deletedAffectedTenants: number;
	// sentinel-issue-trend §4.5 (flow R3): the number of `issue_occurrence_buckets` rows the sweep
	// removed this run -- the length of the bucket DELETE's RETURNING, beside `deletedOccurrences`
	// exactly as the two side-table counts are. Written by the bucket step in `cleanupRetainedData`.
	deletedOccurrenceBuckets: number;
	// sentinel-issue-tag-facets §5: the occurrence step. `deletedOccurrences` above is what this run
	// deleted in paced, issue-locked batches; how the step ended ('drained' when a candidate read returned
	// nothing, else 'budget' / 'lock_timeout' / 'delete_timeout'); the batch size it ended at; the batch
	// holds (p95: the statistic P5 gates on; max and over-ceiling are recorded, not gated); and the
	// standing keep-up signal, how far the oldest surviving occurrence lags the cutoff.
	occurrenceStopped: OccurrenceStopped;
	occurrenceBacklogSeconds: number;
	oldestOccurrenceAt: string | null;
	occurrenceBatches: number;
	occurrenceStepMs: number;
	occurrenceBatchSize: number;
	occurrenceHeldMaxMs: number;
	occurrenceHeldP95Ms: number;
	occurrenceBatchesOverCeiling: number;
	// sentinel-issue-tag-facets §3.4 W11 / §5: drift rows reported this run (0 normally), and the
	// deferred rebuilds: issues with a drift row not rebuilt, and of those the ones handed to the operator.
	tagRollupDriftEvents: number;
	tagRollupRebuildsPending: number;
	tagRollupRebuildsOperatorOnly: number;
	rebuildStepMs: number;
}

/**
 * Test seams for the occurrence step's clock and pause (sentinel-issue-tag-facets §10.2): `sleep` is the
 * pause between batches, `now` is the clock the step's own budget reads (`run.left()` keeps the real one).
 */
export interface RetentionDeps {
	sleep?: (ms: number) => Promise<void>;
	now?: () => number;
}

const defaultSleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, Math.max(0, ms)));

function pgErrorCode(e: unknown): string | null {
	const code = (e as { code?: unknown } | null)?.code;
	return typeof code === 'string' ? code : null;
}

interface OccurrenceStepResult {
	deletedOccurrences: number;
	occurrenceBatches: number;
	occurrenceStepMs: number;
	occurrenceBatchSize: number;
	occurrenceHeldMaxMs: number;
	occurrenceHeldP95Ms: number;
	occurrenceBatchesOverCeiling: number;
	occurrenceStopped: OccurrenceStopped;
	occurrenceBacklogSeconds: number;
	oldestOccurrenceAt: string | null;
}

/**
 * sentinel-issue-tag-facets §5: the occurrence delete, in batches of at most RETENTION_OCCURRENCE_BATCH
 * rows paced to a 50 % duty cycle, until a candidate read returns nothing or the step's budget is spent.
 * Each batch locks the touched issues rows FIRST, in id order (the lock order every writer of
 * issue_tag_values and every deleter of occurrences takes), then deletes; the delete trigger subtracts
 * the deleted rows' tag contributions and deletes rollup rows that reach 0, in the same transaction.
 * Every lock wait is bounded (lock_timeout 250 ms, the lock phase 1 s, the DELETE 1 s), a DELETE past its
 * cap is retried at half the size down to the floor, and a batch that stops early is simply redone by
 * the next hourly run, which starts from the oldest row. D18: the transaction callback has one exit, the
 * DELETE's rows; every failure is thrown and caught outside it, so the batch rolls back as a whole.
 */
async function deleteAgedOccurrences(run: RetentionRun, cutoffDate: Date, deps: RetentionDeps): Promise<OccurrenceStepResult> {
	const sleep = deps.sleep ?? defaultSleep;
	const now = deps.now ?? (() => performance.now());
	const cutoffIso = cutoffDate.toISOString();
	const stepStart = now();
	const stepStartReal = performance.now();
	// The step's budget left, never more than the run's.
	const left = () => Math.min(run.left(), RETENTION_OCCURRENCE_BUDGET_MS - (now() - stepStart));
	let batchSize = RETENTION_OCCURRENCE_BATCH;
	let after: { createdAt: string; id: string } | null = null; // keyset cursor; moves on commit only
	let stopped: OccurrenceStopped;
	const heldMs: number[] = [];
	let deletedOccurrences = 0;
	for (;;) {
		if (left() < RETENTION_OCCURRENCE_MIN_REMAINING_MS) {
			stopped = 'budget';
			break;
		}
		// A plain read on idx_error_occurrences_created_at. After the first batch it starts at the last
		// committed batch's (created_at, id), carried as text (a JS Date would drop the microseconds), so it
		// does not walk the index entries of the rows this run already deleted.
		const candidates: { id: string; issue_id: string; created_at_text: string }[] = await run.db.execute<{
			id: string;
			issue_id: string;
			created_at_text: string;
		}>(sql`
			SELECT id, issue_id, created_at::text AS created_at_text FROM error_occurrences
			 WHERE created_at < ${cutoffIso}::timestamptz
			   ${
					after === null
						? sql``
						: sql`AND created_at >= ${after.createdAt}::timestamptz
			   AND (created_at, id) > (${after.createdAt}::timestamptz, ${after.id}::uuid)`
				}
			 ORDER BY created_at, id
			 LIMIT ${batchSize}`);
		if (candidates.length === 0) {
			stopped = 'drained';
			break;
		}
		const ids = candidates.map((c) => c.id);
		const issueIds = [...new Set(candidates.map((c) => c.issue_id))];
		const t0 = performance.now();
		let phase: 'lock' | 'delete' = 'lock';
		let deleteCapIsBudget = false;
		let deleted: unknown[];
		try {
			deleted = await run.db.transaction(async (tx) => {
				if (left() < RETENTION_RUN_MIN_STATEMENT_MS) throw new OccurrenceBudgetSpent(); // never a cap < 100 ms
				run.setCaps({
					lockMs: Math.min(RETENTION_OCCURRENCE_LOCK_TIMEOUT_MS, left()),
					statementMs: Math.min(RETENTION_OCCURRENCE_LOCK_PHASE_TIMEOUT_MS, left()),
				});
				// 1. issues first, in id order.
				await tx.execute(sql`
					SELECT id FROM issues WHERE id = ANY(${sql.param(issueIds)}::uuid[])
					 ORDER BY id FOR UPDATE`);
				phase = 'delete';
				if (left() < RETENTION_RUN_MIN_STATEMENT_MS) throw new OccurrenceBudgetSpent();
				const deleteCap = Math.min(RETENTION_OCCURRENCE_DELETE_TIMEOUT_MS, left());
				deleteCapIsBudget = deleteCap < RETENTION_OCCURRENCE_DELETE_TIMEOUT_MS;
				run.setCaps({ lockMs: Math.min(RETENTION_OCCURRENCE_LOCK_TIMEOUT_MS, left()), statementMs: deleteCap });
				// 2. the delete; the trigger subtracts and deletes rollup rows that reach 0.
				return tx.execute(sql`
					DELETE FROM error_occurrences
					 WHERE id = ANY(${sql.param(ids)}::uuid[])
					   AND created_at < ${cutoffIso}::timestamptz
					RETURNING id`);
			}); // COMMIT runs under the DELETE's caps
		} catch (e) {
			// Outside the transaction (D18): the batch rolled back, every issue it held released.
			const spent = e instanceof OccurrenceBudgetSpent;
			const code = spent ? null : pgErrorCode(e);
			if (!spent && code !== '55P03' && code !== '57014') throw e; // the deadline, a lost connection: the route's
			heldMs.push(performance.now() - t0);
			if (spent) {
				stopped = 'budget';
				break;
			}
			const reason = occurrenceStopReason(code, phase, deleteCapIsBudget);
			const next = reason === 'delete_timeout' ? nextOccurrenceBatch(batchSize, RETENTION_OCCURRENCE_BATCH_FLOOR) : null;
			if (next !== null) {
				// Retry the same oldest rows, fewer of them: the cursor has not moved. The pause keeps the
				// duty cycle across retries too.
				log.warn('retention.occurrence_batch_reduced', {
					from: batchSize,
					to: next,
					deleted: deletedOccurrences,
					batches: heldMs.length,
				});
				await sleep(Math.min(retentionPauseMs(heldMs[heldMs.length - 1]), Math.max(0, run.left())));
				batchSize = next;
				continue;
			}
			stopped = reason;
			if (reason === 'lock_timeout') {
				log.warn('retention.occurrence_lock_timeout', { deleted: deletedOccurrences, batches: heldMs.length, code, phase });
			} else if (reason === 'delete_timeout') {
				log.warn('retention.occurrence_delete_timeout', {
					deleted: deletedOccurrences,
					batches: heldMs.length,
					heldP95Ms: heldPercentile(heldMs, 0.95),
					batchSize,
				});
			}
			break; // the next run continues from the oldest row
		} finally {
			run.setCaps(null); // back to the run's own bound
		}
		after = { createdAt: candidates[candidates.length - 1].created_at_text, id: candidates[candidates.length - 1].id };
		const held = performance.now() - t0; // every touched issue was held this long
		deletedOccurrences += deleted.length;
		heldMs.push(held);
		await sleep(Math.min(retentionPauseMs(held), Math.max(0, run.left()))); // max(FLOOR, held × (1 − d) / d)
	}
	const heldMaxMs = heldMs.length ? Math.max(...heldMs) : 0;
	if (stopped === 'budget') {
		log.warn('retention.occurrence_budget_exhausted', { deleted: deletedOccurrences, batches: heldMs.length, heldMaxMs });
	}
	const occurrenceStepMs = performance.now() - stepStartReal;
	// The standing keep-up signal, every run: how far behind the cutoff the oldest surviving occurrence is.
	// One descent of idx_error_occurrences_created_at; computed in SQL (no JS parsing of timestamptz text).
	const backlog = await run.db.execute<{ oldest: string | null; backlog_s: number | string }>(sql`
		SELECT min(created_at)::text AS oldest,
		       coalesce(greatest(0, extract(epoch FROM ${cutoffIso}::timestamptz - min(created_at))), 0)::float8 AS backlog_s
		  FROM error_occurrences`);
	const oldestOccurrenceAt = backlog[0]?.oldest ?? null;
	const occurrenceBacklogSeconds = Number(backlog[0]?.backlog_s ?? 0);
	const heldP95Ms = heldPercentile(heldMs, 0.95);
	const overCeiling = heldMs.filter((h) => h > RETENTION_OCCURRENCE_HELD_CEILING_MS).length;
	const cycleMs = heldMs.length ? occurrenceStepMs / heldMs.length : 0;
	if (occurrenceBacklogSeconds > RETENTION_BACKLOG_WARN_SECONDS) {
		log.warn('retention.occurrence_backlog', {
			backlogSeconds: occurrenceBacklogSeconds,
			oldestOccurrenceAt,
			deleted: deletedOccurrences,
			cycleMs,
		});
	}
	if (heldMs.length >= 20 && heldP95Ms > RETENTION_OCCURRENCE_HELD_CEILING_MS) {
		log.warn('retention.occurrence_hold_p95_over_ceiling', { heldP95Ms, batches: heldMs.length, cycleMs });
	}
	if (overCeiling > 0) {
		log.warn('retention.occurrence_hold_over_ceiling', { batchesOverCeiling: overCeiling, heldMaxMs });
	}
	return {
		deletedOccurrences,
		occurrenceBatches: heldMs.length,
		occurrenceStepMs,
		occurrenceBatchSize: batchSize,
		occurrenceHeldMaxMs: heldMaxMs,
		occurrenceHeldP95Ms: heldP95Ms,
		occurrenceBatchesOverCeiling: overCeiling,
		occurrenceStopped: stopped,
		occurrenceBacklogSeconds,
		oldestOccurrenceAt,
	};
}

/**
 * sentinel-issue-tag-facets §3.4 W11 / §5 "Drift report": the drift rows the triggers recorded since the
 * previous run (from ANY writer), reported once as `retention.tag_rollup_drift`, then pruned to the
 * retention window (a row still pending a rebuild is never pruned). One autocommit statement each.
 */
async function reportTagRollupDrift(run: RetentionRun, cutoffDate: Date): Promise<number> {
	const d = run.db;
	const rows = await d.execute<{ issue_id: string; contributions: string | number; source: string }>(sql`
		UPDATE issue_tag_values_drift SET reported_at = now() WHERE reported_at IS NULL
		RETURNING issue_id, contributions, source`);
	if (rows.length > 0) {
		const issueIds = [...new Set(rows.map((r) => r.issue_id))];
		const sources: Record<string, number> = {};
		for (const r of rows) sources[r.source] = (sources[r.source] ?? 0) + 1;
		log.warn('retention.tag_rollup_drift', {
			events: rows.length,
			issues: issueIds.length,
			contributions: rows.reduce((n, r) => n + Number(r.contributions), 0),
			sources,
			issueIds: issueIds.slice(0, 20),
		});
	}
	await d.execute(sql`
		DELETE FROM issue_tag_values_drift
		 WHERE reported_at < ${cutoffDate.toISOString()}::timestamptz AND rebuilt_at IS NOT NULL`);
	return rows.length;
}

interface RebuildStepResult {
	tagRollupRebuildsPending: number;
	tagRollupRebuildsOperatorOnly: number;
	rebuildStepMs: number;
}

/**
 * sentinel-issue-tag-facets §5 "Deferred rebuilds": the LAST step. Rebuilds, each in its own transaction
 * locking only that issue, the drifted issues the delete trigger could not rebuild inline (past its
 * 10,000-occurrence budget). It SIZES BEFORE IT LOCKS (a lock-free probe), SKIPS instead of stopping, and
 * NEVER RETRIES what cannot finish: an issue past `capFull`, whose probe times out, or whose attempt times
 * out is handed to the operator (`operator_only_at`) and never attempted by the route again. The first
 * attempt of a run gets the whole budget, so every run removes at least the oldest issue from the queue.
 */
export async function rebuildPendingTagRollups(run: RetentionRun): Promise<RebuildStepResult> {
	const d = run.db;
	const stepStart = performance.now();
	const queue = await d.execute<{ issue_id: string }>(sql`
		SELECT issue_id FROM issue_tag_values_drift
		 WHERE rebuilt_at IS NULL
		 GROUP BY issue_id
		HAVING count(operator_only_at) = 0
		 ORDER BY min(detected_at), issue_id`);
	const budget = RETENTION_TAG_REBUILD_BUDGET_MS;
	const capFull = tagRebuildCapFull(budget, RETENTION_TAG_REBUILD_FIXED_MS, RETENTION_TAG_REBUILD_US_PER_OCCURRENCE);
	const probeStart = performance.now();
	let deadline = performance.now() + budget; // the budget clock starts after the queue read
	let attempted = false;
	const rebuilt: string[] = [];
	const skipped: string[] = [];
	let rebuiltOccurrences = 0;

	const markOperatorOnly = async (issueId: string, reason: 'too_large' | 'probe_timeout' | 'timeout', atLeast?: number) => {
		await d.execute(sql`
			UPDATE issue_tag_values_drift SET operator_only_at = now()
			 WHERE issue_id = ${issueId}::uuid AND rebuilt_at IS NULL`);
		log.warn('retention.tag_rollup_rebuild_operator_only', {
			issueId,
			reason,
			...(atLeast !== undefined ? { occurrencesAtLeast: atLeast } : {}),
		});
	};

	for (const [i, { issue_id: issueId }] of queue.entries()) {
		if (attempted ? performance.now() >= deadline : i > 0 && performance.now() - probeStart >= RETENTION_TAG_REBUILD_PROBE_BUDGET_MS) {
			break;
		}
		// Probe the size WITHOUT any lock: an index scan of idx_error_occurrences_issue_id to the cap.
		let n: number | null = null;
		try {
			run.setCaps({ statementMs: RETENTION_TAG_REBUILD_PROBE_TIMEOUT_MS, lockMs: Number.POSITIVE_INFINITY });
			const probe = await d.transaction(async (tx) =>
				tx.execute<{ n: number | string }>(sql`
					SELECT count(*)::int AS n FROM (
					  SELECT 1 FROM error_occurrences WHERE issue_id = ${issueId}::uuid LIMIT ${Math.max(0, capFull + 1)}) s`)
			);
			n = Number(probe[0]?.n ?? 0);
		} catch (e) {
			const code = pgErrorCode(e);
			if ((code !== '57014' && code !== '55P03') || run.left() < RETENTION_RUN_MIN_STATEMENT_MS) throw e;
		} finally {
			run.setCaps(null);
		}
		if (n === null) {
			await markOperatorOnly(issueId, 'probe_timeout'); // cannot even be counted to the cap in 1 s
			continue;
		}
		if (n > capFull) {
			await markOperatorOnly(issueId, 'too_large', capFull + 1); // can never fit a run
			continue;
		}
		const est = tagRebuildEstimateMs(n, RETENTION_TAG_REBUILD_FIXED_MS, RETENTION_TAG_REBUILD_US_PER_OCCURRENCE);
		if (attempted && est > deadline - performance.now()) {
			skipped.push(issueId); // stays pending, untouched; a smaller issue further on may still fit
			continue;
		}
		if (!attempted) {
			attempted = true;
			deadline = performance.now() + budget; // the first attempt always gets the whole budget
		}
		try {
			run.setCaps({ statementMs: Math.max(0, deadline - performance.now()), lockMs: Number.POSITIVE_INFINITY });
			await d.transaction(async (tx) => {
				await tx.execute(sql`SELECT issue_tag_values_rebuild(${issueId}::uuid)`);
			});
			rebuilt.push(issueId);
			rebuiltOccurrences += n;
		} catch (e) {
			// D18: caught outside the transaction, which rolled back as a whole.
			const code = pgErrorCode(e);
			if (code !== '57014' && code !== '55P03') throw e;
			// Cut short by the run's deadline rather than its own budget: the UPDATE below is refused before
			// it is sent (RetentionDeadlineError), so the issue stays pending for the next run.
			await markOperatorOnly(issueId, 'timeout');
		} finally {
			run.setCaps(null);
		}
	}

	if (rebuilt.length > 0) {
		log.info('retention.tag_rollup_rebuilt', {
			issues: rebuilt.length,
			issueIds: rebuilt.slice(0, 20),
			occurrences: rebuiltOccurrences,
			ms: Math.round(performance.now() - stepStart),
		});
	}
	const [pending] = await d.execute<{
		pending: number | string;
		operator_only: number | string;
		route_ids: string[] | null;
		operator_ids: string[] | null;
	}>(sql`
		SELECT count(DISTINCT issue_id)::int AS pending,
		       count(DISTINCT issue_id) FILTER (WHERE operator_only_at IS NOT NULL)::int AS operator_only,
		       (array_agg(DISTINCT issue_id::text) FILTER (WHERE operator_only_at IS NULL))[1:20] AS route_ids,
		       (array_agg(DISTINCT issue_id::text) FILTER (WHERE operator_only_at IS NOT NULL))[1:20] AS operator_ids
		  FROM issue_tag_values_drift WHERE rebuilt_at IS NULL`);
	const tagRollupRebuildsPending = Number(pending?.pending ?? 0);
	const tagRollupRebuildsOperatorOnly = Number(pending?.operator_only ?? 0);
	if (tagRollupRebuildsPending > 0) {
		const operatorIds = pending?.operator_ids ?? [];
		log.warn('retention.tag_rollup_rebuild_pending', {
			issueIds: (pending?.route_ids ?? []).filter((id) => !operatorIds.includes(id)),
			operatorOnlyIssueIds: operatorIds,
			skipped: skipped.length,
		});
	}
	return { tagRollupRebuildsPending, tagRollupRebuildsOperatorOnly, rebuildStepMs: performance.now() - stepStart };
}

// sentinel-affected-users §4.5: one side table of the affected users/tenants rollup, as the sweep
// sees it -- the table whose aged rows go, and the `issues` column that is its cardinality
// (decision 12). Two of them; the SQL is identical, only the names differ.
interface AffectedSideTable {
	table: PgTable;
	// `sql.identifier(countColumn.name)` is what the UPDATE's SET uses: `${issues.affectedUserCount}`
	// would render the qualified `"issues"."affected_user_count"`, which Postgres rejects on the
	// left of a SET.
	countColumn: PgColumn;
}

const AFFECTED_USERS_SIDE_TABLE: AffectedSideTable = {
	table: issueAffectedUsers,
	countColumn: issues.affectedUserCount,
};
const AFFECTED_TENANTS_SIDE_TABLE: AffectedSideTable = {
	table: issueAffectedTenants,
	countColumn: issues.affectedTenantCount,
};

/**
 * sentinel-affected-users §4.5 (decisions 12, 23, 28): the retention sweep's step for ONE side
 * table -- three statements in ONE transaction, in this order and no other:
 *
 *   1. `SELECT id FROM issues WHERE id IN (aged side rows' issue_ids) ORDER BY id FOR UPDATE` --
 *      the touched `issues` rows are locked FIRST, in id order. This is the same lock order
 *      `StoreEvent` takes (apps/processor-go/store/store.go: the issues upsert, then the side
 *      table), so an ingest for a touched issue queues on the row lock and proceeds when this
 *      commits; neither transaction can hold a side-table lock the other is waiting for, so `40P01`
 *      is impossible by construction. The first draft locked the side rows first and would have
 *      deadlocked against ingest once an hour under load.
 *   2. `DELETE … WHERE last_seen < cutoff AND issue_id = ANY(locked ids) RETURNING issue_id` --
 *      decision 28: scoped to the set statement 1 locked. An aged row on an issue statement 1 did
 *      not see (an ingest that committed between the two statements with a client `created_at`
 *      older than the cutoff) survives one more hour and is swept by the next run, which locks its
 *      issue first. Its RETURNING length is the result field.
 *   3. `UPDATE issues SET <count> = (SELECT count(*) FROM <side table> …) WHERE id = ANY(locked
 *      ids)` -- the recompute, over the locked set only (an untouched issue's count is not
 *      rewritten). Windowed counts (decision 12): this is where a count goes DOWN.
 *
 * Three statements and NOT one data-modifying CTE, on purpose: a CTE and the query that references
 * it run on the same snapshot ("cannot see one another's effects on the target tables"), so a
 * `count(*)` in the UPDATE half of one statement would count the side rows BEFORE the DELETE half
 * removed them and the recompute would be a no-op after every sweep. Separate statements in one
 * transaction see each other.
 *
 * A transaction -- unlike every other step of `cleanupRetainedData`, which are autocommit
 * statements -- because a row lock lasts exactly as long as its transaction, and because failure
 * must be atomic (flow R4): if any statement throws, Drizzle rolls the transaction back and
 * neither rows nor counts for this side table have moved; the route logs and returns non-2xx and
 * the next hourly run does the same work. There is no window in which rows are gone and the count
 * is stale. D18: nothing in here `return`s early to signal a failure -- an error propagates as a
 * throw so the rollback actually happens. With no aged rows at all the three statements still run
 * (an empty locked set locks, deletes and recomputes nothing) rather than short-circuiting, so the
 * callback has exactly one exit and it is the commit.
 *
 * Returns the number of side rows deleted.
 */
async function sweepAffectedSideTable(
	exec: Exec,
	{ table, countColumn }: AffectedSideTable,
	cutoffDate: Date
): Promise<number> {
	// `::timestamptz`, not `::timestamp`: `last_seen` is TIMESTAMP WITH TIME ZONE (§4.6), and a
	// `::timestamp` cast would drop the ISO literal's `Z` and re-read it in the session's TimeZone,
	// sliding the window by the server's UTC offset -- the same trap triage.ts's window query names.
	const cutoffIso = cutoffDate.toISOString();

	return await exec.transaction(async (tx) => {
		// 1. Lock the touched issues rows first, in id order (decision 23).
		const locked = await tx.execute<{ id: string }>(sql`
			SELECT ${issues.id} AS id
			FROM ${issues}
			WHERE ${issues.id} IN (
				SELECT DISTINCT issue_id FROM ${table} WHERE last_seen < ${cutoffIso}::timestamptz
			)
			ORDER BY ${issues.id}
			FOR UPDATE
		`);
		// `sql.param`, not a bare `${lockedIds}`: Drizzle expands a bare JS array into a `(a, b, c)`
		// tuple, which Postgres cannot cast to `uuid[]`; a Param goes through as ONE `$n` bound to
		// the whole array (postgres-js encodes it as a PG array), which is what `= ANY($2)` needs.
		const lockedIds = sql.param(locked.map((row) => row.id));

		// 2. Delete the aged side rows, scoped to the locked set (decision 28); the returned row
		//    count IS the result field.
		const deleted = await tx.execute(sql`
			DELETE FROM ${table}
			WHERE last_seen < ${cutoffIso}::timestamptz
			  AND issue_id = ANY(${lockedIds}::uuid[])
			RETURNING issue_id
		`);

		// 3. Recompute every touched issue from what remains -- a separate statement, so it sees
		//    statement 2's effect.
		await tx.execute(sql`
			UPDATE ${issues}
			   SET ${sql.identifier(countColumn.name)} = (
			       SELECT count(*) FROM ${table} a WHERE a.issue_id = ${issues.id}
			   )
			 WHERE ${issues.id} = ANY(${lockedIds}::uuid[])
		`);

		return deleted.length;
	});
}

/**
 * R6 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): best-effort deletes a batch of MinIO objects,
 * logging (not throwing) on individual failures -- mirrors `deleteComment`'s ordering rationale
 * (queries/comments.ts): the DB rows are already gone by the time this runs, so a storage failure
 * here just leaves an invisible orphaned object, not a dangling DB reference.
 */
// R11 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): exported so the report delete route
// (routes/api/organizations/[orgId]/reports/[issueId]/+server.ts) reuses this exact helper
// instead of growing a second copy of the same best-effort-delete-and-log pattern.
// sentinel-sourcemaps §4.1: returns the keys whose delete FAILED (still logged, still swallowed)
// so the sourcemap upload route can count each one under `uploads_total{outcome="storage_error"}`
// -- a failed `replaced` delete is one more permanent orphan, and §4.1 says every one is counted.
// Every earlier caller ignores the return value, so this is additive.
// sentinel-issue-tag-facets §5 rule 3: `signal` is the retention run's deadline. Each delete passes it
// to the S3 client, and the loop stops before its next object once it has aborted: the objects left are
// invisible orphans, as a failed delete's already are, counted in
// `retention.attachment_storage_delete_skipped`.
export async function bestEffortDeleteObjects(
	storageKeys: string[],
	context: string,
	signal?: AbortSignal
): Promise<string[]> {
	if (storageKeys.length === 0 || !isStorageConfigured()) {
		return [];
	}
	const failed: string[] = [];
	for (const [i, key] of storageKeys.entries()) {
		if (signal?.aborted) {
			log.warn('retention.attachment_storage_delete_skipped', { context, skipped: storageKeys.length - i });
			break;
		}
		try {
			await (signal ? deleteObject(key, signal) : deleteObject(key));
		} catch (err) {
			log.error('retention.attachment_storage_delete_failed', { context, key, error: err });
			failed.push(key);
		}
	}
	return failed;
}

/**
 * The retention sweep. `run` is the route's retention run (sentinel-issue-tag-facets §5 "One run, one
 * connection, one deadline"): every statement and step below goes through `run.db`, the run's one
 * connection, under timeouts from its deadline. Called without one (the integration suites do), it opens a
 * run of its own on a fresh per-run client, with the route's deadline and no advisory lock, and ends it
 * before returning.
 */
export async function cleanupRetainedData(
	retentionDays: number = 30,
	manualRetentionDays: number = 365,
	tombstoneRetentionDays: number = 30,
	run?: RetentionRun,
	deps: RetentionDeps = {}
): Promise<RetentionResult> {
	if (run) return await sweep(run, retentionDays, manualRetentionDays, tombstoneRetentionDays, deps);
	let lost = false;
	const client = openRetentionRunClient(() => {
		lost = true;
	});
	try {
		const own = openRetentionRun(client as never, { deadlineMs: RETENTION_ROUTE_DEADLINE_MS, isLost: () => lost });
		return await sweep(own, retentionDays, manualRetentionDays, tombstoneRetentionDays, deps);
	} finally {
		await client.end({ timeout: 1 }).catch(() => {});
	}
}

async function sweep(
	run: RetentionRun,
	retentionDays: number,
	manualRetentionDays: number,
	tombstoneRetentionDays: number,
	deps: RetentionDeps
): Promise<RetentionResult> {
	const d = run.db;
	const cutoffDate = new Date();
	cutoffDate.setDate(cutoffDate.getDate() - retentionDays);

	// A04: manual issues (issueType='user_report') never accumulate errorOccurrences rows by
	// construction (they are reported directly, not ingested), so they are ALWAYS "occurrence-less"
	// -- gating their deletion on `retentionDays` (default 30) would delete a legitimate,
	// still-relevant manual issue purely because of its type. `manualCutoff` is a second, longer
	// cutoff (default 365d) that applies only to them.
	const manualCutoff = new Date();
	manualCutoff.setDate(manualCutoff.getDate() - manualRetentionDays);

	// sentinel-issue-tag-facets §5: the occurrence delete, at the same position as the single autocommit
	// DELETE it replaces (before the side-table sweeps, so every later step sees the same state): paced,
	// issue-locked batches under a wall-clock budget. Then the drift report, before the side tables.
	const occurrenceStep = await deleteAgedOccurrences(run, cutoffDate, deps);
	const deletedOccurrences = occurrenceStep.deletedOccurrences;
	const tagRollupDriftEvents = await reportTagRollupDrift(run, cutoffDate);

	// sentinel-affected-users §4.5: the side-table step sits HERE -- after the occurrence delete
	// (so it uses the same `cutoffDate`) and BEFORE the orphan-issue delete below, so an issue whose
	// occurrences all aged out is still present for the count recompute and is then removed by the
	// existing orphan rule with its side rows cascading (flow R2; `ON DELETE CASCADE`, §4.6 -- no
	// statement here). One transaction per side table; see `sweepAffectedSideTable`.
	const deletedAffectedUsers = await sweepAffectedSideTable(d, AFFECTED_USERS_SIDE_TABLE, cutoffDate);
	const deletedAffectedTenants = await sweepAffectedSideTable(
		d,
		AFFECTED_TENANTS_SIDE_TABLE,
		cutoffDate
	);

	// sentinel-issue-trend §4.5 (flows R1, R3; decision 4): the hourly bucket rollup's aged rows go
	// HERE -- immediately after the two side-table sweeps and before the orphan-issue delete, for
	// readability with the other rollup step. Unlike that step it is ONE autocommit statement, in no
	// transaction and taking no `issues` lock, because there is no count on `issues` to recompute
	// from what remains: the delete either lands or throws (the route logs and returns non-2xx, and
	// the next hourly run does the same work), and there is no invariant between this table and any
	// other row, so there is no stale window. No `issues` lock also means no cycle with
	// `StoreEvent`'s bucket upsert is possible (§4.3); an ingest with an old event clock that races
	// this delete commits its row and is removed by the next run, as its occurrence row is (R4).
	//
	// `bucket_start < cutoff`, the same `cutoffDate` the occurrence delete used: a bucket whose hour
	// starts before the cutoff holds occurrences that are all (or, for the ONE straddling bucket,
	// partly) gone by now. The straddling bucket is removed up to an hour early -- at most 1 of 720
	// hours, and outside the 30-day window at DATA_RETENTION_DAYS >= 30. `::timestamptz`, never
	// `::timestamp`, for the reason `sweepAffectedSideTable` gives. An orphaned issue's buckets are
	// NOT this statement's concern: they go with the issue through `ON DELETE CASCADE` (R2, §4.6).
	const deletedOccurrenceBucketsResult = await d.execute<{ issue_id: string }>(sql`
		DELETE FROM ${issueOccurrenceBuckets}
		WHERE ${issueOccurrenceBuckets.bucketStart} < ${cutoffDate.toISOString()}::timestamptz
		RETURNING issue_id
	`);
	const deletedOccurrenceBuckets = deletedOccurrenceBucketsResult.length;

	// DEFECT FIX: this delete previously had no age check at all — it removed ANY zero-occurrence
	// issue immediately, including one created moments ago whose first (and only) occurrence had
	// just been deleted above. An issue with no occurrences yet still has to survive until it is
	// itself old enough to be outside the retention window.
	//
	// `first_seen` (not `last_seen`) is the right column: per packages/db-migrations, it defaults to
	// now() at INSERT and is never updated again (processor-go's upsert in store.go only bumps
	// last_seen via GREATEST on conflict) — so it is exactly "when this issue was created" and keeps
	// that meaning even after its occurrences are gone. `last_seen` would also default to the
	// creation time for a brand-new issue, but conceptually it tracks the most recent occurrence,
	// which is the wrong signal to gate deletion of a row precisely because it has none.
	const noOccurrencesCondition = sql`${issues.id} NOT IN (
		SELECT DISTINCT ${errorOccurrences.issueId}
		FROM ${errorOccurrences}
	)`;

	// System-error (ingested) issues: unchanged behavior, gated on the short `retentionDays` cutoff.
	const systemErrorOrphanCondition = and(
		noOccurrencesCondition,
		sql`${issues.issueType} <> 'user_report'`,
		lt(issues.firstSeen, cutoffDate)
	);

	// A04: manual issues additionally require status IN ('resolved','ignored') -- never delete an
	// 'unresolved' one, however old -- AND assigned_to IS NULL -- never delete a claimed one, agent
	// or human, mid-triage -- AND the longer `manualCutoff`.
	//
	// sentinel-user-report-issue §6: this condition is COMPANION-AGNOSTIC and must stay that way.
	// It keys on `issues.issue_type`, not on the presence of a `manual_issue_reports` row, so an
	// externally-sourced report -- which is an `issue_type = 'user_report'` row with an
	// `external_issue_reports` companion instead -- is aged out by exactly this branch, and its
	// companion goes with it through that table's `ON DELETE CASCADE`. Narrowing this to the
	// manual companion (a join, an EXISTS) would make every external report immortal.
	const manualIssueOrphanCondition = and(
		noOccurrencesCondition,
		eq(issues.issueType, 'user_report'),
		or(eq(issues.status, 'resolved'), eq(issues.status, 'ignored')),
		isNull(issues.assignedTo),
		lt(issues.firstSeen, manualCutoff)
	);

	const orphanCondition = or(systemErrorOrphanCondition, manualIssueOrphanCondition);

	// R6: this delete cascades away any attachments rows tied to a doomed issue (directly, via
	// attachments.issue_id) or to one of its comments (via attachments.comment_id ->
	// issue_comments.issue_id) -- MinIO has no transactional participation in that cascade, so the
	// storage_keys are collected BEFORE the delete, mirroring `deleteComment`
	// (queries/comments.ts). This candidate set is a snapshot: a concurrent occurrence insert
	// between this SELECT and the DELETE below could in principle change who is "orphaned", but
	// the DELETE's own WHERE re-evaluates the same condition, so at worst this collects a
	// storage_key for a row the DELETE then doesn't actually remove -- a harmless no-op object
	// delete, never a false negative that leaves an object behind.
	// N8: the same candidate SELECT also captures the detail a tombstone needs -- organization_id
	// (via the projects join, since the issues row is gone the moment we delete it and cannot be
	// joined afterward), the display message/type, and the claim snapshot so a claim-holding agent
	// can still find the deletion via `?claimed=me`. Keyed by id so we tombstone ONLY the issues the
	// DELETE actually removed (its WHERE re-evaluates orphanCondition; a concurrent occurrence insert
	// could spare a candidate).
	const candidateRows = await d
		.select({
			id: issues.id,
			projectId: issues.projectId,
			organizationId: projects.organizationId,
			message: issues.message,
			issueType: issues.issueType,
			assigneeType: issues.assigneeType,
			assignedTo: issues.assignedTo,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(orphanCondition);
	const candidateIds = candidateRows.map((row) => row.id);
	const candidateById = new Map(candidateRows.map((row) => [row.id, row]));

	let doomedStorageKeys: string[] = [];
	if (candidateIds.length > 0) {
		const commentRows = await d
			.select({ id: issueComments.id })
			.from(issueComments)
			.where(inArray(issueComments.issueId, candidateIds));
		const commentIds = commentRows.map((row) => row.id);

		const attachmentConditions = [inArray(attachments.issueId, candidateIds)];
		if (commentIds.length > 0) {
			attachmentConditions.push(inArray(attachments.commentId, commentIds));
		}

		const attachmentRows = await d
			.select({ storageKey: attachments.storageKey })
			.from(attachments)
			.where(
				attachmentConditions.length > 1 ? or(...attachmentConditions) : attachmentConditions[0]
			);
		doomedStorageKeys = attachmentRows.map((row) => row.storageKey);
	}

	const orphanedIssuesResult = await d.delete(issues).where(orphanCondition).returning({ id: issues.id });

	await bestEffortDeleteObjects(doomedStorageKeys, 'retention_orphaned_issues', run.signal);

	const deletedOrphanedIssues = orphanedIssuesResult.length;

	// N8: one tombstone per issue this run actually deleted, built from the pre-delete snapshot. The
	// insert follows the DELETE (the FK-free tombstone has no ordering dependency on the issue row)
	// and is scoped to `orphanedIssuesResult` so a candidate the DELETE spared is not tombstoned.
	let tombstonesWritten = 0;
	const tombstoneValues = orphanedIssuesResult
		.map((row) => candidateById.get(row.id))
		// `projects.organization_id` is nullable in the schema; an org-scoped feed row is
		// meaningless without one, so a (in practice non-existent) orgless project's issue is simply
		// not tombstoned rather than written with a null org.
		.filter(
			(detail): detail is NonNullable<typeof detail> & { organizationId: string } =>
				detail !== undefined && detail.organizationId !== null
		)
		.map((detail) => ({
			issueId: detail.id,
			organizationId: detail.organizationId,
			projectId: detail.projectId,
			issueMessage: detail.message,
			issueType: detail.issueType,
			assigneeType: detail.assigneeType,
			assignedTo: detail.assignedTo,
			reason: 'retention',
		}));
	if (tombstoneValues.length > 0) {
		await d.insert(issueTombstones).values(tombstoneValues);
		tombstonesWritten = tombstoneValues.length;
	}

	// N8: bound the tombstone table itself -- prune tombstones older than `tombstoneRetentionDays`
	// (default 30). A consumer that has been offline longer than that loses the deletion signal, the
	// same forward-looking-window trade the events feed already makes (agents bootstrap current
	// state via GET /api/agent/issues, not by replaying unbounded history).
	const tombstoneCutoff = new Date();
	tombstoneCutoff.setDate(tombstoneCutoff.getDate() - tombstoneRetentionDays);
	const deletedTombstonesResult = await d
		.delete(issueTombstones)
		.where(lt(issueTombstones.deletedAt, tombstoneCutoff))
		.returning({ id: issueTombstones.id });
	const deletedTombstones = deletedTombstonesResult.length;

	// sentinel-user-report-issue §6 / §4.6: reap external reporters that no longer have a report.
	// `external_reporters` is referenced by `external_issue_reports.external_reporter_id` with no
	// ON DELETE behaviour back from `issues`, so deleting a report above leaves the reporter row
	// standing. Both conditions matter: no remaining report (a reporter with one live report must
	// survive even if it has not been seen for a year) AND `last_seen_at` older than the same long
	// `manualCutoff` the reports themselves use (a reporter that filed and had its report deleted
	// yesterday is still a repeat reporter for §4.2's "first report from this person" signal).
	const deletedExternalReportersResult = await d
		.delete(externalReporters)
		.where(
			and(
				lt(externalReporters.lastSeenAt, manualCutoff),
				sql`NOT EXISTS (
					SELECT 1 FROM ${externalIssueReports}
					WHERE ${externalIssueReports.externalReporterId} = ${externalReporters.id}
				)`
			)
		)
		.returning({ id: externalReporters.id });
	const deletedExternalReporters = deletedExternalReportersResult.length;

	// sentinel-releases-snooze §4.3 "Hourly" (decisions 8, 15, 27, 30, 41, 42): the `next` catch-up,
	// LAST, in its own transaction, once across all projects. See `catchUpNextReleases`.
	await catchUpNextReleases(d);

	// sentinel-issue-tag-facets §5 "Deferred rebuilds": LAST, after the orphan sweep and the catch-up.
	const rebuildStep = await rebuildPendingTagRollups(run);

	return {
		deletedOrphanedIssues,
		retentionDays,
		cutoffDate,
		manualRetentionDays,
		tombstonesWritten,
		deletedTombstones,
		tombstoneRetentionDays,
		deletedExternalReporters,
		deletedAffectedUsers,
		deletedAffectedTenants,
		deletedOccurrenceBuckets,
		...occurrenceStep,
		tagRollupDriftEvents,
		...rebuildStep,
	};
}

/**
 * sentinel-releases-snooze §4.3 "Hourly" -- the `next` catch-up, the post-commit fan-out's rule from
 * the other end (decisions 15, 30). The processor rewrites `resolved_in_version = 'next'` to a
 * release's version the moment that release is first seen, best-effort and outside its ingest
 * transaction; when that rewrite fails or the pod dies inside its window, the `next` issues keep
 * reading `next`. This statement rewrites them to the row the fan-out would have written: for every
 * `resolved` issue waiting on `next`, the EARLIEST release of its project whose `first_seen` is
 * strictly after its `resolved_at` (ties on `first_seen` broken by `version`, so the statement is
 * deterministic). One statement, once across all projects, `DISTINCT ON (n.id)` ordered by
 * `first_seen ASC, version ASC` picking that earliest release per issue. Verbatim from §4.3.
 *
 * What it leaves alone, by construction: a `next` issue whose only releases are older than its
 * `resolved_at` (the join finds nothing); a `next` issue with `resolved_at IS NULL` (decision 27 --
 * `r2.first_seen > NULL` is not true, so it joins nothing); every issue that is not `resolved` +
 * `next` (the outer WHERE, re-checked on the UPDATE's own WHERE so a row that stopped waiting between
 * the subquery and the write is not rewritten). Project-wide across environments (decision 41): the
 * version written may have been first seen in an environment the issue's readers cannot see, and
 * that is the contract's stated cost, not this statement's to fix. Driven by
 * `idx_issues_project_resolved_next` -- the partial index covers `resolved_in_version = 'next'` for
 * every project, so the outer scan is the handful of waiting issues, not `issues`.
 *
 * NO `issue_activity` row (decision 8, extended by decision 30): the resolution did not change, its
 * version became known -- exactly what the fan-out writes, and the fan-out writes no row either.
 *
 * LAST in `cleanupRetainedData`, in its OWN transaction -- not inside any side-table step's -- and
 * its failure does not fail the sweep. Lock order: it takes `issues` row locks only and reads
 * `releases` without one. Against an ingest -- which holds its `issues` row FIRST and wants the
 * `releases` row AFTER it -- there is no cycle. Against the sweep's own side-table steps
 * (`sweepAffectedSideTable`'s `SELECT id … ORDER BY id FOR UPDATE`) it holds nothing those steps
 * wait on, because it runs after them in a transaction of its own; their lock-first protocol is
 * not extended to it and does not need to be. What CAN deadlock is `batchUpdateIssues` over a set
 * containing the same `next` issues in the same second (decision 42): Postgres kills one side with
 * `40P01`; if it is this one, the SQLSTATE goes on the log line and the counter moves, and the next
 * hourly run redoes the work by construction (idempotent -- a rewritten row no longer matches).
 *
 * The failure path is the whole reason this is its own function with a try/catch rather than one
 * more statement in the caller: the sweep's deletes and recomputes are already committed by the
 * time this runs (every earlier step is autocommit or its own transaction), so throwing here would
 * turn a fully successful sweep into a non-2xx route response over a statement whose failure costs
 * exactly one hour. Logged as `retention_cron.next_catchup_failed` with the SQLSTATE
 * (`postgres-js` surfaces it as `code`), and counted on `sentinel_release_next_rewrite_failures_total`
 * in this process's registry -- the SAME name the processor's fan-out counter carries, no label,
 * summed by name at the scrape (§12). D18 does not bite: the transaction body has one statement and
 * exactly one exit, its commit; a throw propagates out of `db.transaction` (which rolls back) and is
 * caught here, outside it.
 *
 * Returns the number of issues rewritten -- for the log line and for tests; nothing on
 * `RetentionResult` carries it, because the contract adds no field there (the sweep's other results
 * are unchanged by this statement's presence).
 */
async function catchUpNextReleases(exec: Exec): Promise<number> {
	try {
		const rewritten = await exec.transaction(async (tx) => {
			const result = await tx.execute<{ id: string }>(sql`
				UPDATE ${issues} i SET resolved_in_version = r.version
				  FROM (SELECT DISTINCT ON (n.id) n.id, r2.version
				          FROM ${issues} n
				          JOIN ${releases} r2 ON r2.project_id = n.project_id AND r2.first_seen > n.resolved_at
				         WHERE n.status = 'resolved' AND n.resolved_in_version = 'next'
				         ORDER BY n.id, r2.first_seen ASC, r2.version ASC) r
				 WHERE i.id = r.id AND i.status = 'resolved' AND i.resolved_in_version = 'next'
				RETURNING i.id
			`);
			return result.length;
		});
		log.info('retention_cron.next_catchup', { rewritten });
		return rewritten;
	} catch (err) {
		// The run ending (its deadline, its connection) is the route's to classify, not this step's.
		if (err instanceof RetentionDeadlineError || err instanceof RetentionConnectionLost) throw err;
		const sqlstate = (err as { code?: unknown } | null)?.code;
		log.error('retention_cron.next_catchup_failed', {
			sqlstate: typeof sqlstate === 'string' ? sqlstate : null,
			error: err,
		});
		recordReleaseNextRewriteFailure();
		return 0;
	}
}

/**
 * N9 (D21): ages out client-supplied idempotency keys older than
 * `AGENT_IDEMPOTENCY_RETENTION_DAYS` (7). Past that window a repeat key is no longer deduplicated --
 * a retry that late is a fresh logical request, not a resend of a dropped response -- so the row's
 * only remaining purpose is table bloat. Same piggyback pattern as reapStaleClaims: driven by the
 * existing retention cron rather than a second scheduled route. Returns the count deleted.
 */
export async function reapExpiredIdempotencyKeys(
	exec: Exec = db,
	retentionDays: number = AGENT_IDEMPOTENCY_RETENTION_DAYS
): Promise<number> {
	const cutoff = new Date();
	cutoff.setDate(cutoff.getDate() - retentionDays);

	const deleted = await exec
		.delete(agentIdempotencyKeys)
		.where(lt(agentIdempotencyKeys.createdAt, cutoff))
		.returning({ id: agentIdempotencyKeys.id });

	return deleted.length;
}

/**
 * sentinel-user-report-issue §8: the same sweep for `report_idempotency_keys`, whose window is
 * also 7 days ("matching the existing reaper's cutoff"). A separate function rather than a
 * parameter on the one above because the two tables are two different key spaces -- the agent's
 * and the calling API key row's -- and collapsing them would make one retention knob mean two
 * things. `idx_report_idempotency_keys_created_at` keeps this off a full scan.
 *
 * Past the window a stored key no longer deduplicates: `POST /api/report` deletes an expired row
 * itself when a repeat arrives (see `deleteExpiredReportIdempotency`), so this sweep is about
 * table bloat, not about correctness. Returns the count deleted.
 */
export async function reapExpiredReportIdempotencyKeys(
	exec: Exec = db,
	retentionDays: number = REPORT_IDEMPOTENCY_RETENTION_DAYS
): Promise<number> {
	const cutoff = new Date();
	cutoff.setDate(cutoff.getDate() - retentionDays);

	const deleted = await exec
		.delete(reportIdempotencyKeys)
		.where(lt(reportIdempotencyKeys.createdAt, cutoff))
		.returning({ id: reportIdempotencyKeys.id });

	return deleted.length;
}

// N7c (A03): result of a single stale-claim reap pass.
export interface ReapStaleClaimsResult {
	releasedClaims: number;
	staleHours: number;
}

/**
 * A03: force-releases agent claims an unattended loop abandoned. Only claims with
 * assigneeType='agent' on unresolved issues are eligible -- resolved/ignored issues retain
 * their claimant as an immutable historical record, and human claims are never auto-released.
 * A claim is stale-eligible when `claimedAt` is either NULL (pre-migration claim, or somehow
 * never set) or older than `staleHours` (default 24, env CLAIM_STALE_HOURS). Even a stale-eligible
 * claim is still protected if the claimant has written an issue_activity row on that issue within
 * the same window -- an agent making visible progress should not be reaped just because it claimed
 * a while ago.
 *
 * Per-candidate: read (select stale-eligible issues) -> check recent activity -> conditional
 * UPDATE re-scoped to the same claimant, so a claim that changed hands between the select and the
 * update (release + reclaim by someone else) is never touched (0 rows updated -> skipped, not an
 * error -- this is a best-effort sweep, not a caller-facing conditional mutation like claimIssue).
 * D18: every skip is a `continue` over a candidate, never an early `return` out of the
 * transaction -- the loop's own writes so far always land as part of one commit.
 */
export async function reapStaleClaims(staleHours: number = 24, exec: Exec = db): Promise<ReapStaleClaimsResult> {
	const cutoff = new Date();
	cutoff.setHours(cutoff.getHours() - staleHours);

	return await exec.transaction(async (tx) => {
		const candidates = await tx
			.select({ id: issues.id, assignedTo: issues.assignedTo })
			.from(issues)
			.where(
				and(
					eq(issues.assigneeType, 'agent'),
					eq(issues.status, 'unresolved'),
					sql`${issues.assignedTo} IS NOT NULL`,
					or(isNull(issues.claimedAt), lt(issues.claimedAt, cutoff))
				)
			);

		let releasedClaims = 0;

		for (const candidate of candidates) {
			if (!candidate.assignedTo) continue;

			const recentActivity = await tx
				.select({ id: issueActivity.id })
				.from(issueActivity)
				.where(
					and(
						eq(issueActivity.issueId, candidate.id),
						eq(issueActivity.actorId, candidate.assignedTo),
						gte(issueActivity.createdAt, cutoff)
					)
				)
				.limit(1);

			if (recentActivity.length > 0) continue;

			const updated = await tx
				.update(issues)
				.set({ assigneeType: null, assignedTo: null, claimedAt: null })
				.where(
					and(
						eq(issues.id, candidate.id),
						eq(issues.assignedTo, candidate.assignedTo),
						eq(issues.assigneeType, 'agent'),
						eq(issues.status, 'unresolved')
					)
				)
				.returning({ id: issues.id });

			if (updated.length === 0) continue;

			await tx.insert(issueActivity).values({
				issueId: candidate.id,
				eventType: 'claim_released',
				actorType: 'system',
				actorId: 'sentinel-claim-reaper',
				newValue: { previousAssignee: candidate.assignedTo, reason: 'stale' },
			});

			releasedClaims++;
		}

		return { releasedClaims, staleHours };
	});
}
