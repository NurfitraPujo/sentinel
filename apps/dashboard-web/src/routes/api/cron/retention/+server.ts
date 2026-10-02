import type { RequestHandler } from '@sveltejs/kit';
import { json } from '@sveltejs/kit';
import {
	cleanupRetainedData,
	reapStaleClaims,
	reapExpiredIdempotencyKeys,
	reapExpiredReportIdempotencyKeys,
	maintainSpans,
} from '$lib/server/retention';
import { reapAllExpiredInvitations } from '$lib/db/queries/organizations';
import { reapAllOrphanAttachments } from '$lib/server/attachment-reaper';
import { env } from '$env/dynamic/private';
import { log } from '$lib/server/observability/log';
import { getRetentionDays } from '$lib/server/retention-window';
import { openRetentionRunClient } from '$lib/server/db';
import {
	openRetentionRun,
	isConnectionLossError,
	RetentionDeadlineError,
	type RetentionRun,
} from '$lib/server/retention-run';
import {
	RETENTION_ROUTE_DEADLINE_MS,
	RETENTION_ROUTE_LOCK_STALE_MS,
	RETENTION_RUN_MIN_STATEMENT_MS,
	retentionRunOutcome,
} from '$lib/server/retention-batch';
import { recordRetentionRun } from '$lib/server/metrics';

const CRON_SECRET_HEADER = 'x-cron-secret';

