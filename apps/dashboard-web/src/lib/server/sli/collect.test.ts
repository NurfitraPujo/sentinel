import { describe, it, expect } from 'vitest';
import { collectSli, queriesFor, VictoriaMetricsError, HOUR_MS, type SliHourRow, type SliStore } from './collect';
import { parseServiceSliConfig, type ServiceSli } from './config';

const NOW = new Date('2026-10-07T12:20:00Z'); // last completed hour starts 11:00
const HOUR = (iso: string) => Date.parse(iso);

const coreApi: ServiceSli = {
	id: 'core-api', kind: 'http', project: 'core', source: 'api_core_80', availabilityTarget: 0.995,
	latency: { le: '1000.0', target: 0.94 }, windowDays: 28,
};
const coreApiSse: ServiceSli = { ...coreApi, streamSource: 'api_core_80_sse' };
const coreJobs: ServiceSli = { id: 'core-jobs', kind: 'jobs', project: 'core', source: 'daya_core_job_handler_runs_total', windowDays: 28 };
const coreNats: ServiceSli = { id: 'core-nats', kind: 'nats', project: 'core', source: 'daya_core_nats_handler_runs_total', windowDays: 28 };

class MemStore implements SliStore {
	rows = new Map<string, SliHourRow>();
	upserts = 0;
	seeded = new Set<string>();
	async hasRows(sliId: string, env: string) {
		return this.seeded.has(`${sliId}|${env}`) || [...this.rows.values()].some((r) => r.sliId === sliId && r.environment === env);
	}
	async upsert(rows: SliHourRow[]) {
		this.upserts++;
		for (const r of rows) this.rows.set(`${r.sliId}|${r.environment}|${r.hourStart.toISOString()}`, r);
	}
}

/** A stub VictoriaMetrics: `series[kind][hourStartMs] = value`, answered as query_range at hour_start+1h. */
function stubVm(series: Record<string, Record<number, number>>, opts: { fail?: boolean; streams?: boolean } = {}) {
	const queries: string[] = [];
	const fetchFn = (async (input: URL | string) => {
		const url = new URL(String(input));
		const query = url.searchParams.get('query')!;
		queries.push(query);
		if (opts.fail) return new Response('boom', { status: 503 });
		const start = Number(url.searchParams.get('start')) * 1000;
		const end = Number(url.searchParams.get('end')) * 1000;
		const key = query.includes('rq_time_bucket') ? 'fast'
			: query.includes('envoy_response_code_class') ? 'errors'
			: query.includes('status="error"') ? 'errors'
			// with a stream cluster, the total over `source` alone (no `=~`) is the latency denominator
			: opts.streams && query.includes('envoy_cluster_upstream_rq') && !query.includes('=~') ? 'latencyTotal'
			: 'total';
		const prefix = query.includes('envoy_cluster_upstream_rq') ? 'http'
			: query.includes('job_handler') ? 'jobs' : 'nats';
		const data = series[`${prefix}.${key}`] ?? {};
		const values = Object.entries(data)
			.map(([h, v]) => [(Number(h) + HOUR_MS) / 1000, String(v)] as [number, string])
			.filter(([t]) => t * 1000 >= start && t * 1000 <= end);
		return new Response(JSON.stringify({ status: 'success', data: { resultType: 'matrix', result: values.length ? [{ metric: {}, values }] : [] } }));
	}) as unknown as typeof fetch;
	return { fetchFn, queries };
}

