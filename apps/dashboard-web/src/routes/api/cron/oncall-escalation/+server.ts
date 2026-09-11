import type { RequestHandler } from '@sveltejs/kit';
import { json } from '@sveltejs/kit';
import {
	sweepEscalations,
	sendEscalationEmails,
	resolveAckMinutes,
	resolveMaxHops,
} from '$lib/server/escalation';
import { env } from '$env/dynamic/private';
import { log } from '$lib/server/observability/log';

/**
 * sentinel-user-report-issue §4.4b — `POST /api/cron/oncall-escalation`.
 *
 * **A separate endpoint and a separate schedule, not a piggyback on retention.** Retention runs
 * hourly (`RETENTION_CRON_INTERVAL_SECONDS: "3600"`); a 15-minute acknowledgement deadline swept
 * hourly is a 75-minute deadline. This one is called every minute — by
 * `deploy/helm/sentinel/templates/oncall-cronjob.yaml` at every-minute cron on Kubernetes, or by the
 * second loop in `scripts/cron-entrypoint.sh` under compose. Both ship OFF.
 *
 * The guard is `retention/+server.ts`'s, statement for statement and in the same order: a missing
 * header is `401` before the configuration is consulted, an unset `CRON_SECRET` is a `500` that
 * says the endpoint is not configured (never a silent 200 that would let an unauthenticated caller
 * drive reassignment), and a wrong secret is the same `401` a missing one gets.
 *
 * **The sweep is idempotent and safe to run concurrently with itself** (§4.4b): every row is
 * claimed with a conditional UPDATE before anything is written, so a doubled schedule — a CronJob
 * and the compose loop both pointed at one dashboard, or two Kubernetes runs overlapping despite
 * `concurrencyPolicy: Forbid` — cannot double-reassign. Running it twice in a row is a no-op the
 * second time.
 *
 * Emails are sent AFTER the sweep's transactions have committed, following notify.ts's §8 rule
 * that an SMTP failure must never roll back the mutation it describes.
 */

const CRON_SECRET_HEADER = 'x-cron-secret';

export const POST: RequestHandler = async ({ request, url }) => {
	const cronSecret = request.headers.get(CRON_SECRET_HEADER);

	if (!cronSecret) {
		return json({ error: 'Missing cron secret' }, { status: 401 });
	}

	const expectedSecret = env.CRON_SECRET;

	if (!expectedSecret) {
		log.error('oncall_escalation_cron.not_configured', {
			note: 'CRON_SECRET environment variable is not set',
		});
		return json({ error: 'Cron endpoint not configured' }, { status: 500 });
	}

	if (cronSecret !== expectedSecret) {
		return json({ error: 'Invalid cron secret' }, { status: 401 });
	}

	const ackMinutes = resolveAckMinutes();
	const maxHops = resolveMaxHops();

	try {
		const result = await sweepEscalations({ ackMinutes, maxHops });

		// Post-commit, best-effort, and never able to fail the sweep. One send per (issue, engineer)
		// pair: a single sweep can move several reports, and each engineer must get a link to the
		// one they were actually put on.
		await sendEscalationEmails(result.notified, url.origin);

		const summary = {
			due: result.due,
			reassigned: result.reassigned,
			exhausted: result.exhausted,
			stoppedByReassignment: result.stoppedByReassignment,
			skipped: result.skipped,
			// sentinel-project-environments §2.2, decisions 38/41: due rows whose next candidate did
			// not hold the issue's environment, so the whole rota was paged and the row terminated.
			unscoped: result.unscoped,
			contended: result.contended,
			ackMinutes,
			maxHops,
		};

		log.info('oncall_escalation_cron.completed', summary);

		return json({ success: true, result: summary });
	} catch (error) {
		log.error('oncall_escalation_cron.failed', { error });
		return json(
			{
				error: 'Escalation sweep failed',
				message: error instanceof Error ? error.message : 'Unknown error',
			},
			{ status: 500 }
		);
	}
};