export const POST: RequestHandler = async ({ request }) => {
	const cronSecret = request.headers.get(CRON_SECRET_HEADER);

	if (!cronSecret) {
		return json({ error: 'Missing cron secret' }, { status: 401 });
	}

	const expectedSecret = env.CRON_SECRET;

	if (!expectedSecret) {
		log.error('retention_cron.not_configured', { note: 'CRON_SECRET environment variable is not set' });
		return json({ error: 'Cron endpoint not configured' }, { status: 500 });
	}

	if (cronSecret !== expectedSecret) {
		return json({ error: 'Invalid cron secret' }, { status: 401 });
	}

	// sentinel-affected-users decision 21: the ONE reading of DATA_RETENTION_DAYS, shared with the
	// `/` and `/issues/{id}` loaders so the window on every count label is the window this sweep
	// enforces.
	const retentionDays = getRetentionDays();
	const manualRetentionDays = parseInt(env.MANUAL_ISSUE_RETENTION_DAYS ?? '365', 10);
	const tombstoneRetentionDays = parseInt(env.TOMBSTONE_RETENTION_DAYS ?? '30', 10);
	const claimStaleHours = parseInt(env.CLAIM_STALE_HOURS ?? '24', 10);

	log.info('retention_cron.started', {
		retentionDays,
		manualRetentionDays,
		tombstoneRetentionDays,
		claimStaleHours,
	});

	// sentinel-issue-tag-facets §5 "One run, one connection, one deadline": one instance at a time, and each
	// run does ALL of its work on the one connection that holds its lock, every statement under timeouts
	// from a deadline counted from the moment the lock is granted. So a live run always ends by its
	// deadline and its lock ends with it; a holder older than the stale bound can only be a dead or
	// wedged process (or a session that is not a run), which fails the Job every hour until it goes.
	let lost = false;
	const client = openRetentionRunClient(() => {
		lost = true;
	});
	let granted = false;
	let run: RetentionRun | null = null;
	try {
		const [{ locked }] = (await client`SELECT pg_try_advisory_lock(hashtext('sentinel.retention.route')) AS locked`) as unknown as { locked: boolean }[];
		if (!locked) {
			// The holder is aged by its connection's start: a run opens its connection for that run, takes
			// the lock first and closes it when the run ends, so that age is the run's age.
			const [holder] = (await client`
				SELECT l.pid, a.state, a.application_name,
				       (extract(epoch FROM clock_timestamp() - a.backend_start) * 1000)::float8 AS held_ms
				  FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
				 WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1
				   AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
				   AND ((l.classid::bigint << 32) | l.objid::bigint) = hashtext('sentinel.retention.route')::bigint
				 LIMIT 1`) as unknown as { pid: number; state: string; application_name: string; held_ms: number }[];
			if (!holder || holder.held_ms <= RETENTION_ROUTE_LOCK_STALE_MS) {
				log.info('retention_cron.skipped_concurrent', { holderPid: holder?.pid ?? null, heldMs: holder?.held_ms ?? null });
				recordRetentionRun('skipped');
				return json({ success: true, skipped: 'already_running' });
			}
			log.error('retention_cron.lock_stale', {
				holderPid: holder.pid,
				heldMs: holder.held_ms,
				state: holder.state,
				holderApp: holder.application_name,
			});
			recordRetentionRun('lock_stale');
			return json({ error: 'retention_lock_stale', holderPid: holder.pid, heldMs: holder.held_ms }, { status: 503 });
		}
		granted = true;
		run = openRetentionRun(client as never, { deadlineMs: RETENTION_ROUTE_DEADLINE_MS, isLost: () => lost });
		const routeStart = performance.now();

		// sentinel-span-ingest D1: create the day partitions spans are written into and drop the ones past the 14-day
		// window. It runs FIRST, before the occurrence sweep and the reapers, so a throw or a deadline in any of them
		// cannot stop partition upkeep (a missing partition drops spans; a missed drop grows the table). It has its own
		// try/catch inside maintainSpans (a failure is logged and reported in the result, never fatal to the run) and
		// it is held to a 3 s lock / 30 s statement cap (SPAN_MAINT_CAPS) so a reader on `spans` cannot spend the run.
		const spanMaintenance = await maintainSpans(run.db, undefined, run);

		const result = await cleanupRetainedData(retentionDays, manualRetentionDays, tombstoneRetentionDays, run);

		// D42: sweep expired pending invitations across every organization. The per-org reaper only
		// fires when that org issues another invitation, so an org that stops inviting never reaps.
		// Piggy-backed on this existing scheduled job rather than adding a second cron endpoint.
		const reapedInvitations = await reapAllExpiredInvitations(run.db);

		// Manual Issues M2 (design §4): same piggyback as reapAllExpiredInvitations above -- an
		// unconditional sweep of DRAFT attachments never linked to an issue/comment within 24h,
		// across every org, driven by this existing scheduled job rather than a second cron route.
		const reapedAttachments = await reapAllOrphanAttachments(run.db, run.signal);

		// N7c (A03): same piggyback again -- force-release agent claims an unattended loop abandoned.
		const claimReap = await reapStaleClaims(claimStaleHours, run.db);

		// N9 (D21): and once more -- age out idempotency keys past their 7-day dedupe window.
		const reapedIdempotencyKeys = await reapExpiredIdempotencyKeys(run.db);

		// sentinel-user-report-issue §8: and the intake endpoint's own 7-day key space, which is the
		// calling API key row's rather than an agent's.
		const reapedReportIdempotencyKeys = await reapExpiredReportIdempotencyKeys(run.db);

		// A run that finished but stopped deleting occurrences early is `behind`, not `completed`.
		const outcome = retentionRunOutcome(result);
		recordRetentionRun(outcome);
		log.info('retention_cron.completed', {
			outcome,
			routeMs: Math.round(performance.now() - routeStart),
			reapedInvitations,
			reapedAttachments,
			releasedClaims: claimReap.releasedClaims,
			reapedIdempotencyKeys,
			reapedReportIdempotencyKeys,
			spanMaintenance,
			deletedOccurrences: result.deletedOccurrences,
			// sentinel-affected-users §4.5 (flow R3): side rows deleted, NOT issues touched.
			deletedAffectedUsers: result.deletedAffectedUsers,
			deletedAffectedTenants: result.deletedAffectedTenants,
			// sentinel-issue-trend §4.5 (flow R3): bucket rows deleted, beside the side-table counts.
			deletedOccurrenceBuckets: result.deletedOccurrenceBuckets,
			deletedOrphanedIssues: result.deletedOrphanedIssues,
			tombstonesWritten: result.tombstonesWritten,
			deletedTombstones: result.deletedTombstones,
			// sentinel-issue-tag-facets §5: the occurrence step and the standing keep-up signal P5 reads.
			occurrenceStopped: result.occurrenceStopped,
			occurrenceBatches: result.occurrenceBatches,
			occurrenceStepMs: result.occurrenceStepMs,
			occurrenceBatchSize: result.occurrenceBatchSize,
			occurrenceHeldP95Ms: result.occurrenceHeldP95Ms,
			occurrenceHeldMaxMs: result.occurrenceHeldMaxMs,
			occurrenceBatchesOverCeiling: result.occurrenceBatchesOverCeiling,
			occurrenceBacklogSeconds: result.occurrenceBacklogSeconds,
			oldestOccurrenceAt: result.oldestOccurrenceAt,
			tagRollupDriftEvents: result.tagRollupDriftEvents,
			tagRollupRebuildsPending: result.tagRollupRebuildsPending,
			tagRollupRebuildsOperatorOnly: result.tagRollupRebuildsOperatorOnly,
			rebuildStepMs: result.rebuildStepMs,
			cutoffDate: result.cutoffDate.toISOString(),
		});

		return json({
			success: true,
			result: {
				reapedInvitations,
				reapedAttachments,
				releasedClaims: claimReap.releasedClaims,
				reapedIdempotencyKeys,
				reapedReportIdempotencyKeys,
				spanMaintenance,
				deletedOccurrences: result.deletedOccurrences,
				deletedAffectedUsers: result.deletedAffectedUsers,
				deletedAffectedTenants: result.deletedAffectedTenants,
				deletedOccurrenceBuckets: result.deletedOccurrenceBuckets,
				deletedOrphanedIssues: result.deletedOrphanedIssues,
				tombstonesWritten: result.tombstonesWritten,
				deletedTombstones: result.deletedTombstones,
				retentionDays: result.retentionDays,
				manualRetentionDays: result.manualRetentionDays,
				tombstoneRetentionDays: result.tombstoneRetentionDays,
				cutoffDate: result.cutoffDate.toISOString(),
			},
		});
	} catch (error) {
		const code = (error as { code?: unknown } | null)?.code;
		if (isConnectionLossError(error) || lost) {
			lost = true; // never send the unlock on a session that is gone
			log.error('retention_cron.connection_lost', { error });
			recordRetentionRun('connection_lost');
			return json({ error: 'retention_connection_lost' }, { status: 500 });
		}
		if (
			error instanceof RetentionDeadlineError ||
			((code === '57014' || code === '55P03') && run !== null && run.left() < RETENTION_RUN_MIN_STATEMENT_MS)
		) {
			log.error('retention_cron.deadline_exceeded', {
				routeMs: run ? Math.round(RETENTION_ROUTE_DEADLINE_MS - run.left()) : null,
				error: error instanceof Error ? error.message : String(error),
			});
			recordRetentionRun('deadline_exceeded');
			return json({ error: 'retention_deadline_exceeded' }, { status: 500 });
		}
		log.error('retention_cron.failed', { error });
		recordRetentionRun('failed');
		return json(
			{
				error: 'Retention cleanup failed',
				message: error instanceof Error ? error.message : 'Unknown error',
			},
			{ status: 500 }
		);
	} finally {
		// Ending the session releases the lock in any case; the explicit unlock only makes the release
		// synchronous for a back-to-back POST. Never after a loss: it would reconnect without the lock.
		if (granted && !lost) {
			try {
				await client`SELECT pg_advisory_unlock(hashtext('sentinel.retention.route'))`;
			} catch {
				// the end() below releases it
			}
		}
		await client.end({ timeout: 1 }).catch(() => {});
	}
};
