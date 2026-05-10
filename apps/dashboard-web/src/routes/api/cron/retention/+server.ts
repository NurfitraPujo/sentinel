import type { RequestHandler } from '@sveltejs/kit';
import { json } from '@sveltejs/kit';
import { cleanupRetainedData } from '$lib/server/retention';
import { env } from '$env/dynamic/private';

const CRON_SECRET_HEADER = 'x-cron-secret';

export const POST: RequestHandler = async ({ request }) => {
	const cronSecret = request.headers.get(CRON_SECRET_HEADER);

	if (!cronSecret) {
		return json({ error: 'Missing cron secret' }, { status: 401 });
	}

	const expectedSecret = env.CRON_SECRET;

	if (!expectedSecret) {
		console.error('[RetentionCron] CRON_SECRET environment variable is not set');
		return json({ error: 'Cron endpoint not configured' }, { status: 500 });
	}

	if (cronSecret !== expectedSecret) {
		return json({ error: 'Invalid cron secret' }, { status: 401 });
	}

	const retentionDays = parseInt(env.DATA_RETENTION_DAYS ?? '30', 10);

	console.log(`[RetentionCron] Starting data retention cleanup for ${retentionDays} days`);

	try {
		const result = await cleanupRetainedData(retentionDays);

		console.log('[RetentionCron] Cleanup completed:', {
			deletedOccurrences: result.deletedOccurrences,
			markedStaleIssues: result.markedStaleIssues,
			deletedOrphanedIssues: result.deletedOrphanedIssues,
			cutoffDate: result.cutoffDate.toISOString(),
		});

		return json({
			success: true,
			result: {
				deletedOccurrences: result.deletedOccurrences,
				markedStaleIssues: result.markedStaleIssues,
				deletedOrphanedIssues: result.deletedOrphanedIssues,
				retentionDays: result.retentionDays,
				cutoffDate: result.cutoffDate.toISOString(),
			},
		});
	} catch (error) {
		console.error('[RetentionCron] Cleanup failed:', error);
		return json(
			{
				error: 'Retention cleanup failed',
				message: error instanceof Error ? error.message : 'Unknown error',
			},
			{ status: 500 }
		);
	}
};