describe('collectSli', () => {
	it('upserts the last 3 completed hours for an SLI that already has rows, and rounds to integers', async () => {
		const store = new MemStore();
		store.seeded.add('core-api|production');
		const vm = stubVm({
			'http.total': { [HOUR('2026-10-07T09:00:00Z')]: 100.4, [HOUR('2026-10-07T10:00:00Z')]: 200.6, [HOUR('2026-10-07T11:00:00Z')]: 300 },
			'http.errors': { [HOUR('2026-10-07T10:00:00Z')]: 2.2 },
			'http.fast': { [HOUR('2026-10-07T09:00:00Z')]: 90, [HOUR('2026-10-07T10:00:00Z')]: 190, [HOUR('2026-10-07T11:00:00Z')]: 280 },
			'http.total.old': {},
		});
		const rows = await collectSli({ slis: [coreApi], cluster: 'tuju-production', vmUrl: 'http://vm', store, now: NOW, fetchFn: vm.fetchFn });
		expect(rows.map((r) => r.hourStart.toISOString())).toEqual(['2026-10-07T09:00:00.000Z', '2026-10-07T10:00:00.000Z', '2026-10-07T11:00:00.000Z']);
		expect(rows[0]).toMatchObject({ sliId: 'core-api', environment: 'production', total: 100, errors: 0, fast: 90 });
		expect(rows[1]).toMatchObject({ total: 201, errors: 2, fast: 190 });
		expect(vm.queries).toHaveLength(3); // total, errors, fast
	});

	it('an http SLI without a streamSource writes latencyTotal = total; jobs and nats write NULL', async () => {
		const store = new MemStore();
		store.seeded.add('core-api|production');
		store.seeded.add('core-jobs|production');
		const h = HOUR('2026-10-07T11:00:00Z');
		const vm = stubVm({ 'http.total': { [h]: 300.4 }, 'http.fast': { [h]: 280 }, 'jobs.total': { [h]: 10 } });
		const rows = await collectSli({ slis: [coreApi, coreJobs], cluster: 'tuju-production', vmUrl: 'http://vm', store, now: NOW, fetchFn: vm.fetchFn });
		expect(rows.find((r) => r.sliId === 'core-api')).toMatchObject({ total: 300, latencyTotal: 300, fast: 280 });
		expect(rows.find((r) => r.sliId === 'core-jobs')!.latencyTotal).toBeNull();
		expect(vm.queries).toHaveLength(5); // http 3 + jobs 2: no extra latencyTotal query without a streamSource
	});

	it('with a streamSource, total counts both clusters and latencyTotal counts the ordinary cluster only', async () => {
		const store = new MemStore();
		store.seeded.add('core-api|production');
		const h = HOUR('2026-10-07T11:00:00Z');
		const vm = stubVm({
			'http.total': { [h]: 1000 }, // ordinary + SSE
			'http.latencyTotal': { [h]: 900 }, // ordinary only
			'http.errors': { [h]: 4 },
			'http.fast': { [h]: 850 },
		}, { streams: true });
		const rows = await collectSli({ slis: [coreApiSse], cluster: 'tuju-production', vmUrl: 'http://vm', store, now: NOW, fetchFn: vm.fetchFn });
		expect(rows).toHaveLength(1);
		expect(rows[0]).toMatchObject({ total: 1000, errors: 4, fast: 850, latencyTotal: 900 });
		expect(vm.queries).toHaveLength(4); // total, errors, fast, latencyTotal
	});

	it('re-upserting the same hours is idempotent and absorbs a late scrape', async () => {
		const store = new MemStore();
		store.seeded.add('core-jobs|production');
		const first = stubVm({ 'jobs.total': { [HOUR('2026-10-07T11:00:00Z')]: 10 } });
		await collectSli({ slis: [coreJobs], cluster: 'tuju-production', vmUrl: 'http://vm', store, now: NOW, fetchFn: first.fetchFn });
		const late = stubVm({ 'jobs.total': { [HOUR('2026-10-07T11:00:00Z')]: 14 }, 'jobs.errors': { [HOUR('2026-10-07T11:00:00Z')]: 1 } });
		await collectSli({ slis: [coreJobs], cluster: 'tuju-production', vmUrl: 'http://vm', store, now: NOW, fetchFn: late.fetchFn });
		expect(store.rows.size).toBe(1);
		expect([...store.rows.values()][0]).toMatchObject({ total: 14, errors: 1, fast: null });
	});

	it('backfills 30 days against an empty table, 720 hours', async () => {
		const store = new MemStore();
		const series: Record<number, number> = {};
		for (let i = 1; i <= 720; i++) series[HOUR('2026-10-07T11:00:00Z') - (i - 1) * HOUR_MS] = i;
		const vm = stubVm({ 'http.total': series, 'http.errors': {}, 'http.fast': series });
		const rows = await collectSli({ slis: [coreApi], cluster: 'tuju-production', vmUrl: 'http://vm', store, now: NOW, fetchFn: vm.fetchFn });
		expect(rows).toHaveLength(720);
		expect(rows[0].hourStart.toISOString()).toBe('2026-09-07T12:00:00.000Z');
		expect(vm.queries).toHaveLength(3); // one query_range per series, not one per hour
	});

	it('writes no row for an absent series (core-nats before core ships the counter), never zeros', async () => {
		const store = new MemStore();
		const vm = stubVm({});
		const rows = await collectSli({ slis: [coreNats], cluster: 'tuju-production', vmUrl: 'http://vm', store, now: NOW, fetchFn: vm.fetchFn });
		expect(rows).toEqual([]);
		expect(store.upserts).toBe(0);
		expect(store.rows.size).toBe(0);
	});

	it('writes nothing when VictoriaMetrics fails, even for SLIs that came earlier in the run', async () => {
		const store = new MemStore();
		store.seeded.add('core-jobs|production');
		store.seeded.add('core-nats|production');
		let call = 0;
		const ok = stubVm({ 'jobs.total': { [HOUR('2026-10-07T11:00:00Z')]: 10 } });
		const fetchFn = (async (u: URL | string) => (++call > 2 ? new Response('down', { status: 502 }) : ok.fetchFn(u))) as unknown as typeof fetch;
		await expect(
			collectSli({ slis: [coreJobs, coreNats], cluster: 'tuju-production', vmUrl: 'http://vm', store, now: NOW, fetchFn })
		).rejects.toBeInstanceOf(VictoriaMetricsError);
		expect(store.upserts).toBe(0);
	});

	it('maps the cluster to the environment', async () => {
		const store = new MemStore();
		store.seeded.add('core-jobs|staging');
		const vm = stubVm({ 'jobs.total': { [HOUR('2026-10-07T11:00:00Z')]: 5 } });
		const rows = await collectSli({ slis: [coreJobs], cluster: 'tuju-staging', vmUrl: 'http://vm', store, now: NOW, fetchFn: vm.fetchFn });
		expect(rows[0].environment).toBe('staging');
		await expect(collectSli({ slis: [coreJobs], cluster: 'nope', vmUrl: 'http://vm', store, now: NOW, fetchFn: vm.fetchFn })).rejects.toThrow(/unknown SLI cluster/);
	});
});

