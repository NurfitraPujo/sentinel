import type { RequestHandler } from '@sveltejs/kit';
import { json } from '@sveltejs/kit';
import { env } from '$env/dynamic/private';
import { log } from '$lib/server/observability/log';
import { parseServiceSliConfig } from '$lib/server/sli/config';
import { collectSli, VictoriaMetricsError } from '$lib/server/sli/collect';
import { pgSliStore } from '$lib/server/sli/store';

const CRON_SECRET_HEADER = 'x-cron-secret';

/**
 * sentinel-reporting-and-dashboard D6: hourly SLI denominators from VictoriaMetrics into `sli_hourly`.
 *
 * Statuses (contract §6): a missing or wrong `x-cron-secret` answers exactly what /api/cron/retention answers (401,
 * and 500 when CRON_SECRET is unset); 500 on a VictoriaMetrics failure, with nothing written; 200 on success.
 */
export const POST: RequestHandler = async ({ request }) => {
	const cronSecret = request.headers.get(CRON_SECRET_HEADER);
	if (!cronSecret) return json({ error: 'Missing cron secret' }, { status: 401 });

	const expectedSecret = env.CRON_SECRET;
	if (!expectedSecret) {
		log.error('sli_collect.not_configured', { note: 'CRON_SECRET environment variable is not set' });
		return json({ error: 'Cron endpoint not configured' }, { status: 500 });
	}
	if (cronSecret !== expectedSecret) return json({ error: 'Invalid cron secret' }, { status: 401 });

	let slis;
	try {
		slis = parseServiceSliConfig(env.SERVICE_SLI_CONFIG);
	} catch (err) {
		log.error('sli_collect.bad_config', { error: err });
		return json({ error: 'SERVICE_SLI_CONFIG is invalid' }, { status: 500 });
	}
	const vmUrl = env.VICTORIAMETRICS_URL;
	const cluster = env.SLI_CLUSTER;
	if (slis.length === 0 || !vmUrl || !cluster) {
		// Nothing configured is not a failure: the chart renders these only where collection is wanted (CCE has none).
		log.info('sli_collect.skipped', { slis: slis.length, haveVmUrl: !!vmUrl, haveCluster: !!cluster });
		return json({ ok: true, skipped: true, rows: 0 });
	}

	try {
		const rows = await collectSli({ slis, cluster, vmUrl, store: pgSliStore });
		log.info('sli_collect.done', { rows: rows.length, slis: slis.length });
		return json({ ok: true, rows: rows.length });
	} catch (err) {
		log.error('sli_collect.failed', { error: err });
		if (err instanceof VictoriaMetricsError) return json({ error: 'VictoriaMetrics query failed' }, { status: 500 });
		return json({ error: 'SLI collection failed' }, { status: 500 });
	}
};
