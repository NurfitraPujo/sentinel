import { describe, it, expect, beforeEach } from 'vitest';
import {
	REPORT_OUTCOME,
	recordReportReceived,
	renderMetrics,
	metricsRegistry,
	resetMetricsForTest,
	ESCALATION_OUTCOME,
	recordEscalation,
	recordEscalationExhausted,
	recordOncallScheduleUnresolved,
	recordReleaseNextRewriteFailure,
	SOURCEMAP_UPLOAD_OUTCOME,
	recordSourcemapUpload,
	RETENTION_RUN_OUTCOME,
	recordRetentionRun,
} from './metrics';

/**
 * sentinel-user-report-issue SR-3 / §12 — the metrics surface this item stands up.
 *
 * What is asserted is what §12's alert table actually depends on: the metric NAME, the single
 * fixed `outcome` label, that every value in the closed set exists as a series from process start
 * (a ratio alert over an absent series does not evaluate), and that nothing caller-controlled can
 * become a label. The last one is the unbounded-cardinality rule `procmetrics`' doc comment
 * states, and it is enforced by the type of `recordReportReceived`, so the test asserts the
 * rendered output carries no other label rather than trying to call it with a bad value.
 */

function parse(body: string): Map<string, number> {
	const out = new Map<string, number>();
	for (const line of body.split('\n')) {
		if (!line || line.startsWith('#')) continue;
		const match = line.match(/^(\S+)\s+(\S+)$/);
		if (match) out.set(match[1], Number(match[2]));
	}
	return out;
}

describe('SR-3 sentinel_reports_received_total', () => {
	beforeEach(() => {
		resetMetricsForTest();
	});

	it('exposes the metric under its §12 name', async () => {
		const { body } = await renderMetrics();
		expect(body).toContain('# TYPE sentinel_reports_received_total counter');
	});

	it('initialises EVERY outcome to zero, so an alert on outcome="error" evaluates from t=0', async () => {
		const series = parse((await renderMetrics()).body);
		for (const outcome of Object.values(REPORT_OUTCOME)) {
			expect(series.get(`sentinel_reports_received_total{outcome="${outcome}"}`)).toBe(0);
		}
	});

	it('carries the closed set of outcomes and nothing else', () => {
		expect(Object.values(REPORT_OUTCOME).sort()).toEqual([
			'conflict',
			'created',
			'deduplicated',
			'error',
			'forbidden',
			'ratelimited',
			'rejected',
			'unauthorized',
		]);
	});

	it('increments the named series and leaves the others alone', async () => {
		recordReportReceived(REPORT_OUTCOME.Created);
		recordReportReceived(REPORT_OUTCOME.Created);
		recordReportReceived(REPORT_OUTCOME.Error);

		const series = parse((await renderMetrics()).body);
		expect(series.get('sentinel_reports_received_total{outcome="created"}')).toBe(2);
		expect(series.get('sentinel_reports_received_total{outcome="error"}')).toBe(1);
		expect(series.get('sentinel_reports_received_total{outcome="rejected"}')).toBe(0);
	});

	it('carries `outcome` as its ONLY label — no org, project, key or reporter may reach a label', async () => {
		recordReportReceived(REPORT_OUTCOME.Rejected);
		const { body } = await renderMetrics();
		for (const line of body.split('\n')) {
			if (!line.startsWith('sentinel_reports_received_total{')) continue;
			const labels = line.slice(line.indexOf('{') + 1, line.indexOf('}'));
			expect(labels).toMatch(/^outcome="[a-z]+"$/);
		}
	});

	it('renders the Prometheus text exposition content type a scraper expects', async () => {
		const { contentType } = await renderMetrics();
		expect(contentType).toContain('text/plain');
		expect(contentType).toContain('version=0.0.4');
	});

	it('registers the metric exactly once, so a module re-evaluation cannot throw or double-count', async () => {
		// The HMR hazard the global singleton exists for: a second `state()` must reuse the registry.
		const first = metricsRegistry();
		recordReportReceived(REPORT_OUTCOME.Created);
		expect(metricsRegistry()).toBe(first);
		const series = parse((await renderMetrics()).body);
		expect(series.get('sentinel_reports_received_total{outcome="created"}')).toBe(1);
	});

	it('never throws, whatever it is handed at runtime', () => {
		// Values are typed, but the recording helper is on the response path of an endpoint that
		// must not 500 because of a counter (`procmetrics`' nil guard, ported).
		expect(() => recordReportReceived('created')).not.toThrow();
		expect(() => recordReportReceived('not-an-outcome' as never)).not.toThrow();
	});

	it('DROPS an outcome outside the closed set instead of minting a new time series', async () => {
		recordReportReceived('u_8f3c41' as never);
		const { body } = await renderMetrics();
		expect(body).not.toContain('u_8f3c41');
		const series = [...parse(body).keys()].filter((k) =>
			k.startsWith('sentinel_reports_received_total')
		);
		expect(series).toHaveLength(Object.values(REPORT_OUTCOME).length);
	});
});

