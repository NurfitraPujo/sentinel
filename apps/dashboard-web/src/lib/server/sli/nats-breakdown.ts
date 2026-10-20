import { RANGES, type Range } from '../analytics/literals';
import { promQuote, vmGet } from './collect';

/**
 * sentinel-dashboard-metrics-improvement D8: the top NATS contributors to the `core-nats` SLI's failures, from ONE live
 * VictoriaMetrics instant-query pair (errors, runs) instead of a new table. Nothing from the request reaches the query:
 * the cluster is a literal from config (`SLI_CLUSTER`, escaped), the window is one of `RANGES`.
 */

export const NATS_TOP_N = 5;
const METRIC = 'daya_core_nats_handler_runs_total';
/** Short: this runs on a page render, and a slow VictoriaMetrics must cost the list only. */
const TIMEOUT_MS = 8_000;

export interface NatsContributor {
	stream: string;
	consumer: string;
	errors: number;
	runs: number;
	/** errors / runs; null with no runs. */
	rate: number | null;
}

export type NatsBreakdown = { status: 'ok'; rows: NatsContributor[] } | { status: 'unavailable' };

export interface VmConfig {
	url: string;
	cluster: string;
	fetchFn?: typeof fetch;
}

/** The two PromQL expressions. `range` must be one of RANGES (a PromQL duration as spelled there). */
export function natsBreakdownQueries(cluster: string, range: Range): { errors: string; runs: string } {
	if (!(RANGES as readonly string[]).includes(range)) throw new Error(`not a dashboard range: ${String(range).slice(0, 20)}`);
	const c = promQuote(cluster);
	return {
		errors: `topk(${NATS_TOP_N}, sum by (stream, consumer) (increase(${METRIC}{cluster="${c}", status="error"}[${range}])))`,
		runs: `sum by (stream, consumer) (increase(${METRIC}{cluster="${c}"}[${range}]))`,
	};
}

const key = (m: Record<string, string>) => `${m.stream ?? ''}\u0000${m.consumer ?? ''}`;

/** Never throws: a missing config, a VictoriaMetrics failure or an unreadable answer is `unavailable`, for the list only. */
export async function fetchNatsBreakdown(vm: VmConfig | null | undefined, range: Range, now: Date): Promise<NatsBreakdown> {
	if (!vm?.url || !vm.cluster) return { status: 'unavailable' };
	try {
		const fetchFn = vm.fetchFn ?? fetch;
		const qs = natsBreakdownQueries(vm.cluster, range);
		const time = String(now.getTime() / 1000);
		const [errors, runs] = await Promise.all([
			vmGet(fetchFn, vm.url, '/api/v1/query', { query: qs.errors, time }, TIMEOUT_MS),
			vmGet(fetchFn, vm.url, '/api/v1/query', { query: qs.runs, time }, TIMEOUT_MS),
		]);
		const runsBy = new Map<string, number>();
		for (const r of runs.result) {
			const v = Number(r?.value?.[1]);
			if (Number.isFinite(v)) runsBy.set(key(r.metric ?? {}), v);
		}
		const rows: NatsContributor[] = [];
		for (const r of errors.result) {
			const v = Number(r?.value?.[1]);
			if (!Number.isFinite(v)) continue; // NaN/Inf is an absent sample
			const m = r.metric ?? {};
			const run = Math.round(runsBy.get(key(m)) ?? 0);
			rows.push({ stream: m.stream ?? '', consumer: m.consumer ?? '', errors: Math.round(v), runs: run, rate: run > 0 ? v / run : null });
		}
		rows.sort((a, b) => b.errors - a.errors);
		return { status: 'ok', rows: rows.filter((r) => r.errors > 0).slice(0, NATS_TOP_N) };
	} catch (err) {
		console.warn(JSON.stringify({ level: 'warn', event: 'dashboard.nats_breakdown_unavailable', error: String((err as Error)?.message ?? err).slice(0, 300) }));
		return { status: 'unavailable' };
	}
}
