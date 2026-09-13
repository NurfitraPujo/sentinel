import type { RequestHandler } from '@sveltejs/kit';
import { json } from '@sveltejs/kit';
import {
	cleanupRetainedData,
	reapStaleClaims,
	reapExpiredIdempotencyKeys,
	reapExpiredReportIdempotencyKeys,
} from '$lib/server/retention';
import { reapAllExpiredInvitations } from '$lib/db/queries/organizations';
import { reapAllOrphanAttachments } from '$lib/server/attachment-reaper';
import { env } from '$env/dynamic/private';
import { log } from '$lib/server/observability/log';
import { getRetentionDays } from '$lib/server/retention-window';

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

	try {
		const result = await cleanupRetainedData(
			retentionDays,
			manualRetentionDays,
			tombstoneRetentionDays
		);

		// D42: sweep expired pending invitations across every organization. The per-org reaper only
		// fires when that org issues another invitation, so an org that stops inviting never reaps.
		// Piggy-backed on this existing scheduled job rather than adding a second cron endpoint.
		const reapedInvitations = await reapAllExpiredInvitations();

		// Manual Issues M2 (design §4): same piggyback as reapAllExpiredInvitations above -- an
		// unconditional sweep of DRAFT attachments never linked to an issue/comment within 24h,
		// across every org, driven by this existing scheduled job rather than a second cron route.
		const reapedAttachments = await reapAllOrphanAttachments();

		// N7c (A03): same piggyback again -- force-release agent claims an unattended loop abandoned.
		const claimReap = await reapStaleClaims(claimStaleHours);

		// N9 (D21): and once more -- age out idempotency keys past their 7-day dedupe window.
		const reapedIdempotencyKeys = await reapExpiredIdempotencyKeys();

		// sentinel-user-report-issue §8: and the intake endpoint's own 7-day key space, which is the
		// calling API key row's rather than an agent's.
		const reapedReportIdempotencyKeys = await reapExpiredReportIdempotencyKeys();

		log.info('retention_cron.completed', {
			reapedInvitations,
			reapedAttachments,
			releasedClaims: claimReap.releasedClaims,
			reapedIdempotencyKeys,
			reapedReportIdempotencyKeys,
			deletedOccurrences: result.deletedOccurrences,
			// sentinel-affected-users §4.5 (flow R3): side rows deleted, NOT issues touched.
			deletedAffectedUsers: result.deletedAffectedUsers,
			deletedAffectedTenants: result.deletedAffectedTenants,
			deletedOrphanedIssues: result.deletedOrphanedIssues,
			tombstonesWritten: result.tombstonesWritten,
			deletedTombstones: result.deletedTombstones,
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
				deletedOccurrences: result.deletedOccurrences,
				deletedAffectedUsers: result.deletedAffectedUsers,
				deletedAffectedTenants: result.deletedAffectedTenants,
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
		log.error('retention_cron.failed', { error });
		return json(
			{
				error: 'Retention cleanup failed',
				message: error instanceof Error ? error.message : 'Unknown error',
			},
			{ status: 500 }
		);
	}
};
