import type { ServiceSli } from './config';

/**
 * sentinel-reporting-and-dashboard D6: copy each SLI's hourly denominators from VictoriaMetrics into `sli_hourly`.
 *
 * Every value is `increase(...[1h])` evaluated at `hour_start + 1h`, summed, rounded to an integer. Instead of one
 * instant query per hour, a single `query_range` per series with step 1h returns the same values (each step evaluates
 * the expression at that instant), so the 30-day first-run backfill is a handful of requests, and a normal run is
 * under 10.
 *
 * Failure rules (D6): a VictoriaMetrics failure throws `VictoriaMetricsError` BEFORE anything is written, so a run
 * never writes a partial set and never writes zeros. A series with no sample for an hour writes no row for that hour.
 * The one interpretation made here: when `total` has a sample but the `errors` series has none, errors is 0 (a
 * filtered counter that never fired has no series; the total existing proves the pipeline was scraping). `fast` with
 * no sample stays NULL.
 */

export const HOUR_MS = 3_600_000;
export const BACKFILL_DAYS = 30;
/** The last N completed hours re-upserted every run, so a late scrape is absorbed by the next run. */
export const REUPSERT_HOURS = 3;

export class VictoriaMetricsError extends Error {}

export interface SliHourRow {
	sliId: string;
	environment: string;
	hourStart: Date;
	total: number;
	errors: number;
	fast: number | null;
	/** http only: the latency ratio's denominator (requests on `source` alone). NULL for jobs/nats. */
	latencyTotal: number | null;
}

export interface SliStore {
	/** Does the table already hold any row for this SLI in this environment? Empty → the 30-day backfill. */
	hasRows(sliId: string, environment: string): Promise<boolean>;
	/** Upsert by primary key, atomically. */
	upsert(rows: SliHourRow[]): Promise<void>;
}

export interface CollectOptions {
	slis: ServiceSli[];
	/** `example-production` or `example-staging`: the cluster this Sentinel belongs to (D6). */
	cluster: string;
	vmUrl: string;
	store: SliStore;
	now?: Date;
	fetchFn?: typeof fetch;
}

const CLUSTER_ENVIRONMENT: Record<string, string> = {
	'example-production': 'production',
	'example-staging': 'staging',
};

export function environmentForCluster(cluster: string): string {
	const env = CLUSTER_ENVIRONMENT[cluster];
	if (!env) throw new Error(`unknown SLI cluster "${cluster}" (expected one of ${Object.keys(CLUSTER_ENVIRONMENT).join(', ')})`);
	return env;
}