/**
 * The escalation half of §12's table. Same three properties as above, and the third one matters
 * more here than anywhere: `sentinel_report_escalations_exhausted_total` and
 * `sentinel_oncall_schedule_unresolved_total` are the ONLY things that notice two states §4.4
 * designs to be silent, so an alert on `> 0` that cannot evaluate because the series does not
 * exist yet is the same as no alert at all.
 */
describe('§12 escalation metrics', () => {
	beforeEach(() => {
		resetMetricsForTest();
	});

	it('exposes all three under their §12 names', async () => {
		const { body } = await renderMetrics();
		expect(body).toContain('# TYPE sentinel_report_escalations_total counter');
		expect(body).toContain('# TYPE sentinel_report_escalations_exhausted_total counter');
		expect(body).toContain('# TYPE sentinel_oncall_schedule_unresolved_total counter');
	});

	it('initialises every series to zero, so a "> 0" alert evaluates from t=0', async () => {
		const series = parse((await renderMetrics()).body);
		for (const outcome of Object.values(ESCALATION_OUTCOME)) {
			expect(series.get(`sentinel_report_escalations_total{outcome="${outcome}"}`)).toBe(0);
		}
		expect(series.get('sentinel_report_escalations_exhausted_total')).toBe(0);
		expect(series.get('sentinel_oncall_schedule_unresolved_total')).toBe(0);
	});

	it('carries the closed set of escalation outcomes and nothing else', () => {
		expect(Object.values(ESCALATION_OUTCOME).sort()).toEqual([
			'assigned',
			'failed',
			'reassigned',
		]);
	});

	it('counts each signal on its own counter', async () => {
		recordEscalation(ESCALATION_OUTCOME.Assigned);
		recordEscalation(ESCALATION_OUTCOME.Assigned);
		recordEscalation(ESCALATION_OUTCOME.Reassigned);
		recordEscalationExhausted();
		recordOncallScheduleUnresolved();
		recordOncallScheduleUnresolved();

		const series = parse((await renderMetrics()).body);
		expect(series.get('sentinel_report_escalations_total{outcome="assigned"}')).toBe(2);
		expect(series.get('sentinel_report_escalations_total{outcome="reassigned"}')).toBe(1);
		expect(series.get('sentinel_report_escalations_total{outcome="failed"}')).toBe(0);
		expect(series.get('sentinel_report_escalations_exhausted_total')).toBe(1);
		expect(series.get('sentinel_oncall_schedule_unresolved_total')).toBe(2);
	});

	it('drops an outcome outside the closed set rather than minting a series', async () => {
		// The unbounded-cardinality rule at runtime, not only in the type system: an organization
		// id reaching a label is exactly the failure §12's "fixed label constants" forbids.
		expect(() => recordEscalation('11111111-1111-4111-8111-111111111111' as never)).not.toThrow();
		const body = (await renderMetrics()).body;
		expect(body).not.toContain('11111111-1111-4111-8111-111111111111');
	});

	it('never emits a label other than outcome', async () => {
		recordEscalation(ESCALATION_OUTCOME.Failed);
		const body = (await renderMetrics()).body;
		for (const line of body.split('\n')) {
			if (!line.startsWith('sentinel_report_escalations_total{')) continue;
			expect(line).toMatch(/^sentinel_report_escalations_total\{outcome="[a-z]+"\} \d+$/);
		}
		// The two unlabelled counters must stay unlabelled — there is nothing to break either down
		// by that is not tenant-identifying.
		expect(body).not.toMatch(/sentinel_report_escalations_exhausted_total\{/);
		expect(body).not.toMatch(/sentinel_oncall_schedule_unresolved_total\{/);
	});
});

/**
 * sentinel-releases-snooze §4.3 "Hourly" / §12 (decision 30): the dashboard's half of
 * `sentinel_release_next_rewrite_failures_total`. Three properties, and the third is the one that
 * matters: the NAME is the processor's (`obs.MetricReleaseNextRewriteFailures`, pinned by
 * tests/unit's TestObsReleaseNextRewriteMetricName), the series exists at zero from process start
 * so a `> 0` alert evaluates, and there is NO label -- the two processes' series are summed by name
 * at the scrape, and a label on this side would make them two metrics.
 */
describe('§12 sentinel_release_next_rewrite_failures_total (sentinel-releases-snooze)', () => {
	beforeEach(() => {
		resetMetricsForTest();
	});

	it('is registered under the processor\'s exact name and initialised to zero', async () => {
		const { body } = await renderMetrics();
		expect(body).toContain('# TYPE sentinel_release_next_rewrite_failures_total counter');
		expect(parse(body).get('sentinel_release_next_rewrite_failures_total')).toBe(0);
	});

	it('increments by one per recorded failure', async () => {
		recordReleaseNextRewriteFailure();
		recordReleaseNextRewriteFailure();
		expect(parse((await renderMetrics()).body).get('sentinel_release_next_rewrite_failures_total')).toBe(2);
	});

	it('carries NO label, so it sums by name with the processor\'s counter at the scrape', async () => {
		recordReleaseNextRewriteFailure();
		const { body } = await renderMetrics();
		const series = body.split('\n').filter((l) => l.startsWith('sentinel_release_next_rewrite_failures_total'));
		expect(series).toHaveLength(1);
		expect(series[0]).toMatch(/^sentinel_release_next_rewrite_failures_total 1$/);
	});

	it('never throws', () => {
		expect(() => recordReleaseNextRewriteFailure()).not.toThrow();
	});
});

/**
 * sentinel-sourcemaps §12: `sentinel_sourcemap_uploads_total{outcome}` in `REPORT_OUTCOME`'s
 * shape. The five-value set is the contract's own (created · replaced · unchanged · rejected ·
 * storage_error), and `storage_error` is the series `SentinelSourcemapUploadStorageErrors` reads,
 * so it must exist at zero from the first scrape.
 */
describe('§12 sentinel_sourcemap_uploads_total (sentinel-sourcemaps)', () => {
	beforeEach(() => {
		resetMetricsForTest();
	});

	it('carries exactly the contract\'s five outcomes', () => {
		expect(Object.values(SOURCEMAP_UPLOAD_OUTCOME).sort()).toEqual(
			['created', 'rejected', 'replaced', 'storage_error', 'unchanged'].sort()
		);
	});

	it('exposes the metric under its §12 name with every outcome initialised to zero', async () => {
		const { body } = await renderMetrics();
		expect(body).toContain('# TYPE sentinel_sourcemap_uploads_total counter');
		const series = parse(body);
		for (const outcome of Object.values(SOURCEMAP_UPLOAD_OUTCOME)) {
			expect(series.get(`sentinel_sourcemap_uploads_total{outcome="${outcome}"}`)).toBe(0);
		}
	});

	it('increments the named series and leaves the others alone', async () => {
		recordSourcemapUpload('created');
		recordSourcemapUpload('created');
		recordSourcemapUpload('storage_error');
		const series = parse((await renderMetrics()).body);
		expect(series.get('sentinel_sourcemap_uploads_total{outcome="created"}')).toBe(2);
		expect(series.get('sentinel_sourcemap_uploads_total{outcome="storage_error"}')).toBe(1);
		expect(series.get('sentinel_sourcemap_uploads_total{outcome="unchanged"}')).toBe(0);
		expect(series.get('sentinel_sourcemap_uploads_total{outcome="rejected"}')).toBe(0);
		expect(series.get('sentinel_sourcemap_uploads_total{outcome="replaced"}')).toBe(0);
	});

	it('DROPS an outcome outside the closed set and never throws', async () => {
		expect(() => recordSourcemapUpload('proj-1' as never)).not.toThrow();
		const { body } = await renderMetrics();
		expect(body).not.toContain('proj-1');
		const series = [...parse(body).keys()].filter((k) => k.startsWith('sentinel_sourcemap_uploads_total'));
		expect(series).toHaveLength(Object.values(SOURCEMAP_UPLOAD_OUTCOME).length);
	});

	it('carries `outcome` as its ONLY label', async () => {
		recordSourcemapUpload('rejected');
		const { body } = await renderMetrics();
		for (const line of body.split('\n')) {
			if (!line.startsWith('sentinel_sourcemap_uploads_total')) continue;
			const labels = line.match(/\{([^}]*)\}/)?.[1] ?? '';
			expect(labels).toMatch(/^outcome="[a-z_]+"$/);
		}
	});
});

/**
 * sentinel-issue-tag-facets §5 "The standing signal": `sentinel_retention_runs_total{outcome}`, one
 * increment per authorised retention POST. The seven outcomes exist at zero from the first scrape,
 * because `SentinelRetentionNotCompleting` and `SentinelRetentionLockStale` read series that would
 * otherwise be absent until the first such run.
 */
describe('sentinel_retention_runs_total (sentinel-issue-tag-facets)', () => {
	beforeEach(() => {
		resetMetricsForTest();
	});

	it('carries exactly the seven outcomes, behind among them', () => {
		expect(Object.values(RETENTION_RUN_OUTCOME).sort()).toEqual(
			['behind', 'completed', 'connection_lost', 'deadline_exceeded', 'failed', 'lock_stale', 'skipped'].sort()
		);
	});

	it('exposes every outcome at zero, then counts one increment on its own series', async () => {
		let { body } = await renderMetrics();
		expect(body).toContain('# TYPE sentinel_retention_runs_total counter');
		let series = parse(body);
		for (const outcome of Object.values(RETENTION_RUN_OUTCOME)) {
			expect(series.get(`sentinel_retention_runs_total{outcome="${outcome}"}`)).toBe(0);
		}
		recordRetentionRun(RETENTION_RUN_OUTCOME.LockStale);
		({ body } = await renderMetrics());
		series = parse(body);
		expect(series.get('sentinel_retention_runs_total{outcome="lock_stale"}')).toBe(1);
		expect(series.get('sentinel_retention_runs_total{outcome="completed"}')).toBe(0);
	});
});
