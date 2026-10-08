import { computeInsights, renderSamples, type InsightsDeps } from './insights';

/**
 * sentinel-reporting-and-dashboard D20: the last good refresh, served on every scrape.
 *
 * A background refresh recomputes everything from Databend every 5 minutes through the same module as the page. A
 * scrape only serves the last refresh. If a refresh fails the previous values keep being served, and the three
 * `sentinel_insights_*` series say how stale they are. The first refresh starts on the first scrape (or `start()`),
 * so a pod without Databend configured simply serves refresh errors and no insight series.
 */
export const REFRESH_INTERVAL_MS = 5 * 60_000;

export interface InsightsState {
	body: string;
	lastSuccess: number | null;
	errors: number;
	lastDurationMs: number | null;
	serviceLabelOverflow: number;
	refreshing: Promise<void> | null;
	timer: ReturnType<typeof setInterval> | null;
}

export function createInsightsState(): InsightsState {
	return { body: '', lastSuccess: null, errors: 0, lastDurationMs: null, serviceLabelOverflow: 0, refreshing: null, timer: null };
}

export async function refresh(state: InsightsState, deps: () => Promise<InsightsDeps>, log?: (event: string, f: Record<string, unknown>) => void): Promise<void> {
	if (state.refreshing) return state.refreshing;
	const started = Date.now();
	state.refreshing = (async () => {
		try {
			const result = await computeInsights(await deps());
			state.body = renderSamples(result.samples); // replaced only on success: a failure keeps the last good values
			state.serviceLabelOverflow += result.serviceLabelOverflow;
			state.lastSuccess = Date.now();
			state.lastDurationMs = Date.now() - started;
		} catch (err) {
			state.errors += 1;
			log?.('insights.refresh_failed', { error: String((err as Error)?.message ?? err).slice(0, 300) });
		} finally {
			state.refreshing = null;
		}
	})();
	return state.refreshing;
}

/** Starts the 5-minute refresh loop once. The timer never keeps the process alive. */
export function start(state: InsightsState, deps: () => Promise<InsightsDeps>, log?: (event: string, f: Record<string, unknown>) => void): void {
	if (state.timer) return;
	void refresh(state, deps, log);
	state.timer = setInterval(() => void refresh(state, deps, log), REFRESH_INTERVAL_MS);
	state.timer.unref?.();
}

export function render(state: InsightsState): string {
	const own = [
		'# HELP sentinel_insights_refresh_errors_total Failed refreshes of the insight gauges',
		'# TYPE sentinel_insights_refresh_errors_total counter',
		`sentinel_insights_refresh_errors_total ${state.errors}`,
		'# HELP sentinel_insight_service_label_overflow_total Services recorded as other because a project exceeded 30 distinct service labels',
		'# TYPE sentinel_insight_service_label_overflow_total counter',
		`sentinel_insight_service_label_overflow_total ${state.serviceLabelOverflow}`,
	];
	if (state.lastSuccess !== null) {
		own.push(
			'# HELP sentinel_insights_last_success_timestamp_seconds Unix time of the last successful refresh',
			'# TYPE sentinel_insights_last_success_timestamp_seconds gauge',
			`sentinel_insights_last_success_timestamp_seconds ${state.lastSuccess / 1000}`,
			'# HELP sentinel_insights_refresh_duration_ms Duration of the last successful refresh',
			'# TYPE sentinel_insights_refresh_duration_ms gauge',
			`sentinel_insights_refresh_duration_ms ${state.lastDurationMs}`
		);
	}
	return state.body + own.join('\n') + '\n';
}