const q = (v: string) => v.replace(/\\/g, '\\\\').replace(/"/g, '\\"');

/** The PromQL expressions of a kind (D6 table). `fast` is null for kinds with no latency objective; `latencyTotal` is non-null
 *  only when an http SLI has a `streamSource` (otherwise it equals `total`, and the collector reuses that value). */
export function queriesFor(sli: ServiceSli, cluster: string): { total: string; errors: string; fast: string | null; latencyTotal: string | null } {
	const c = q(cluster);
	switch (sli.kind) {
		case 'http': {
			const n = q(sli.source);
			// Latency reads the ordinary cluster only; availability also reads the SSE cluster so stream failures count.
			const avail = sli.streamSource ? `=~"${n}|${q(sli.streamSource)}"` : `="${n}"`;
			return {
				total: `sum(increase(envoy_cluster_upstream_rq_xx{cluster="${c}", envoy_cluster_name${avail}}[1h]))`,
				errors: `sum(increase(envoy_cluster_upstream_rq_xx{cluster="${c}", envoy_cluster_name${avail}, envoy_response_code_class="5"}[1h]))`,
				fast: sli.latency
					? `sum(increase(envoy_cluster_upstream_rq_time_bucket{cluster="${c}", envoy_cluster_name="${n}", le="${q(sli.latency.le)}"}[1h]))`
					: null,
				latencyTotal: sli.streamSource
					? `sum(increase(envoy_cluster_upstream_rq_xx{cluster="${c}", envoy_cluster_name="${n}"}[1h]))`
					: null,
			};
		}
		case 'jobs':
			return {
				total: `sum(increase(example_core_job_handler_runs_total{cluster="${c}"}[1h]))`,
				errors: `sum(increase(example_core_job_handler_runs_total{cluster="${c}", status="error"}[1h]))`,
				fast: null,
				latencyTotal: null,
			};
		case 'nats':
			return {
				total: `sum(increase(example_core_nats_handler_runs_total{cluster="${c}", status=~"success|error"}[1h]))`,
				errors: `sum(increase(example_core_nats_handler_runs_total{cluster="${c}", status="error"}[1h]))`,
				fast: null,
				latencyTotal: null,
			};
	}
}

/** The start of the hour containing `t`, UTC. */
export function floorHour(t: Date): Date {
	return new Date(Math.floor(t.getTime() / HOUR_MS) * HOUR_MS);
}

/** One GET against VictoriaMetrics' Prometheus-compatible API; throws VictoriaMetricsError on anything but success. */
export async function vmGet(fetchFn: typeof fetch, vmUrl: string, path: string, params: Record<string, string>, timeoutMs = 30_000): Promise<{ result: any[] }> {
	const url = new URL(path, vmUrl);
	for (const [k, v] of Object.entries(params)) url.searchParams.set(k, v);
	let res: Response;
	try {
		res = await fetchFn(url, { signal: AbortSignal.timeout(timeoutMs) });
	} catch (err) {
		throw new VictoriaMetricsError(`VictoriaMetrics request failed: ${(err as Error).message}`);
	}
	if (!res.ok) throw new VictoriaMetricsError(`VictoriaMetrics answered ${res.status}`);
	let body: any;
	try {
		body = await res.json();
	} catch {
		throw new VictoriaMetricsError('VictoriaMetrics answered a body that is not JSON');
	}
	if (body?.status !== 'success' || !Array.isArray(body?.data?.result)) {
		throw new VictoriaMetricsError(`VictoriaMetrics answered status ${String(body?.status)}`);
	}
	return body.data;
}

async function queryRange(
	fetchFn: typeof fetch,
	vmUrl: string,
	query: string,
	firstEval: Date,
	lastEval: Date
): Promise<Map<number, number>> {
	const body = { data: await vmGet(fetchFn, vmUrl, '/api/v1/query_range', {
		query, start: String(firstEval.getTime() / 1000), end: String(lastEval.getTime() / 1000), step: '3600',
	}) };
	// `sum(...)` yields at most one series; sum across any that appear, per timestamp.
	const out = new Map<number, number>();
	for (const series of body.data.result) {
		for (const [ts, raw] of series.values ?? []) {
			const v = Number(raw);
			if (!Number.isFinite(v)) continue; // NaN/Inf is an absent sample, never a zero
			out.set(Number(ts), (out.get(Number(ts)) ?? 0) + v);
		}
	}
	return out;
}

/** Collect every SLI. Returns the rows written. Throws VictoriaMetricsError with nothing written. */
export async function collectSli(opts: CollectOptions): Promise<SliHourRow[]> {
	const fetchFn = opts.fetchFn ?? fetch;
	const environment = environmentForCluster(opts.cluster);
	const now = opts.now ?? new Date();
	const lastHour = new Date(floorHour(now).getTime() - HOUR_MS); // last COMPLETED hour's start

	const rows: SliHourRow[] = [];
	for (const sli of opts.slis) {
		const backfill = !(await opts.store.hasRows(sli.id, environment));
		const hours = backfill ? BACKFILL_DAYS * 24 : REUPSERT_HOURS;
		const firstHour = new Date(lastHour.getTime() - (hours - 1) * HOUR_MS);
		const firstEval = new Date(firstHour.getTime() + HOUR_MS);
		const lastEval = new Date(lastHour.getTime() + HOUR_MS);
		const qs = queriesFor(sli, opts.cluster);

		const total = await queryRange(fetchFn, opts.vmUrl, qs.total, firstEval, lastEval);
		const errors = await queryRange(fetchFn, opts.vmUrl, qs.errors, firstEval, lastEval);
		const fast = qs.fast ? await queryRange(fetchFn, opts.vmUrl, qs.fast, firstEval, lastEval) : null;
		const latencyTotal = qs.latencyTotal ? await queryRange(fetchFn, opts.vmUrl, qs.latencyTotal, firstEval, lastEval) : null;

		for (let h = firstHour.getTime(); h <= lastHour.getTime(); h += HOUR_MS) {
			const evalSec = (h + HOUR_MS) / 1000;
			const t = total.get(evalSec);
			if (t === undefined) continue; // absent series: no row, never zeros
			rows.push({
				sliId: sli.id,
				environment,
				hourStart: new Date(h),
				total: Math.round(t),
				errors: Math.round(errors.get(evalSec) ?? 0),
				fast: fast ? (fast.has(evalSec) ? Math.round(fast.get(evalSec)!) : null) : null,
				// http: requests on `source` alone when a stream cluster is split off, else the same value as `total`.
				latencyTotal: sli.kind !== 'http' ? null : latencyTotal ? (latencyTotal.has(evalSec) ? Math.round(latencyTotal.get(evalSec)!) : null) : Math.round(t),
			});
		}
	}
	if (rows.length) await opts.store.upsert(rows);
	return rows;
}
export { q as promQuote };