describe('queriesFor (D6 table)', () => {
	it('http', () => {
		const q = queriesFor(coreApi, 'tuju-production');
		expect(q.total).toBe('sum(increase(envoy_cluster_upstream_rq_xx{cluster="tuju-production", envoy_cluster_name="api_core_80"}[1h]))');
		expect(q.errors).toContain('envoy_response_code_class="5"');
		expect(q.fast).toBe('sum(increase(envoy_cluster_upstream_rq_time_bucket{cluster="tuju-production", envoy_cluster_name="api_core_80", le="1000.0"}[1h]))');
	});
	it('http without a streamSource reads one cluster everywhere and has no separate latency denominator', () => {
		expect(queriesFor(coreApi, 'tuju-production').latencyTotal).toBeNull();
	});
	it('http with a streamSource: total and 5xx read both clusters, latency and its denominator read source only', () => {
		const q = queriesFor(coreApiSse, 'tuju-production');
		expect(q.total).toBe('sum(increase(envoy_cluster_upstream_rq_xx{cluster="tuju-production", envoy_cluster_name=~"api_core_80|api_core_80_sse"}[1h]))');
		expect(q.errors).toBe('sum(increase(envoy_cluster_upstream_rq_xx{cluster="tuju-production", envoy_cluster_name=~"api_core_80|api_core_80_sse", envoy_response_code_class="5"}[1h]))');
		expect(q.fast).toBe('sum(increase(envoy_cluster_upstream_rq_time_bucket{cluster="tuju-production", envoy_cluster_name="api_core_80", le="1000.0"}[1h]))');
		expect(q.latencyTotal).toBe('sum(increase(envoy_cluster_upstream_rq_xx{cluster="tuju-production", envoy_cluster_name="api_core_80"}[1h]))');
	});
	it('jobs and nats have no latency objective', () => {
		expect(queriesFor(coreJobs, 'c').fast).toBeNull();
		expect(queriesFor(coreNats, 'c').errors).toBe('sum(increase(daya_core_nats_handler_runs_total{cluster="c", status="error"}[1h]))');
	});
	it('nats total ignores rejected runs (a rejection is neither good nor bad availability)', () => {
		expect(queriesFor(coreNats, 'c').total).toBe('sum(increase(daya_core_nats_handler_runs_total{cluster="c", status=~"success|error"}[1h]))');
	});
});

describe('parseServiceSliConfig (D7)', () => {
	const ok = JSON.stringify([{ id: 'core-api', kind: 'http', project: 'core', source: 'api_core_80', availabilityTarget: 0.995, latency: { le: '1000.0', target: 0.94 } }]);
	it('accepts the starting set shape and defaults the window to 28 days', () => {
		expect(parseServiceSliConfig(ok)[0]).toMatchObject({ id: 'core-api', windowDays: 28 });
	});
	it('is empty when unset', () => expect(parseServiceSliConfig(undefined)).toEqual([]));
	it('rejects a latency le that is not an edge bucket', () => {
		const bad = JSON.stringify([{ id: 'x', kind: 'http', project: 'p', source: 's', latency: { le: '999.0', target: 0.9 } }]);
		expect(() => parseServiceSliConfig(bad)).toThrow(/edge histogram buckets/);
	});
	it('names every problem, rejects duplicates and bad kinds', () => {
		const bad = JSON.stringify([{ id: 'a', kind: 'grpc', project: 'p', source: 's' }, { id: 'a', kind: 'jobs', project: 'p', source: 's', availabilityTarget: 2 }]);
		expect(() => parseServiceSliConfig(bad)).toThrow(/kind must be one of[\s\S]*duplicated[\s\S]*availabilityTarget/);
	});
	it('accepts a streamSource on an http SLI and rejects one on other kinds, a bad name, or the same as source', () => {
		const base = { id: 'a', kind: 'http', project: 'p', source: 'api_core_80' };
		expect(parseServiceSliConfig(JSON.stringify([{ ...base, streamSource: 'api_core_80_sse' }]))[0].streamSource).toBe('api_core_80_sse');
		expect(parseServiceSliConfig(JSON.stringify([base]))[0].streamSource).toBeUndefined();
		expect(() => parseServiceSliConfig(JSON.stringify([{ ...base, kind: 'jobs', streamSource: 'x' }]))).toThrow(/only valid for kind http/);
		expect(() => parseServiceSliConfig(JSON.stringify([{ ...base, streamSource: 'a|b' }]))).toThrow(/not a valid Envoy cluster name/);
		expect(() => parseServiceSliConfig(JSON.stringify([{ ...base, streamSource: 'api_core_80' }]))).toThrow(/must differ from source/);
	});
	it('rejects non-JSON', () => expect(() => parseServiceSliConfig('{')).toThrow(/not valid JSON/));
});
